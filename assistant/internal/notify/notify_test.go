package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestNotifyReminderSendsOneHighPriorityNotification(t *testing.T) {
	server, requests := captureServer(t)
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL, RemindersKey: true}),
		Secrets: newFakeSecrets(),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.NotifyReminder(context.Background(), "check the oven")
	req := waitForRequest(t, requests)
	var payload publishRequest
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Title != "Reminder" || payload.Message != "check the oven" {
		t.Fatalf("title/message = %q / %q", payload.Title, payload.Message)
	}
	if payload.Priority == nil || *payload.Priority != 4 {
		t.Fatalf("priority = %v, want numeric 4 (high)", payload.Priority)
	}
	if len(payload.Tags) != 1 || payload.Tags[0] != "bell" {
		t.Fatalf("tags = %v", payload.Tags)
	}
	select {
	case extra := <-requests:
		t.Fatalf("one Reminder sent two requests: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotifyReminderObeysSwitchAndQuiet(t *testing.T) {
	server, requests := captureServer(t)
	secretSvc := newFakeSecrets()
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{URLKey: server.URL, RemindersKey: false}),
		Secrets: secretSvc,
		Quiet:   quietService(t),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The switch is off, so nothing is attempted even though Quiet is on.
	svc.NotifyReminder(context.Background(), "check the oven")
	select {
	case req := <-requests:
		t.Fatalf("a disabled Reminder Notification was sent: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}

	// Switch on, Quiet on: still no network and no retry when the window ends.
	svc.Bind(newFakeReader(map[string]any{URLKey: server.URL, RemindersKey: true}))
	svc.NotifyReminder(context.Background(), "check the oven")
	select {
	case req := <-requests:
		t.Fatalf("a quiet Reminder Notification was sent: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
	if secretSvc.count(TopicSecret) != 0 {
		t.Fatal("a quiet Reminder Notification provisioned a topic")
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

// countingTransport is the fake ntfy server for the batching tests. It counts
// publishes and keeps their decoded payloads, so "one push per alert" becomes a
// number instead of an inference.
type countingTransport struct {
	mu      sync.Mutex
	payload []publishRequest
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var payload publishRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.payload = append(t.payload, payload)
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"test"}`)),
		Request:    req,
	}, nil
}

func (t *countingTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.payload)
}

func (t *countingTransport) messages() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.payload))
	for _, payload := range t.payload {
		out = append(out, payload.Message)
	}
	return out
}

// syncBuffer collects log lines written from the recovery timer goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// batchFixture is a Notify service over a fake ntfy endpoint, a fake secret
// store and a captured Debug-level log.
type batchFixture struct {
	svc     *Service
	fake    *countingTransport
	secrets *fakeSecrets
	logs    *syncBuffer
	values  *fakeReader
}

func newBatchFixture(t *testing.T, values map[string]any, tweak func(*Options)) *batchFixture {
	t.Helper()
	if values == nil {
		values = map[string]any{}
	}
	values[URLKey] = "https://ntfy.test"
	if _, ok := values[MissedAlertsKey]; !ok {
		values[MissedAlertsKey] = true
	}
	fake := &countingTransport{}
	secrets := newFakeSecrets()
	logs := &syncBuffer{}
	reader := newFakeReader(values)
	opts := Options{
		Reader:  reader,
		Secrets: secrets,
		Logger:  slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Config:  DefaultConfig(),
		HTTPClient: &http.Client{
			Transport: fake,
		},
	}
	if tweak != nil {
		tweak(&opts)
	}
	svc, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A cooldown deferral arms a real timer; stop it on teardown so a test's
	// frozen clock cannot leave a re-arming goroutine behind.
	t.Cleanup(func() {
		svc.batchMu.Lock()
		if svc.pendingTimer != nil {
			svc.pendingTimer.Stop()
			svc.pendingTimer = nil
		}
		svc.batchMu.Unlock()
	})
	return &batchFixture{svc: svc, fake: fake, secrets: secrets, logs: logs, values: reader}
}

// waitForCount waits until the fake endpoint has seen want publishes.
func (f *batchFixture) waitForCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.fake.count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("publishes = %d, want %d: %v", f.fake.count(), want, f.fake.messages())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertCount pins the publish count after a short settle, so a stray extra
// push is caught instead of racing the assertion.
func (f *batchFixture) assertCount(t *testing.T, want int) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if got := f.fake.count(); got != want {
		t.Fatalf("publishes = %d, want %d: %v", got, want, f.fake.messages())
	}
}

func recoveryAlerts() []MissedAlert {
	return []MissedAlert{
		{Kind: "timer", Label: "pasta", Reason: "restart", Duration: 5 * time.Minute},
		{Kind: "alarm", Label: "7am", Reason: "downtime"},
		{Kind: "timer", Label: "washing machine", Reason: "downtime", Duration: 1 * time.Hour},
	}
}

func TestRecoveryBurstPushesOneSummaryNamingEveryAlert(t *testing.T) {
	// A whole recovery pass lands inside one hold, so three missed alerts are
	// one push. The test flushes explicitly instead of sleeping the hold.
	fixture := newBatchFixture(t, nil, func(opts *Options) { opts.RecoveryHold = time.Hour })
	ctx := context.Background()
	for _, alert := range recoveryAlerts() {
		fixture.svc.NotifyMissed(ctx, alert)
	}
	if got := fixture.fake.count(); got != 0 {
		t.Fatalf("a held recovery alert pushed %d requests before the summary", got)
	}
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	fixture.assertCount(t, 1)
	messages := fixture.fake.messages()
	want := "You missed 3 alerts: pasta timer, 7am alarm, washing machine timer."
	if messages[0] != want {
		t.Fatalf("message = %q, want %q", messages[0], want)
	}
	payload := fixture.fake.payload[0]
	if payload.Title != "Missed alerts" {
		t.Fatalf("title = %q", payload.Title)
	}
	if payload.Priority == nil || *payload.Priority != 4 {
		t.Fatalf("priority = %v, want numeric 4 (high)", payload.Priority)
	}
	if len(payload.Tags) != 1 || payload.Tags[0] != "warning" {
		t.Fatalf("tags = %v", payload.Tags)
	}
}

func TestRecoveryBurstFlushesOnTheHoldWithoutACall(t *testing.T) {
	fixture := newBatchFixture(t, nil, func(opts *Options) { opts.RecoveryHold = 25 * time.Millisecond })
	ctx := context.Background()
	for _, alert := range recoveryAlerts() {
		fixture.svc.NotifyMissed(ctx, alert)
	}
	fixture.waitForCount(t, 1)
	fixture.assertCount(t, 1)
	if message := fixture.fake.messages()[0]; !strings.Contains(message, "You missed 3 alerts") {
		t.Fatalf("message = %q", message)
	}
}

func TestRecoveryBurstIsOneSummaryUnderConcurrency(t *testing.T) {
	fixture := newBatchFixture(t, nil, func(opts *Options) { opts.RecoveryHold = 25 * time.Millisecond })
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "alarm", Reason: "downtime"})
		}()
	}
	wg.Wait()
	fixture.waitForCount(t, 1)
	fixture.assertCount(t, 1)
	if message := fixture.fake.messages()[0]; !strings.Contains(message, "You missed 40 alerts") {
		t.Fatalf("message = %q, want the whole burst counted once", message)
	}
}

func TestLiveMissedAlertPushesImmediately(t *testing.T) {
	// A Ring that just reached max_ring is not a burst: it must not wait for a
	// hold that only exists to collect a recovery pass.
	fixture := newBatchFixture(t, nil, func(opts *Options) { opts.RecoveryHold = time.Hour })
	fixture.svc.NotifyMissed(context.Background(), MissedAlert{
		Kind: "timer", Label: "pasta", Reason: "max_ring", Duration: 5 * time.Minute,
	})
	fixture.waitForCount(t, 1)
	if message := fixture.fake.messages()[0]; !strings.Contains(message, "pasta") {
		t.Fatalf("message = %q", message)
	}
}

func TestMissedCooldownDefersTheSecondSummaryUntilTheGapEnds(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	fixture := newBatchFixture(t, map[string]any{
		CooldownKey: 60 * time.Second,
	}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
		opts.Now = func() time.Time { return now }
	})
	ctx := context.Background()
	for _, alert := range recoveryAlerts() {
		fixture.svc.NotifyMissed(ctx, alert)
	}
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("first flushPending: %v", err)
	}
	fixture.waitForCount(t, 1)

	// A second burst ten seconds later is inside notify.cooldown. It is kept
	// pending and re-armed for the remainder of the gap: the user already has a
	// push, but the recovered alert must not be lost.
	now = now.Add(10 * time.Second)
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "alarm", Label: "wash car", Reason: "downtime"})
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("second flushPending: %v", err)
	}
	fixture.assertCount(t, 1)
	if logs := fixture.logs.String(); !strings.Contains(logs, "deferred by cooldown") {
		t.Fatalf("the deferral was not logged at Debug: %s", logs)
	}
	fixture.svc.batchMu.Lock()
	remaining := len(fixture.svc.pending)
	fixture.svc.batchMu.Unlock()
	if remaining != 1 {
		t.Fatalf("the cooldown-blocked alert was not kept pending: %d", remaining)
	}

	// Once the gap ends the held alert reaches the endpoint: one extra publish,
	// not zero and not a merge that loses the label.
	now = now.Add(51 * time.Second)
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("flushPending after the gap: %v", err)
	}
	fixture.waitForCount(t, 2)
	fixture.assertCount(t, 2)
	if message := fixture.fake.messages()[1]; !strings.Contains(message, "wash car") {
		t.Fatalf("second summary = %q, want it to name the deferred alert", message)
	}
}

func TestMissedCooldownExpiresForTheNextSummary(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	fixture := newBatchFixture(t, map[string]any{
		CooldownKey: 60 * time.Second,
	}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
		opts.Now = func() time.Time { return now }
	})
	ctx := context.Background()
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "timer", Label: "pasta", Reason: "restart"})
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("first flushPending: %v", err)
	}
	fixture.waitForCount(t, 1)
	now = now.Add(61 * time.Second)
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "alarm", Label: "7am", Reason: "downtime"})
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("second flushPending: %v", err)
	}
	fixture.waitForCount(t, 2)
}

func TestMissedCooldownZeroNeverSkips(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	fixture := newBatchFixture(t, map[string]any{CooldownKey: time.Duration(0)}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
		opts.Now = func() time.Time { return now }
	})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "timer", Label: "pasta", Reason: "restart"})
		if err := fixture.svc.flushPending(ctx); err != nil {
			t.Fatalf("flushPending %d: %v", i, err)
		}
	}
	fixture.waitForCount(t, 2)
}

// TestCloseDrainsHeldBatchExactlyOnce covers the shutdown-drain acceptance
// criterion: a held batch is published once by Close, not zero times and not
// twice. Close is also idempotent and safe on a nil service.
func TestCloseDrainsHeldBatchExactlyOnce(t *testing.T) {
	fixture := newBatchFixture(t, nil, func(opts *Options) { opts.RecoveryHold = time.Hour })
	ctx := context.Background()
	for _, alert := range recoveryAlerts() {
		fixture.svc.NotifyMissed(ctx, alert)
	}
	if got := fixture.fake.count(); got != 0 {
		t.Fatalf("a held recovery alert pushed %d requests before Close", got)
	}
	if err := fixture.svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fixture.assertCount(t, 1)
	if got := fixture.fake.messages()[0]; got != "You missed 3 alerts: pasta timer, 7am alarm, washing machine timer." {
		t.Fatalf("drained summary = %q", got)
	}
	// A second Close must not republish the same batch.
	if err := fixture.svc.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	fixture.assertCount(t, 1)
	var nilSvc *Service
	if err := nilSvc.Close(ctx); err != nil {
		t.Fatalf("Close on a nil service: %v", err)
	}
}

// TestCloseFlushesInsideCooldown proves the final drain cannot lose an alert
// merely because a previous summary was recent: there is no later flush to
// wait for, so Close publishes even inside notify.cooldown.
func TestCloseFlushesInsideCooldown(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	fixture := newBatchFixture(t, map[string]any{CooldownKey: 60 * time.Second}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
		opts.Now = func() time.Time { return now }
	})
	ctx := context.Background()
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "timer", Label: "pasta", Reason: "restart"})
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("first flushPending: %v", err)
	}
	fixture.waitForCount(t, 1)

	// A second batch immediately after is inside the gap; at shutdown Close
	// must still deliver it.
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "alarm", Label: "wash car", Reason: "downtime"})
	if err := fixture.svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fixture.assertCount(t, 2)
	if got := fixture.fake.messages()[1]; !strings.Contains(got, "wash car") {
		t.Fatalf("drained summary = %q, want the held alert", got)
	}
}

// TestRecoveryPassLongerThanHoldIsOneDebouncedSummary covers the acceptance
// criterion for a pass whose inter-arrival gap is smaller than the hold but
// whose total length exceeds it: every alert lands in one summary.
func TestRecoveryPassLongerThanHoldIsOneDebouncedSummary(t *testing.T) {
	const (
		hold = 40 * time.Millisecond
		gap  = 12 * time.Millisecond
	)
	fixture := newBatchFixture(t, map[string]any{CooldownKey: time.Duration(0)}, func(opts *Options) {
		opts.RecoveryHold = hold
	})
	ctx := context.Background()
	labels := []string{"pasta", "oven", "washing machine", "bins", "kettle"}
	for _, label := range labels {
		fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "timer", Label: label, Reason: "restart"})
		time.Sleep(gap)
	}
	// Total pass length (5 * 12ms = 60ms) exceeds the 40ms hold, but every
	// arrival re-arms it, so a single flush happens after the last alert.
	fixture.waitForCount(t, 1)
	fixture.assertCount(t, 1)
	message := fixture.fake.messages()[0]
	if !strings.Contains(message, "You missed 5 alerts") {
		t.Fatalf("message = %q, want whole burst counted once", message)
	}
	for _, label := range labels {
		if !strings.Contains(message, label) {
			t.Fatalf("message = %q, missing %q", message, label)
		}
	}
}

// TestAlertAfterFirstFlushIsPublishedAfterCooldown covers the acceptance
// criterion for alerts arriving after the first flush: the cooldown defers
// them but they still reach the endpoint in exactly one extra publish.
func TestAlertAfterFirstFlushIsPublishedAfterCooldown(t *testing.T) {
	fixture := newBatchFixture(t, map[string]any{CooldownKey: 40 * time.Millisecond}, func(opts *Options) {
		opts.RecoveryHold = 5 * time.Millisecond
	})
	ctx := context.Background()
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "timer", Label: "pasta", Reason: "restart"})
	fixture.waitForCount(t, 1)

	// This alert lands after the first push but well inside the cooldown. The
	// old code dropped it; it must now arrive as one extra publish.
	fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "alarm", Label: "wash car", Reason: "downtime"})
	fixture.waitForCount(t, 2)
	fixture.assertCount(t, 2)
	joined := strings.Join(fixture.fake.messages(), " | ")
	if !strings.Contains(joined, "pasta") || !strings.Contains(joined, "wash car") {
		t.Fatalf("combined summaries = %q, want both alerts", joined)
	}
}

// TestCooldownShorterThanBurstCoversEveryAlert covers the acceptance criterion
// for a cooldown shorter than a burst: whatever the number of summaries, the
// combined names cover every alert and never a subset.
func TestCooldownShorterThanBurstCoversEveryAlert(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	const cooldown = 15 * time.Second
	fixture := newBatchFixture(t, map[string]any{CooldownKey: cooldown}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
		opts.Now = func() time.Time { return now }
	})
	ctx := context.Background()
	labels := []string{"pasta", "oven", "bins"}
	for _, label := range labels {
		fixture.svc.NotifyMissed(ctx, MissedAlert{Kind: "timer", Label: label, Reason: "restart"})
		if err := fixture.svc.flushPending(ctx); err != nil {
			t.Fatalf("flushPending: %v", err)
		}
		// Each arrival is inside the next cooldown window, so at least one
		// batch is deferred instead of published immediately.
		now = now.Add(cooldown / 2)
	}
	// Drive any batch the cooldown left pending, advancing the fake clock past
	// the gap so it is published rather than re-deferred.
	for i := 0; i <= len(labels); i++ {
		fixture.svc.batchMu.Lock()
		empty := len(fixture.svc.pending) == 0
		fixture.svc.batchMu.Unlock()
		if empty {
			break
		}
		now = now.Add(cooldown)
		if err := fixture.svc.flushPending(ctx); err != nil {
			t.Fatalf("drain flushPending: %v", err)
		}
	}
	fixture.assertCount(t, 2)
	joined := strings.Join(fixture.fake.messages(), " | ")
	for _, label := range labels {
		if !strings.Contains(joined, label) {
			t.Fatalf("combined summaries = %q, missing %q", joined, label)
		}
	}
}

func TestRecoveryBurstObeysQuietAndTheSwitch(t *testing.T) {
	// Quiet hours still drop the burst before any network request or topic.
	fixture := newBatchFixture(t, map[string]any{MissedAlertsKey: true}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
		opts.Quiet = quietService(t)
	})
	ctx := context.Background()
	for _, alert := range recoveryAlerts() {
		fixture.svc.NotifyMissed(ctx, alert)
	}
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	fixture.assertCount(t, 0)
	if fixture.secrets.count(TopicSecret) != 0 {
		t.Fatal("a quiet recovery burst provisioned a topic")
	}

	// The missed-alert switch off: nothing is even held.
	switchedOff := newBatchFixture(t, map[string]any{MissedAlertsKey: false}, func(opts *Options) {
		opts.RecoveryHold = time.Hour
	})
	for _, alert := range recoveryAlerts() {
		switchedOff.svc.NotifyMissed(ctx, alert)
	}
	if err := switchedOff.svc.flushPending(ctx); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	switchedOff.assertCount(t, 0)
}

func TestRemindersAreNeverBatchedOrCooldownSuppressed(t *testing.T) {
	// A due Reminder is user content the user asked for: each one is one
	// request, even inside notify.cooldown, and it never joins a summary.
	fixture := newBatchFixture(t, map[string]any{
		RemindersKey: true,
		CooldownKey:  10 * time.Minute,
	}, func(opts *Options) { opts.RecoveryHold = time.Hour })
	ctx := context.Background()
	for _, message := range []string{"check the oven", "call mum", "take the bins out"} {
		fixture.svc.NotifyReminder(ctx, message)
	}
	fixture.waitForCount(t, 3)
	fixture.assertCount(t, 3)
	for i, want := range []string{"check the oven", "call mum", "take the bins out"} {
		if got := fixture.fake.messages()[i]; got != want {
			t.Fatalf("message %d = %q, want %q", i, got, want)
		}
	}
}

func TestMissedSummaryNamesStayOutOfTheLog(t *testing.T) {
	// A label is private text. The summary carries it to the phone, which is the
	// existing egress, but it never reaches a log line.
	fixture := newBatchFixture(t, nil, func(opts *Options) { opts.RecoveryHold = time.Hour })
	ctx := context.Background()
	for _, alert := range recoveryAlerts() {
		fixture.svc.NotifyMissed(ctx, alert)
	}
	if err := fixture.svc.flushPending(ctx); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	fixture.waitForCount(t, 1)
	logs := fixture.logs.String()
	if logs == "" {
		t.Fatal("the batching path logged nothing; the Debug seam is not wired")
	}
	for _, label := range []string{"pasta", "7am", "washing machine"} {
		if strings.Contains(logs, label) {
			t.Fatalf("private label %q reached the log: %s", label, logs)
		}
	}
	if strings.Contains(logs, fixture.fake.payload[0].Topic) {
		t.Fatal("the topic reached the log")
	}
}

func TestMissedSummaryRendering(t *testing.T) {
	single := missedSummaryNotification([]MissedAlert{
		{Kind: "timer", Label: "pasta", Reason: "downtime", Duration: 5 * time.Minute},
	})
	wantSingle := missedNotification(MissedAlert{
		Kind: "timer", Label: "pasta", Reason: "downtime", Duration: 5 * time.Minute,
	})
	if single.Message != wantSingle.Message || single.Title != wantSingle.Title {
		t.Fatalf("one alert = %q/%q, want the detailed single wording %q/%q",
			single.Title, single.Message, wantSingle.Title, wantSingle.Message)
	}

	batch := missedSummaryNotification([]MissedAlert{
		{Kind: "timer", Label: "pasta"},
		{Kind: "alarm", Label: "7am"},
		{Kind: "timer"},
	})
	if batch.Title != "Missed alerts" || batch.Kind != "missed" {
		t.Fatalf("title/kind = %q/%q", batch.Title, batch.Kind)
	}
	if want := "You missed 3 alerts: pasta timer, 7am alarm, timer."; batch.Message != want {
		t.Fatalf("message = %q, want %q", batch.Message, want)
	}

	// A long burst keeps the exact count but bounds the name list.
	many := make([]MissedAlert, 0, maxSummaryNames+3)
	for i := 0; i < maxSummaryNames+3; i++ {
		many = append(many, MissedAlert{Kind: "timer", Label: fmt.Sprintf("t%d", i)})
	}
	long := missedSummaryNotification(many)
	if want := "You missed 9 alerts: t0 timer, t1 timer, t2 timer, t3 timer, t4 timer, t5 timer and 3 more."; long.Message != want {
		t.Fatalf("long message = %q, want %q", long.Message, want)
	}
	if long.Kind != "missed_timer" {
		t.Fatalf("kind = %q, want the one kind it covers", long.Kind)
	}

	// A label is user text: it cannot add a line to the summary.
	flattened := missedSummaryNotification([]MissedAlert{
		{Kind: "timer", Label: "pasta\n5m"},
		{Kind: "timer", Label: "bins"},
	})
	if want := "You missed 2 alerts: pasta 5m timer, bins timer."; flattened.Message != want {
		t.Fatalf("flattened message = %q, want %q", flattened.Message, want)
	}
}

func TestCooldownSettingIsLiveAndValidated(t *testing.T) {
	defs := Definitions(DefaultConfig())
	found := false
	for _, def := range defs {
		if def.Key != CooldownKey {
			continue
		}
		found = true
		if def.Kind != settings.Live {
			t.Fatalf("cooldown kind = %v, want a live setting", def.Kind)
		}
		if got, ok := def.Default.(time.Duration); !ok || got != DefaultCooldown {
			t.Fatalf("cooldown default = %v, want %s", def.Default, DefaultCooldown)
		}
	}
	if !found {
		t.Fatalf("missing setting %q", CooldownKey)
	}
	if DefaultCooldown != time.Minute {
		t.Fatalf("DefaultCooldown = %s, want 60s", DefaultCooldown)
	}
	if err := validCooldown(-time.Second); err == nil {
		t.Fatal("validCooldown accepted a negative gap")
	}
	if err := validCooldown("60s"); err == nil {
		t.Fatal("validCooldown accepted a string")
	}
	if err := validCooldown(DefaultCooldown); err != nil {
		t.Fatalf("validCooldown(default): %v", err)
	}

	// A wrong-typed stored value falls back to the shipped default instead of
	// silencing the phone.
	svc, err := New(Options{
		Reader:  newFakeReader(map[string]any{CooldownKey: "not a duration"}),
		Secrets: newFakeSecrets(),
		Config:  DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := svc.cooldown(); got != DefaultCooldown {
		t.Fatalf("cooldown() = %s, want the default %s", got, DefaultCooldown)
	}
}
