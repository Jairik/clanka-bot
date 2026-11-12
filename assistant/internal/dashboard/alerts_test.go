package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// alertClock is a settable clock so the page and the schedulers agree on one
// instant without sleeping through a real day.
type alertClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *alertClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *alertClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// alertSpeaker records spoken lines. The page must never reach it; the Timer
// cancellation test asserts the cancelled alert is absent.
type alertSpeaker struct {
	texts chan string
}

func newAlertSpeaker() *alertSpeaker { return &alertSpeaker{texts: make(chan string, 16)} }

func (s *alertSpeaker) SayText(_ context.Context, text string) error {
	select {
	case s.texts <- text:
	default:
	}
	return nil
}

func (s *alertSpeaker) seen() int {
	return len(s.texts)
}

// alertPlayer captures Ring starts. It can block a pass until the Ring's
// context is cancelled, which is how the ringing-cancel test holds two Alarms
// on the one speaker slot in a known order.
type alertPlayer struct {
	mu      sync.Mutex
	starts  int
	started chan struct{}
	onPlay  func(context.Context) error
}

func newAlertPlayer() *alertPlayer {
	return &alertPlayer{started: make(chan struct{}, 16)}
}

func (p *alertPlayer) Play(ctx context.Context, _ sound.Clip) error {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
	if p.onPlay != nil {
		return p.onPlay(ctx)
	}
	return nil
}

func (p *alertPlayer) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

func (p *alertPlayer) waitStart(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the alert never reached the Ring player")
	}
}

// alertEmbedder fails the test if a canonical library name ever needs a model
// lookup. The Dashboard only submits canonical names, so exact matching must
// resolve every page write without a network call.
type alertEmbedder struct {
	calls *int64
}

func (e alertEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	atomic.AddInt64(e.calls, 1)
	return nil, fmt.Errorf("dashboard must not call the embedder")
}

// alertModules adapts the fixture's real modules to dashboard.ModuleProvider.
type alertModules map[string]tool.Module

func (m alertModules) Module(name string) tool.Module { return m[name] }

// alertsFixture wires the real SQLite store, settings, sound library, three
// managers, Ring engine and authenticated Dashboard.
type alertsFixture struct {
	t        *testing.T
	ctx      context.Context
	dir      string
	store    *store.Store
	settings *settings.Service
	sounds   *sound.Service
	zone     *tz.Service
	clock    *alertClock
	registry *tool.Registry
	modules  alertModules

	timer    *timer.Manager
	alarm    *alarm.Manager
	reminder *reminder.Manager

	player    *alertPlayer
	speaker   *alertSpeaker
	embedCall *int64
	handler   *dashboard.Handler

	ringMu  sync.Mutex
	maxRing time.Duration
}

