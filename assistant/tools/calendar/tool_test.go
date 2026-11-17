package calendar

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"gopkg.in/yaml.v3"
)

type memorySecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *memorySecrets) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	val, ok := s.values[name]
	return val, ok
}

func (s *memorySecrets) Set(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[name] = value
}

// captureHandler records log records for testing assertions.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(name string) slog.Handler       { return h }

func (h *captureHandler) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var buf strings.Builder
	for _, r := range h.records {
		buf.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			buf.WriteString(" " + a.Key + "=" + fmt.Sprint(a.Value))
			return true
		})
		buf.WriteString("\n")
	}
	return buf.String()
}

const sampleICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//EN
BEGIN:VEVENT
UID:event-1@example.com
DTSTAMP:20261116T080000Z
DTSTART:20261116T093000Z
DTEND:20261116T100000Z
SUMMARY:stand-up
END:VEVENT
BEGIN:VEVENT
UID:event-2@example.com
DTSTAMP:20261116T080000Z
DTSTART:20261116T120000Z
DTEND:20261116T130000Z
SUMMARY:lunch with Sam
END:VEVENT
BEGIN:VEVENT
UID:event-3@example.com
DTSTAMP:20261116T080000Z
DTSTART:20261116T160000Z
DTEND:20261116T170000Z
SUMMARY:dentist
END:VEVENT
BEGIN:VEVENT
UID:event-4@example.com
DTSTAMP:20261116T080000Z
DTSTART:20261117T100000Z
DTEND:20261117T110000Z
SUMMARY:project review
END:VEVENT
END:VCALENDAR`

// TestCalendarTodayAndNextWithFakeServer exercises today, next and empty-day phrasing.
func TestCalendarTodayAndNextWithFakeServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		fmt.Fprint(w, sampleICS)
	}))
	t.Cleanup(ts.Close)

	secrets := &memorySecrets{}
	secrets.Set(SecretName, ts.URL+"/feed.ics?token=private-token-12345")

	fixedTime := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixedTime }

	mod := NewModule()
	reg := tool.NewRegistry(2 * time.Second)
	deps := tool.Deps{
		Secrets: secrets,
		Clock:   clock,
		Zone:    tz.New("UTC", nil),
		Logger:  slog.Default(),
	}

	if err := mod.Register(reg, deps); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, ok := reg.Info(Name); !ok {
		t.Fatalf("tool %s not registered", Name)
	}
	if _, ok := reg.Info(NextName); !ok {
		t.Fatalf("tool %s not registered", NextName)
	}

	// 1. Test calendar.today with 3 events
	todayRes, err := reg.Execute(context.Background(), Name, []byte(`{}`))
	if err != nil {
		t.Fatalf("calendar.today: %v", err)
	}
	todayOut, ok := todayRes.(TodayOut)
	if !ok {
		t.Fatalf("calendar.today returned %T, want TodayOut", todayRes)
	}
	if len(todayOut.Events) != 3 {
		t.Fatalf("got %d events today, want 3", len(todayOut.Events))
	}
	wantSpokenToday := "You have 3 events today: stand-up at 9:30, lunch with Sam at 12, dentist at 4."
	if todayOut.Spoken != wantSpokenToday {
		t.Errorf("today spoken = %q, want %q", todayOut.Spoken, wantSpokenToday)
	}
	spokenToday, ok := reg.Speak(Name, todayOut)
	if !ok || spokenToday != wantSpokenToday {
		t.Errorf("Speak returned %q (ok=%v), want %q", spokenToday, ok, wantSpokenToday)
	}

	// 2. Test calendar.today for tomorrow (1 event)
	tomorrowRes, err := reg.Execute(context.Background(), Name, []byte(`{"day":"tomorrow"}`))
	if err != nil {
		t.Fatalf("calendar.today tomorrow: %v", err)
	}
	tomorrowOut := tomorrowRes.(TodayOut)
	if len(tomorrowOut.Events) != 1 {
		t.Fatalf("got %d events tomorrow, want 1", len(tomorrowOut.Events))
	}
	wantTomorrow := "You have 1 event tomorrow: project review at 10."
	if tomorrowOut.Spoken != wantTomorrow {
		t.Errorf("tomorrow spoken = %q, want %q", tomorrowOut.Spoken, wantTomorrow)
	}

	// 3. Test empty-day wording
	emptyRes, err := reg.Execute(context.Background(), Name, []byte(`{"day":"friday"}`))
	if err != nil {
		t.Fatalf("calendar.today friday: %v", err)
	}
	emptyOut := emptyRes.(TodayOut)
	if len(emptyOut.Events) != 0 {
		t.Fatalf("got %d events on friday, want 0", len(emptyOut.Events))
	}
	wantEmptyFriday := "You have no events on Friday."
	if emptyOut.Spoken != wantEmptyFriday {
		t.Errorf("friday empty spoken = %q, want %q", emptyOut.Spoken, wantEmptyFriday)
	}

	// 4. Test calendar.next at 09:00 (first event is stand-up at 9:30)
	nextRes, err := reg.Execute(context.Background(), NextName, []byte(`{}`))
	if err != nil {
		t.Fatalf("calendar.next: %v", err)
	}
	nextOut := nextRes.(NextOut)
	if nextOut.Event == nil || nextOut.Event.Summary != "stand-up" {
		t.Fatalf("calendar.next event = %+v, want stand-up", nextOut.Event)
	}
	wantNext := "Your next event is stand-up at 9:30."
	if nextOut.Spoken != wantNext {
		t.Errorf("next spoken = %q, want %q", nextOut.Spoken, wantNext)
	}

	// 5. Test calendar.next at 11:00 (next event is lunch with Sam at 12)
	fixedTime = time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)
	nextRes2, err := reg.Execute(context.Background(), NextName, []byte(`{}`))
	if err != nil {
		t.Fatalf("calendar.next 2: %v", err)
	}
	nextOut2 := nextRes2.(NextOut)
	if nextOut2.Event == nil || nextOut2.Event.Summary != "lunch with Sam" {
		t.Fatalf("calendar.next 2 event = %+v, want lunch with Sam", nextOut2.Event)
	}
	wantNext2 := "Your next event is lunch with Sam at 12."
	if nextOut2.Spoken != wantNext2 {
		t.Errorf("next 2 spoken = %q, want %q", nextOut2.Spoken, wantNext2)
	}

	// 6. Test calendar.next at 18:00 (all today events done, next is tomorrow)
	fixedTime = time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	nextRes3, err := reg.Execute(context.Background(), NextName, []byte(`{}`))
	if err != nil {
		t.Fatalf("calendar.next 3: %v", err)
	}
	nextOut3 := nextRes3.(NextOut)
	if nextOut3.Event == nil || nextOut3.Event.Summary != "project review" {
		t.Fatalf("calendar.next 3 event = %+v, want project review", nextOut3.Event)
	}
	wantNext3 := "Your next event is project review tomorrow at 10."
	if nextOut3.Spoken != wantNext3 {
		t.Errorf("next 3 spoken = %q, want %q", nextOut3.Spoken, wantNext3)
	}

	// 7. Test calendar.next past all events in window
	fixedTime = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	nextRes4, err := reg.Execute(context.Background(), NextName, []byte(`{}`))
	if err != nil {
		t.Fatalf("calendar.next 4: %v", err)
	}
	nextOut4 := nextRes4.(NextOut)
	if nextOut4.Event != nil {
		t.Errorf("expected nil event, got %+v", nextOut4.Event)
	}
	if nextOut4.Spoken != "You have no upcoming events." {
		t.Errorf("next empty spoken = %q, want 'You have no upcoming events.'", nextOut4.Spoken)
	}
}

// TestURLNeverAppearsInLogsOrErrors proves private calendar URLs and tokens never leak.
func TestURLNeverAppearsInLogsOrErrors(t *testing.T) {
	const secretToken = "super-secret-token-xyz-987"
	const secretPath = "/private-feed/user42.ics"

	cases := []struct {
		name    string
		handler http.HandlerFunc
		urlFn   func(serverURL string) string
	}{
		{
			name: "http_500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "server error", http.StatusInternalServerError)
			},
			urlFn: func(srv string) string { return srv + secretPath + "?token=" + secretToken },
		},
		{
			name: "http_404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "not found", http.StatusNotFound)
			},
			urlFn: func(srv string) string { return srv + secretPath + "?token=" + secretToken },
		},
		{
			name: "oversized_feed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/calendar")
				// Write > 2 MiB
				chunk := bytes.Repeat([]byte("A"), 64*1024)
				for i := 0; i < 35; i++ { // 35 * 64KB = 2240 KB > 2 MiB
					w.Write(chunk)
				}
			},
			urlFn: func(srv string) string { return srv + secretPath + "?token=" + secretToken },
		},
		{
			name: "malformed_ics",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/calendar")
				w.Write([]byte("NOT A VALID ICALENDAR STREAM"))
			},
			urlFn: func(srv string) string { return srv + secretPath + "?token=" + secretToken },
		},
		{
			name: "forbidden_redirect",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://another-host.example.com/feed.ics?token="+secretToken, http.StatusFound)
			},
			urlFn: func(srv string) string { return srv + secretPath + "?token=" + secretToken },
		},
		{
			name: "insecure_cleartext_origin",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			urlFn: func(_ string) string {
				return "http://insecure-host.example.com" + secretPath + "?token=" + secretToken
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			targetURL := tc.urlFn(srv.URL)
			secrets := &memorySecrets{}
			secrets.Set(SecretName, targetURL)

			capturer := &captureHandler{}
			logger := slog.New(capturer)

			mod := NewModule()
			reg := tool.NewRegistry(2 * time.Second)
			deps := tool.Deps{
				Secrets: secrets,
				Clock:   time.Now,
				Zone:    tz.New("UTC", nil),
				Logger:  logger,
			}
			if err := mod.Register(reg, deps); err != nil {
				t.Fatalf("Register: %v", err)
			}

			if _, ok := reg.Info(Name); !ok {
				t.Fatalf("tool %s not registered", Name)
			}

			// Call tool
			_, toolErr := reg.Execute(context.Background(), Name, []byte(`{}`))
			// Check Health
			healthErr := mod.Health(context.Background())

			// Collect all strings that could leak the secret
			logOutput := capturer.String()
			errString := ""
			if toolErr != nil {
				errString += toolErr.Error() + " "
			}
			if healthErr != nil {
				errString += healthErr.Error() + " "
			}
			if lastErr := mod.client.LastError(); lastErr != nil {
				errString += lastErr.Error() + " "
			}

			allOutput := logOutput + " " + errString
			if strings.Contains(allOutput, secretToken) {
				t.Fatalf("secret token leaked in output:\n%s", allOutput)
			}
			if strings.Contains(allOutput, secretPath) {
				t.Fatalf("secret path leaked in output:\n%s", allOutput)
			}
			if strings.Contains(allOutput, targetURL) {
				t.Fatalf("full URL leaked in output:\n%s", allOutput)
			}
		})
	}
}

// TestCacheAndKeepLastGoodCopy proves cached events are returned on subsequent calls
// and preserved when an upstream fetch fails.
func TestCacheAndKeepLastGoodCopy(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			http.Error(w, "upstream failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		fmt.Fprint(w, sampleICS)
	}))
	t.Cleanup(srv.Close)

	secrets := &memorySecrets{}
	secrets.Set(SecretName, srv.URL+"/calendar.ics")

	fixedTime := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixedTime }

	mod := NewModule()
	reg := tool.NewRegistry(2 * time.Second)
	deps := tool.Deps{
		Secrets: secrets,
		Clock:   clock,
		Zone:    tz.New("UTC", nil),
		Logger:  slog.Default(),
	}
	if err := mod.Register(reg, deps); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Call 1: Success
	res1, err := reg.Execute(context.Background(), Name, []byte(`{}`))
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	out1 := res1.(TodayOut)
	if len(out1.Events) != 3 {
		t.Fatalf("call 1 events = %d, want 3", len(out1.Events))
	}
	if calls != 1 {
		t.Fatalf("server calls = %d, want 1", calls)
	}

	// Call 2: Within 10 minutes cache window; server should not be called again.
	fixedTime = fixedTime.Add(5 * time.Minute)
	res2, err := reg.Execute(context.Background(), Name, []byte(`{}`))
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	out2 := res2.(TodayOut)
	if len(out2.Events) != 3 {
		t.Fatalf("call 2 events = %d, want 3", len(out2.Events))
	}
	if calls != 1 {
		t.Fatalf("server calls = %d, want 1 (cache should prevent call)", calls)
	}

	// Call 3: After 15 minutes, cache expired. Server returns 503, but last good copy is kept!
	fixedTime = fixedTime.Add(15 * time.Minute)
	res3, err := reg.Execute(context.Background(), Name, []byte(`{}`))
	if err != nil {
		t.Fatalf("call 3 failed despite last good copy: %v", err)
	}
	out3 := res3.(TodayOut)
	if len(out3.Events) != 3 {
		t.Fatalf("call 3 events = %d, want 3 (reused last good copy)", len(out3.Events))
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}

	// Health now reports error class http_503, but event count is preserved
	info := mod.HealthInfo()
	if info.EventCount != 4 {
		t.Errorf("HealthInfo.EventCount = %d, want 4", info.EventCount)
	}
	if info.ErrorClass != "http_503" {
		t.Errorf("HealthInfo.ErrorClass = %q, want http_503", info.ErrorClass)
	}
	if err := mod.Health(context.Background()); err == nil {
		t.Errorf("Health() = nil, want error for http_503")
	}
}

// TestLocalOnlyGating verifies the config switch toggles LocalOnly between Yes and No.
func TestLocalOnlyGating(t *testing.T) {
	// Default: allow_agent is false -> LocalOnlyYes
	modDefault := NewModule()
	regDefault := tool.NewRegistry(2 * time.Second)
	if err := modDefault.Register(regDefault, tool.Deps{}); err != nil {
		t.Fatalf("Register default: %v", err)
	}
	tInfoDefault, _ := regDefault.Info(Name)
	if !tInfoDefault.LocalOnly {
		t.Errorf("default %s LocalOnly = false, want true (LocalOnlyYes)", Name)
	}
	nInfoDefault, _ := regDefault.Info(NextName)
	if !nInfoDefault.LocalOnly {
		t.Errorf("default %s LocalOnly = false, want true (LocalOnlyYes)", NextName)
	}

	// With allow_agent: true -> LocalOnlyNo
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("allow_agent: true"), &node); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	modAgent := NewModule()
	if err := modAgent.Configure(&node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	regAgent := tool.NewRegistry(2 * time.Second)
	if err := modAgent.Register(regAgent, tool.Deps{}); err != nil {
		t.Fatalf("Register agent: %v", err)
	}
	tInfoAgent, _ := regAgent.Info(Name)
	if tInfoAgent.LocalOnly {
		t.Errorf("agent-enabled %s LocalOnly = true, want false (LocalOnlyNo)", Name)
	}
	nInfoAgent, _ := regAgent.Info(NextName)
	if nInfoAgent.LocalOnly {
		t.Errorf("agent-enabled %s LocalOnly = true, want false (LocalOnlyNo)", NextName)
	}
}
