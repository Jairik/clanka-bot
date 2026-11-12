package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// fakeReader is a live-settings stand-in for the notify tests.
type fakeReader struct {
	mu     sync.RWMutex
	values map[string]settings.Value
}

func newFakeReader(values map[string]any) *fakeReader {
	reader := &fakeReader{values: make(map[string]settings.Value, len(values))}
	reader.set(values)
	return reader
}

func (r *fakeReader) set(values map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range values {
		r.values[key] = settings.Value{Key: key, Value: value}
	}
}

func (r *fakeReader) Get(key string) (settings.Value, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.values[key]
	return value, ok
}

// fakeSecrets is the write-only store stand-in. It records only what a caller
// could see through the real service: Get and Set.
type fakeSecrets struct {
	mu     sync.Mutex
	values map[string]string
	sets   int
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{values: make(map[string]string)} }

func (s *fakeSecrets) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[name]
	return value, ok
}

func (s *fakeSecrets) Set(_ context.Context, name, value string) (secrets.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	s.values[name] = strings.TrimSpace(value)
	return secrets.Metadata{Name: name, Set: true, Source: secrets.SourceStore}, nil
}

func (s *fakeSecrets) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[name]; ok {
		return 1
	}
	return 0
}

// quietService builds the real Quiet service over a window covering noon.
func quietService(t *testing.T) *quiet.Service {
	t.Helper()
	reader := newFakeReader(map[string]any{
		quiet.EnabledKey: true,
		quiet.StartKey:   "00:00",
		quiet.EndKey:     "23:59",
		quiet.VolumeKey:  35,
	})
	svc := quiet.New(quiet.Options{
		Reader: reader,
		Now:    func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
	if !svc.IsQuiet() {
		t.Fatal("test Quiet service is not quiet at noon")
	}
	return svc
}

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

func captureServer(t *testing.T) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"test","event":"message"}`)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func waitForRequest(t *testing.T, requests <-chan capturedRequest) capturedRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("the fake ntfy server received no request")
		return capturedRequest{}
	}
}

func TestTopicIsGeneratedOncePersistedAndConcurrent(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/data/assistant.db"
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	secretSvc, err := secrets.New(ctx, st.DB(), []string{TopicSecret, TokenSecret})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	svc, err := New(Options{Secrets: secretSvc, Config: DefaultConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const callers = 16
	topics := make(chan string, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			topic, err := svc.Topic(ctx)
			if err != nil {
				t.Errorf("Topic: %v", err)
				return
			}
			topics <- topic
		}()
	}
	wg.Wait()
	close(topics)
	var first string
	for topic := range topics {
		if first == "" {
			first = topic
			continue
		}
		if topic != first {
			t.Fatalf("two topics were generated: %q and %q", first, topic)
		}
	}
	if first == "" || !validTopic(first) {
		t.Fatalf("generated topic = %q, want a valid ntfy topic", first)
	}
	// The secret is durable: a fresh service over the same database resolves
	// the same topic without generating a new one.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	reopened, err := secrets.New(ctx, st.DB(), []string{TopicSecret, TokenSecret})
	if err != nil {
		t.Fatalf("reopen secrets: %v", err)
	}
	svc, err = New(Options{Secrets: reopened, Config: DefaultConfig()})
	if err != nil {
		t.Fatalf("New after reopen: %v", err)
	}
	topic, err := svc.Topic(ctx)
	if err != nil {
		t.Fatalf("Topic after reopen: %v", err)
	}
	if topic != first {
		t.Fatalf("topic changed across restart: %q -> %q", first, topic)
	}
	if _, ok := reopened.Get(TokenSecret); ok {
		t.Fatal("a token was generated; ntfy tokens must be server-issued")
	}
}

func TestInvalidStoredTopicIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	secretSvc := newFakeSecrets()
	if _, err := secretSvc.Set(ctx, TopicSecret, "not a valid topic!"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	svc, err := New(Options{Secrets: secretSvc, Config: DefaultConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := svc.Topic(ctx); err == nil {
		t.Fatal("Topic accepted an invalid stored topic")
	}
	if secretSvc.sets != 1 {
		t.Fatalf("sets = %d, want the invalid value left untouched", secretSvc.sets)
	}
}

func TestPublishPayloadAndServerIssuedToken(t *testing.T) {
	server, requests := captureServer(t)
	secretSvc := newFakeSecrets()
	if _, err := secretSvc.Set(context.Background(), TokenSecret, "tk_server-issued-token"); err != nil {
		t.Fatalf("Set token: %v", err)
	}
	reader := newFakeReader(map[string]any{URLKey: server.URL})
	var logs bytes.Buffer
	svc, err := New(Options{
		Reader:  reader,
		Secrets: secretSvc,
		Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	message := "Hello \"phone\"\nsecond line 😀"
	if err := svc.Send(context.Background(), Notification{
		Kind: "phone", Title: "Assistant", Message: message,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	req := waitForRequest(t, requests)
	if req.Method != http.MethodPost || req.Path != "/" {
		t.Fatalf("request = %s %s, want POST /", req.Method, req.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tk_server-issued-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var payload publishRequest
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, req.Body)
	}
	if payload.Topic == "" || !validTopic(payload.Topic) {
		t.Fatalf("payload topic = %q", payload.Topic)
	}
	if payload.Message != message {
		t.Fatalf("payload message = %q, want %q", payload.Message, message)
	}
	if payload.Title != "Assistant" {
		t.Fatalf("payload title = %q", payload.Title)
	}
	if strings.Contains(logs.String(), "tk_server-issued-token") {
		t.Fatalf("the access token reached the log: %s", logs.String())
	}
	if strings.Contains(logs.String(), payload.Topic) {
		t.Fatalf("the topic reached the log: %s", logs.String())
	}
}

func TestNoAuthorizationWhenNoTokenIsStored(t *testing.T) {
	server, requests := captureServer(t)
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL}),
		Secrets: newFakeSecrets(),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Send(context.Background(), Notification{Kind: "phone", Message: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	req := waitForRequest(t, requests)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("anonymous ntfy.sh publish sent Authorization %q", got)
	}
}

func TestURLSettingAppliesToTheNextSend(t *testing.T) {
	first, firstRequests := captureServer(t)
	second, secondRequests := captureServer(t)
	reader := newFakeReader(map[string]any{URLKey: first.URL})
	svc, err := New(Options{Reader: reader, Secrets: newFakeSecrets(), Config: DefaultConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Send(context.Background(), Notification{Kind: "phone", Message: "one"}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	waitForRequest(t, firstRequests)
	// A live settings write takes effect on the next send without a restart.
	reader.set(map[string]any{URLKey: second.URL})
	if err := svc.Send(context.Background(), Notification{Kind: "phone", Message: "two"}); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	waitForRequest(t, secondRequests)
	select {
	case req := <-firstRequests:
		t.Fatalf("the updated URL still used the old server: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestQuietMutesBeforeNetworkAndTopicProvisioning(t *testing.T) {
	server, requests := captureServer(t)
	secretSvc := newFakeSecrets()
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL}),
		Secrets: secretSvc,
		Quiet:   quietService(t),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = svc.Send(context.Background(), Notification{Kind: "phone", Message: "should not leave"})
	var muted *MutedError
	if !errors.As(err, &muted) {
		t.Fatalf("Send error = %v, want a MutedError", err)
	}
	if muted.Reason != "quiet hours" {
		t.Fatalf("muted reason = %q", muted.Reason)
	}
	select {
	case req := <-requests:
		t.Fatalf("a muted Notification reached the network: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
	if secretSvc.count(TopicSecret) != 0 {
		t.Fatal("Quiet suppression provisioned a topic")
	}
}

func TestRequestContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the request so the server can detect the client disconnect and
		// cancel the request context promptly.
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL}),
		Secrets: newFakeSecrets(),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- svc.Send(ctx, Notification{Kind: "phone", Message: "cancel me"})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send ignored the cancelled request context")
	}
}

func TestPublishTimeoutIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()
	svc, err := New(Options{
		Reader:     newFakeReader(map[string]any{URLKey: server.URL}),
		Secrets:    newFakeSecrets(),
		HTTPClient: &http.Client{Timeout: 20 * time.Millisecond},
		Config:     DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	started := time.Now()
	err = svc.Send(context.Background(), Notification{Kind: "phone", Message: "slow"})
	if err == nil {
		t.Fatal("Send succeeded against a server that never answered")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Send took %s; the client timeout was not bounded", elapsed)
	}
}

func TestURLValidationRejectsCredentialsAndQueries(t *testing.T) {
	server, requests := captureServer(t)
	for _, raw := range []string{
		"ftp://ntfy.example",
		"ntfy.example/topic",
		"https://user:pass@ntfy.example",
		"https://ntfy.example/?auth=secret",
		"https://ntfy.example/#fragment",
	} {
		t.Run(raw, func(t *testing.T) {
			svc, err := New(Options{
				Reader:  newFakeReader(map[string]any{URLKey: raw}),
				Secrets: newFakeSecrets(),
				Config:  DefaultConfig(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := svc.Send(context.Background(), Notification{Kind: "phone", Message: "x"}); err == nil {
				t.Fatalf("Send accepted URL %q", raw)
			}
			select {
			case req := <-requests:
				t.Fatalf("invalid URL %q reached the network: %+v", raw, req)
			default:
			}
		})
	}
	server.Close()
}

func TestRedirectsAreNotFollowedAndTokenStaysOnTheConfiguredHost(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	secretSvc := newFakeSecrets()
	if _, err := secretSvc.Set(context.Background(), TokenSecret, "tk_redirect-secret"); err != nil {
		t.Fatalf("Set token: %v", err)
	}
	var logs bytes.Buffer
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: source.URL}),
		Secrets: secretSvc,
		Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = svc.Send(context.Background(), Notification{Kind: "phone", Message: "redirect"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusFound {
		t.Fatalf("Send error = %v, want HTTP 302", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("the client followed the redirect")
	}
	if strings.Contains(err.Error(), "tk_redirect-secret") || strings.Contains(logs.String(), "tk_redirect-secret") {
		t.Fatal("the token appeared in an error or log")
	}
}

func TestHTTPErrorDoesNotLeakTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer server.Close()
	secretSvc := newFakeSecrets()
	if _, err := secretSvc.Set(context.Background(), TokenSecret, "tk_unauthorized"); err != nil {
		t.Fatalf("Set token: %v", err)
	}
	var logs bytes.Buffer
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL}),
		Secrets: secretSvc,
		Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = svc.Send(context.Background(), Notification{Kind: "phone", Message: "nope"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Send error = %v, want HTTP 401", err)
	}
	if strings.Contains(err.Error(), "tk_unauthorized") || strings.Contains(logs.String(), "tk_unauthorized") {
		t.Fatal("the token appeared in an error or log")
	}
}

func TestNotifyMissedSendsOneHighPriorityNotification(t *testing.T) {
	server, requests := captureServer(t)
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL, MissedAlertsKey: true}),
		Secrets: newFakeSecrets(),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.NotifyMissed(context.Background(), MissedAlert{
		Kind:     "timer",
		Label:    "pasta",
		Reason:   "max_ring",
		Duration: 5 * time.Minute,
	})
	req := waitForRequest(t, requests)
	var payload publishRequest
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Title != "Missed timer" {
		t.Fatalf("title = %q", payload.Title)
	}
	if !strings.Contains(payload.Message, "pasta") || !strings.Contains(payload.Message, "5m0s") {
		t.Fatalf("message = %q", payload.Message)
	}
	if payload.Priority == nil || *payload.Priority != 4 {
		t.Fatalf("priority = %v, want numeric 4 (high)", payload.Priority)
	}
	if len(payload.Tags) != 1 || payload.Tags[0] != "warning" {
		t.Fatalf("tags = %v", payload.Tags)
	}
	select {
	case extra := <-requests:
		t.Fatalf("one missed alert sent two requests: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotifyMissedObeysSwitchAndQuiet(t *testing.T) {
	server, requests := captureServer(t)
	secretSvc := newFakeSecrets()
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL, MissedAlertsKey: false}),
		Secrets: secretSvc,
		Quiet:   quietService(t),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The switch is off, so nothing is attempted even though Quiet is on.
	svc.NotifyMissed(context.Background(), MissedAlert{Kind: "alarm", Reason: "max_ring"})
	select {
	case req := <-requests:
		t.Fatalf("a disabled missed Notification was sent: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}

	// Switch on, Quiet on: still no network and no retry when the window ends.
	svc.Bind(newFakeReader(map[string]any{URLKey: server.URL, MissedAlertsKey: true}))
	svc.NotifyMissed(context.Background(), MissedAlert{Kind: "alarm", Reason: "max_ring"})
	select {
	case req := <-requests:
		t.Fatalf("a quiet missed Notification was sent: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
	if secretSvc.count(TopicSecret) != 0 {
		t.Fatal("a quiet missed Notification provisioned a topic")
	}
}

func TestMissedNotificationReasons(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"restart", "still ringing when the assistant restarted"},
		{"downtime", "missed while the assistant was offline"},
		{"max_ring", "missed after 5m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			got := missedNotification(MissedAlert{
				Kind: "timer", Label: "pasta", Reason: tc.reason, Duration: 5 * time.Minute,
			})
			if !strings.Contains(got.Message, tc.want) {
				t.Fatalf("message = %q, want %q", got.Message, tc.want)
			}
		})
	}
}

func TestNewRejectsAnInvalidConfigURL(t *testing.T) {
	if _, err := New(Options{Config: Config{URL: "ftp://ntfy.example"}}); err == nil {
		t.Fatal("New accepted an ftp URL")
	}
}

func TestNormalizeBaseURLAcceptsAnUppercaseScheme(t *testing.T) {
	got, err := normalizeBaseURL("HTTPS://ntfy.example/")
	if err != nil {
		t.Fatalf("normalizeBaseURL: %v", err)
	}
	if got != "https://ntfy.example" {
		t.Fatalf("normalized URL = %q", got)
	}
}

func TestDefinitionsNeverExposeSecretNames(t *testing.T) {
	defs := Definitions(DefaultConfig())
	seen := make(map[string]bool, len(defs))
	for _, def := range defs {
		seen[def.Key] = true
		if def.Key == TopicSecret || def.Key == TokenSecret {
			t.Fatalf("secret %q became a settings definition", def.Key)
		}
	}
	for _, key := range []string{URLKey, MissedAlertsKey} {
		if !seen[key] {
			t.Fatalf("missing setting %q", key)
		}
	}
}

func TestPriorityUsesTheNumericJSONForm(t *testing.T) {
	cases := map[string]int{"min": 1, "low": 2, "default": 3, "high": 4, "max": 5, "urgent": 5, "5": 5}
	for raw, want := range cases {
		got, err := priorityValue(raw)
		if err != nil || got == nil || *got != want {
			t.Fatalf("priorityValue(%q) = %v, %v; want %d", raw, got, err, want)
		}
	}
	if got, err := priorityValue(""); err != nil || got != nil {
		t.Fatalf("empty priority = %v, %v; want omitted", got, err)
	}
	if _, err := priorityValue("loud"); err == nil {
		t.Fatal("priorityValue accepted an unknown name")
	}
	if _, err := priorityValue("6"); err == nil {
		t.Fatal("priorityValue accepted out-of-range number 6")
	}
}