func newAlertsFixture(t *testing.T) *alertsFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, filepath.Join(dir, "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cues, err := sound.New(ctx, st, dir, testCueFormat, logger)
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	defs := cues.Definitions(sound.Config{})
	defs = append(defs, ring.Definitions()...)
	defs = append(defs, tz.Definition(""))
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	cues.Bind(svc, nil)
	zone := tz.New("", logger)
	zone.Bind(svc)

	clock := &alertClock{now: time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC)}
	player := newAlertPlayer()
	speaker := newAlertSpeaker()
	embedCalls := new(int64)
	matcher := namematch.New(alertEmbedder{calls: embedCalls}, namematch.Config{
		Threshold: func() float64 { return namematch.DefaultThreshold },
	})

	f := &alertsFixture{
		t:         t,
		ctx:       ctx,
		dir:       dir,
		store:     st,
		settings:  svc,
		sounds:    cues,
		zone:      zone,
		clock:     clock,
		player:    player,
		speaker:   speaker,
		embedCall: embedCalls,
		maxRing:   5 * time.Second,
	}
	focusRegistry := focus.New()
	engine := ring.New(ring.Deps{
		Focus: focusRegistry,
		Settings: func(ring.Kind) ring.Settings {
			f.ringMu.Lock()
			defer f.ringMu.Unlock()
			return ring.Settings{
				MaxRing:    f.maxRing,
				Volume:     100,
				Gap:        2 * time.Millisecond,
				SpeakLabel: ring.SpeakNever,
			}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) {
			return sound.Clip{PCM: []byte{0, 0, 0, 0}, Format: audio.Format{Rate: 8000, Width: 2, Channels: 1}}, true
		},
		ClipName: func(name string) (sound.Clip, bool) {
			return cues.ClipName(sound.RoleAlarmRing, name)
		},
		Play:   player.Play,
		Logger: logger,
	})

	deps := tool.Deps{
		Clock:     clock.Now,
		Logger:    logger,
		Store:     st,
		Settings:  func() *settings.Service { return svc },
		Zone:      zone,
		Cues:      cues,
		Names:     matcher,
		Focus:     focusRegistry,
		Ring:      engine,
		Speaker:   speaker,
		LastReply: nil,
	}
	f.registry = tool.NewRegistry(2 * time.Second)
	f.modules = make(alertModules)
	for _, module := range []tool.Module{timer.NewModule(), alarm.NewModule(), reminder.NewModule()} {
		if configurable, ok := module.(tool.Configurable); ok {
			if err := configurable.Configure(nil); err != nil {
				t.Fatalf("Configure(%s): %v", module.Name(), err)
			}
		}
		if err := module.Register(f.registry, deps); err != nil {
			t.Fatalf("Register(%s): %v", module.Name(), err)
		}
		if starter, ok := module.(tool.Startable); ok {
			if err := starter.Start(ctx); err != nil {
				t.Fatalf("Start(%s): %v", module.Name(), err)
			}
		}
		f.modules[module.Name()] = module
	}
	f.timer = f.modules["timer"].(*timer.Module).Manager()
	f.alarm = f.modules["alarm"].(*alarm.Module).Manager()
	f.reminder = f.modules["reminder"].(*reminder.Module).Manager()
	if f.timer == nil || f.alarm == nil || f.reminder == nil {
		t.Fatal("a Scheduled alert manager is nil after Register")
	}
	t.Cleanup(f.timer.Close)
	t.Cleanup(f.alarm.Close)
	t.Cleanup(f.reminder.Close)

	f.handler = newHandler(t, dashboard.Options{
		Settings:    svc,
		Sounds:      cues,
		Zone:        zone,
		ToolModules: f.modules,
		Now:         clock.Now,
	})
	return f
}

// setMaxRing changes the engine's max_ring before an alert is set.
func (f *alertsFixture) setMaxRing(d time.Duration) {
	f.ringMu.Lock()
	f.maxRing = d
	f.ringMu.Unlock()
}

// addSound writes a real WAV into the library and rescans it, so the picker
// and Manager.Update resolve a canonical library name without an embedder.
func (f *alertsFixture) addSound(name string, d time.Duration) {
	f.t.Helper()
	path := filepath.Join(f.sounds.LibraryDir(), name+".wav")
	if err := os.WriteFile(path, testWAV(16000, 1, d, 0.2), 0o600); err != nil {
		f.t.Fatalf("write sound %s: %v", name, err)
	}
	if err := f.sounds.Rescan(f.ctx); err != nil {
		f.t.Fatalf("rescan sounds: %v", err)
	}
}

func (f *alertsFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, target, owner))
	return rec
}

func (f *alertsFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.postWith(target, values, func(*http.Request) {})
}

func (f *alertsFixture) postWith(target string, values url.Values, edit func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	edit(req)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// waitFor polls a local condition with a bounded wall-clock deadline.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAlertsPageWithoutManagersIsReadOnly(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/alerts", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Scheduled alerts are not configured") {
		t.Error("page did not explain the missing module list")
	}
}

func TestAlertsPageListsEveryKindWithNextTimeAndLiveZone(t *testing.T) {
	f := newAlertsFixture(t)
	if _, err := f.settings.Set(f.ctx, tz.Key, "Europe/Berlin"); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	f.clock.Set(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	if _, err := f.timer.Start("pasta", 10*time.Minute); err != nil {
		t.Fatalf("timer.Start: %v", err)
	}
	if _, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"}); err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	if _, err := f.reminder.StartClock("call mum", 17, 0, false); err != nil {
		t.Fatalf("reminder.StartClock: %v", err)
	}
	if _, err := f.reminder.StartClock("Read <script>alert(1)</script>", 18, 0, false); err != nil {
		t.Fatalf("reminder.StartClock: %v", err)
	}

	rec := f.get("/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Scheduled alerts",
		"Active scheduled alerts",
		"Alarm overrides",
		"Missed alerts",
		"Timer", "pasta", "10m0s of 10m0s left",
		"Alarm", "work", "Every day", "07:00",
		"Reminder", "call mum", "17:00",
		"Europe/Berlin",
		`datetime="2026-11-12T06:00:00Z"`,
		`datetime="2026-11-12T16:00:00Z"`,
		`href="/alerts" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The German local time is one hour ahead of UTC in November.
	if !strings.Contains(body, "06:10") {
		t.Error("the Timer's next time was not shown in the live zone")
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a reminder message was rendered as raw markup")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the reminder message was not escaped")
	}
	if atomic.LoadInt64(f.embedCall) != 0 {
		t.Fatalf("rendering the page called the embedder %d times", atomic.LoadInt64(f.embedCall))
	}
}

func TestAlertsCancelStopsOneFutureTimerThroughItsManager(t *testing.T) {
	f := newAlertsFixture(t)
	timerItem, err := f.timer.Start("pasta", 250*time.Millisecond)
	if err != nil {
		t.Fatalf("timer.Start: %v", err)
	}
	target := "/alerts/timer/" + url.PathEscape(timerItem.ID) + "/cancel"

	// Identity and same-origin are required before the cancel route can run.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated cancel status = %d, want 403", rec.Code)
	}
	rec = f.postWith(target, nil, func(req *http.Request) {
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin cancel status = %d, want 403", rec.Code)
	}
	if list := f.timer.List(); len(list) != 1 {
		t.Fatalf("rejected cancels removed the Timer: %+v", list)
	}

	rec = f.post(target, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/alerts?flash=alert-cancelled" {
		t.Fatalf("Location = %q", location)
	}
	time.Sleep(450 * time.Millisecond)
	if list := f.timer.List(); len(list) != 0 {
		t.Fatalf("cancelled Timer is still listed: %+v", list)
	}
	if f.speaker.seen() != 0 {
		t.Fatal("a cancelled future Timer fired its alert")
	}
	var rows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM timers`).Scan(&rows); err != nil {
		t.Fatalf("count timers: %v", err)
	}
	if rows != 0 {
		t.Fatalf("persisted timers = %d after cancel, want 0", rows)
	}
	// A second cancel is honest: zero matched, nothing written.
	rec = f.post(target, nil)
	if location := rec.Header().Get("Location"); location != "/alerts?flash=alert-gone" {
		t.Fatalf("second cancel Location = %q", location)
	}
}

func TestAlertsAlarmOverrideSaveResetAndVoiceShareOneRow(t *testing.T) {
	f := newAlertsFixture(t)
	f.addSound("birds", 100*time.Millisecond)
	f.addSound("rain", 100*time.Millisecond)
	f.clock.Set(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	set, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	id := set.ID
	target := "/alerts/alarms/" + url.PathEscape(id)

	rec := f.post(target, url.Values{"sound": {"birds"}, "volume": {"0"}, "ramp": {"10"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	row := f.alarm.List()
	if len(row) != 1 {
		t.Fatalf("alarms = %+v, want one", row)
	}
	if row[0].Sound != "birds" || row[0].Volume == nil || *row[0].Volume != 0 ||
		row[0].RampSeconds == nil || *row[0].RampSeconds != 10 {
		t.Fatalf("stored overrides = %+v, want birds / explicit 0 / 10s", row[0])
	}
	body := f.get("/alerts").Body.String()
	for _, want := range []string{"birds", "0%", "10s", "Overrides set", "Clear all overrides"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks the saved override value %q", want)
		}
	}

	// The voice Tool and the page use one manager, so an alarm.update is
	// visible on the next page read without a second write path.
	if _, err := f.registry.Execute(f.ctx, "alarm.update", json.RawMessage(`{"id":"`+id+`","sound":"rain"}`)); err != nil {
		t.Fatalf("alarm.update: %v", err)
	}
	row = f.alarm.List()
	if row[0].Sound != "rain" {
		t.Fatalf("voice update stored %q, want rain", row[0].Sound)
	}
	if body := f.get("/alerts").Body.String(); !strings.Contains(body, "rain") {
		t.Error("the page did not show the voice-written sound")
	}

	// Reset clears to NULL/kind defaults, never to an explicit 0.
	rec = f.post(target+"/reset", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	row = f.alarm.List()
	if row[0].Sound != "" || row[0].Volume != nil || row[0].RampSeconds != nil {
		t.Fatalf("reset row = %+v, want all overrides cleared", row[0])
	}
	if body := f.get("/alerts").Body.String(); !strings.Contains(body, "Kind defaults") {
		t.Error("the page did not show the cleared state")
	}

	// An explicit 0 after a reset is a value, not a default; a second reset
	// returns it to NULL.
	if rec := f.post(target, url.Values{"volume": {"0"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("explicit-zero save status = %d", rec.Code)
	}
	row = f.alarm.List()
	if row[0].Volume == nil || *row[0].Volume != 0 || row[0].RampSeconds != nil {
		t.Fatalf("explicit-zero row = %+v, want volume 0 and ramp NULL", row[0])
	}
	if body := f.get("/alerts").Body.String(); !strings.Contains(body, "0%") {
		t.Error("the page did not show the explicit 0% override")
	}
	if rec := f.post(target+"/reset", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("second reset status = %d", rec.Code)
	}
	if row = f.alarm.List(); row[0].Volume != nil {
		t.Fatalf("reset volume = %v, want NULL", *row[0].Volume)
	}
	if calls := atomic.LoadInt64(f.embedCall); calls != 0 {
		t.Fatalf("page/voice canonical names triggered %d embed calls", calls)
	}
}

func TestAlertsAlarmOverrideRejectsInvalidInputWithoutPartialWrite(t *testing.T) {
	f := newAlertsFixture(t)
	f.addSound("birds", 100*time.Millisecond)
	f.addSound("long", 61*time.Second)
	f.clock.Set(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	set, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	target := "/alerts/alarms/" + url.PathEscape(set.ID)

	rec := f.post(target, url.Values{"sound": {"birds"}, "volume": {"150"}, "ramp": {"7"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid volume status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`value="150"`, `value="7"`, "between 0 and 100", `role="alert"`} {
		if !strings.Contains(body, want) {
			t.Errorf("rejected form lacks %q", want)
		}
	}
	if !strings.Contains(body, `value="birds" selected`) {
		t.Error("the rejected form did not preserve the selected sound")
	}
	row := f.alarm.List()
	if len(row) != 1 || row[0].Sound != "" || row[0].Volume != nil || row[0].RampSeconds != nil {
		t.Fatalf("partial write after invalid volume: %+v", row)
	}

	rec = f.post(target, url.Values{"volume": {"0"}, "volume_reset": {"1"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not both") {
		t.Fatalf("contradictory volume status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = f.post(target, url.Values{"ramp": {"3601"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 0 and 60 minutes") {
		t.Fatalf("over-cap ramp status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The sound cap is the existing sound.Service validation, applied by the
	// same Manager.Update the voice Tool calls; the page does not duplicate it.
	rec = f.post(target, url.Values{"sound": {"long"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "longer than 1m0s") {
		t.Fatalf("over-cap sound status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if row := f.alarm.List(); row[0].Volume != nil || row[0].RampSeconds != nil || row[0].Sound != "" {
		t.Fatalf("invalid submissions wrote a value: %+v", row[0])
	}

	// The htmx journey gets a 200 fragment with the same preserved input and
	// no full document.
	rec = f.postWith(target, url.Values{"volume": {"abc"}}, func(req *http.Request) {
		req.Header.Set("HX-Request", "true")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx invalid status = %d", rec.Code)
	}
	fragment := rec.Body.String()
	if strings.Contains(fragment, "<html") || !strings.HasPrefix(strings.TrimSpace(fragment), `<div id="alerts-content">`) {
		t.Fatalf("htmx error was not a content fragment: %s", fragment)
	}
	for _, want := range []string{`value="abc"`, `role="alert"`, "Volume must be a whole number"} {
		if !strings.Contains(fragment, want) {
			t.Errorf("htmx fragment lacks %q", want)
		}
	}
	if row := f.alarm.List(); row[0].Volume != nil {
		t.Fatalf("invalid htmx submission wrote %+v", row[0])
	}
	if calls := atomic.LoadInt64(f.embedCall); calls != 0 {
		t.Fatalf("invalid forms called the embedder %d times", calls)
	}
}

func TestAlertsRingingCancelTargetsOnlyThatAlarm(t *testing.T) {
	f := newAlertsFixture(t)
	f.setMaxRing(5 * time.Second)
	f.player.onPlay = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	f.clock.Set(time.Date(2026, 11, 12, 6, 59, 59, 950_000_000, time.UTC))

	first, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "first"})
	if err != nil {
		t.Fatalf("set first: %v", err)
	}
	f.player.waitStart(t)

	// The repeating Alarm is both a pending row and an open ringing event; the
	// page must list it once, not once per source.
	rec := f.get("/alerts")
	if got := strings.Count(rec.Body.String(), `<tr class="alert-alarm">`); got != 1 {
		t.Fatalf("active Alarm rows while ringing = %d, want 1 (deduped)", got)
	}

	// A second Alarm is created while the first Ring owns the slot. It waits
	// on the same one-speaker queue; cancelling the first by id must let this
	// unrelated new Ring start, not cancel it too.
	second, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "second"})
	if err != nil {
		t.Fatalf("set second: %v", err)
	}
	rec = f.post("/alerts/alarm/"+url.PathEscape(first.ID)+"/cancel", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ringing cancel status = %d, body = %s", rec.Code, rec.Body.String())
	}
	f.player.waitStart(t)
	if got := f.player.startCount(); got < 2 {
		t.Fatalf("Ring starts = %d, want the unrelated Alarm to ring", got)
	}
	remaining := f.alarm.List()
	if len(remaining) != 1 || remaining[0].ID != second.ID {
		t.Fatalf("remaining Alarms = %+v, want only the second", remaining)
	}
	if rec := f.post("/alerts/alarm/"+url.PathEscape(second.ID)+"/cancel", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("second cancel status = %d", rec.Code)
	}

	waitFor(t, 3*time.Second, "both cancelled Alarm history rows", func() bool {
		events, err := f.alarm.History(10)
		if err != nil {
			return false
		}
		terminal := 0
		for _, event := range events {
			if event.Kind == "alarm" && !event.State.Open() {
				terminal++
			}
		}
		return terminal >= 2
	})
	events, err := f.alarm.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	seen := map[string]string{}
	for _, event := range events {
		seen[event.SourceID] = string(event.State) + "/" + event.EndedBy
	}
	if got := seen[first.ID]; got != "stopped/cancel" {
		t.Errorf("first event = %q, want stopped/cancel", got)
	}
	if got := seen[second.ID]; got != "stopped/cancel" {
		t.Errorf("second event = %q, want stopped/cancel", got)
	}
}

func TestAlertsListsAndCancelsOneOffAlarmAndRingingTimer(t *testing.T) {
	f := newAlertsFixture(t)
	f.setMaxRing(5 * time.Second)
	f.player.onPlay = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	f.clock.Set(time.Date(2026, 11, 12, 6, 59, 59, 950_000_000, time.UTC))

	// A one-off Alarm deletes its pending row when it fires, and List omits the
	// row while its Ring finishes. The page must still show it and cancel it.
	fired, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: string(alarm.Once), Label: "wake once"})
	if err != nil {
		t.Fatalf("set one-off Alarm: %v", err)
	}
	f.player.waitStart(t)
	if rows := f.alarm.List(); len(rows) != 0 {
		t.Fatalf("pending Alarms after firing = %+v, want none", rows)
	}

	// A real Timer fires while the Alarm owns the speaker. Its pending row and
	// live entry are already gone too, so only the open history row remains.
	timerItem, err := f.timer.Start("kettle", 120*time.Millisecond)
	if err != nil {
		t.Fatalf("start Timer: %v", err)
	}
	waitFor(t, 3*time.Second, "the Timer to reach an open history row", func() bool {
		events, err := f.timer.History(10)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "timer" && event.SourceID == timerItem.ID && event.State.Open() {
				return true
			}
		}
		return false
	})

	rec := f.get("/alerts")
	body := rec.Body.String()
	for _, want := range []string{"wake once", "kettle", "Ringing now", "Fired; waiting for the speaker"} {
		if !strings.Contains(body, want) {
			t.Fatalf("active page missing %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, `<tr class="alert-alarm">`); got != 1 {
		t.Fatalf("active Alarm rows = %d, want the one firing Alarm", got)
	}
	// A fired one-off has no next occurrence, so it must not get an override
	// editor; only its active cancel row belongs on the page.
	if !strings.Contains(body, "No Alarms are set.") || strings.Contains(body, `id="alarm-overrides-`+fired.ID+`"`) {
		t.Fatalf("fired one-off Alarm got a nonexistent pending editor:\n%s", body)
	}

	// Cancelling the ringing one-off frees the speaker for the queued Timer.
	if rec := f.post("/alerts/alarm/"+url.PathEscape(fired.ID)+"/cancel", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel ringing one-off status = %d", rec.Code)
	}
	f.player.waitStart(t)

	// The Timer now rings and stays on the page; cancel by its id ends it and
	// records the user's cancel rather than a shutdown.
	rec = f.get("/alerts")
	if !strings.Contains(rec.Body.String(), "kettle") {
		t.Fatalf("ringing Timer missing after it started:\n%s", rec.Body.String())
	}
	if rec := f.post("/alerts/timer/"+url.PathEscape(timerItem.ID)+"/cancel", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel ringing Timer status = %d", rec.Code)
	}
	waitFor(t, 3*time.Second, "both open Ring history rows to finish", func() bool {
		events, err := f.alarm.History(10)
		if err != nil {
			return false
		}
		seen := map[string]string{}
		for _, event := range events {
			if event.Kind == "alarm" && event.SourceID == fired.ID {
				seen["alarm/"+event.SourceID] = string(event.State) + "/" + event.EndedBy
			}
			if event.Kind == "timer" && event.SourceID == timerItem.ID {
				seen["timer/"+event.SourceID] = string(event.State) + "/" + event.EndedBy
			}
		}
		return seen["alarm/"+fired.ID] == "stopped/cancel" && seen["timer/"+timerItem.ID] == "stopped/cancel"
	})

	rec = f.get("/alerts")
	body = rec.Body.String()
	if strings.Contains(body, "wake once") || strings.Contains(body, "kettle") {
		t.Fatalf("cancelled alerts stayed on the page:\n%s", body)
	}
}

func TestAlertsMutationsRequireIdentityAndSameOrigin(t *testing.T) {
	f := newAlertsFixture(t)
	f.clock.Set(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	set, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	update := "/alerts/alarms/" + url.PathEscape(set.ID)
	routes := []string{update, update + "/reset", "/alerts/alarm/" + url.PathEscape(set.ID) + "/cancel"}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(""))
			req.Host = "127.0.0.1:8082"
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("unauthenticated status = %d, want 403", rec.Code)
			}
			rec = f.postWith(route, nil, func(req *http.Request) {
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("cross-origin status = %d, want 403", rec.Code)
			}
		})
	}
	if rows := f.alarm.List(); len(rows) != 1 || rows[0].Sound != "" || rows[0].Volume != nil {
		t.Fatalf("rejected mutations changed the Alarm: %+v", rows)
	}
}

func TestAlertsCancelAlarmAndReminderByID(t *testing.T) {
	f := newAlertsFixture(t)
	f.clock.Set(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	alarmSet, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	reminderItem, err := f.reminder.StartClock("call mum", 17, 0, false)
	if err != nil {
		t.Fatalf("reminder.StartClock: %v", err)
	}

	rec := f.post("/alerts/alarm/"+url.PathEscape(alarmSet.ID)+"/cancel", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/alerts?flash=alert-cancelled" {
		t.Fatalf("alarm cancel status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if rows := f.alarm.List(); len(rows) != 0 {
		t.Fatalf("Alarm cancel left %+v", rows)
	}
	rec = f.post("/alerts/reminder/"+url.PathEscape(reminderItem.ID)+"/cancel", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/alerts?flash=alert-cancelled" {
		t.Fatalf("reminder cancel status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if rows := f.reminder.List(); len(rows) != 0 {
		t.Fatalf("Reminder cancel left %+v", rows)
	}

	// An unknown kind is a 404, not a silent no-op or a broad cancel.
	rec = f.post("/alerts/unicorn/1/cancel", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown kind status = %d, want 404", rec.Code)
	}
}

func TestAlertsHTMXSaveResetAndCancelReturnFragments(t *testing.T) {
	f := newAlertsFixture(t)
	f.addSound("birds", 100*time.Millisecond)
	f.clock.Set(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	set, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	timerItem, err := f.timer.Start("pasta", time.Hour)
	if err != nil {
		t.Fatalf("timer.Start: %v", err)
	}

	// Save: one htmx request returns the content fragment and writes the row.
	rec := f.postWith("/alerts/alarms/"+url.PathEscape(set.ID), url.Values{"sound": {"birds"}}, func(req *http.Request) {
		req.Header.Set("HX-Request", "true")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	checkAlertsFragment(t, rec.Body.String())
	for _, want := range []string{"Saved.", "birds"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("htmx save fragment lacks %q", want)
		}
	}
	if rows := f.alarm.List(); rows[0].Sound != "birds" {
		t.Fatalf("htmx save stored sound %q", rows[0].Sound)
	}

	// Reset: same route family, fragment response.
	rec = f.postWith("/alerts/alarms/"+url.PathEscape(set.ID)+"/reset", nil, func(req *http.Request) {
		req.Header.Set("HX-Request", "true")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx reset status = %d", rec.Code)
	}
	checkAlertsFragment(t, rec.Body.String())
	if !strings.Contains(rec.Body.String(), "Reset.") {
		t.Error("htmx reset fragment lacks the reset notice")
	}
	if rows := f.alarm.List(); rows[0].Sound != "" {
		t.Fatalf("htmx reset left %q", rows[0].Sound)
	}

	// Cancel: the htmx path swaps the content and removes the Timer.
	rec = f.postWith("/alerts/timer/"+url.PathEscape(timerItem.ID)+"/cancel", nil, func(req *http.Request) {
		req.Header.Set("HX-Request", "true")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx cancel status = %d", rec.Code)
	}
	checkAlertsFragment(t, rec.Body.String())
	if !strings.Contains(rec.Body.String(), "Cancelled.") {
		t.Error("htmx cancel fragment lacks the cancellation notice")
	}
	if list := f.timer.List(); len(list) != 0 {
		t.Fatalf("htmx cancel left %+v", list)
	}
}

// checkAlertsFragment asserts an htmx response is the swappable content block,
// not a whole page.
func checkAlertsFragment(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, "<html") {
		t.Fatalf("htmx response contains a full document: %s", body)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="alerts-content">`) {
		t.Fatalf("htmx response is not the alerts content fragment: %s", body)
	}
}

func TestAlertsShowsMissedHistoryOnceFromTheSharedStore(t *testing.T) {
	f := newAlertsFixture(t)
	f.setMaxRing(25 * time.Millisecond)
	f.clock.Set(time.Date(2026, 11, 12, 6, 59, 59, 980_000_000, time.UTC))
	set, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "gym"})
	if err != nil {
		t.Fatalf("alarm.Set: %v", err)
	}
	waitFor(t, 3*time.Second, "a missed Alarm history row", func() bool {
		events, err := f.alarm.History(10)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "alarm" && event.State == alertstore.StateMissed {
				return true
			}
		}
		return false
	})

	// A Timer missed in an earlier run has no pending row; the page must still
	// show it from alert_events, and exactly once even though all three
	// managers expose the same cross-kind history.
	now := f.clock.Now()
	if _, err := f.store.DB().Exec(
		`INSERT INTO alert_events (kind, source_id, label, duration_ms, fires_at, fired_at, state, ended_at, ended_by)
		 VALUES ('timer', '99', 'oldtimer', 600000, ?, ?, 'missed', ?, 'timeout')`,
		now.Add(-time.Hour).UnixMilli(), now.Add(-59*time.Minute).UnixMilli(), now.Add(-58*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("seed missed timer: %v", err)
	}

	rec := f.get("/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Missed alerts", "gym", "oldtimer", "Timer", "Alarm", "timeout"} {
		if !strings.Contains(body, want) {
			t.Errorf("missed page lacks %q", want)
		}
	}
	if strings.Count(body, "oldtimer") != 1 {
		t.Fatalf("the missed history row appeared %d times, want once", strings.Count(body, "oldtimer"))
	}
	// The pending daily Alarm moved to tomorrow and is still listed.
	if !strings.Contains(body, "Every day") {
		t.Error("the repeating Alarm disappeared from the active list after being missed")
	}
	remaining := f.alarm.List()
	if len(remaining) != 1 || remaining[0].ID != set.ID {
		t.Fatalf("alarm list = %+v, want the repeating Alarm", remaining)
	}
}
