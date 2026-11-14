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

// alertsResp decodes the parts of the Scheduled alerts view the tests assert
// on (encoding/json default field names).
type alertsResp struct {
	Flash       string
	Error       string
	HasManagers bool
	ZoneName    string
	Alerts      []struct {
		KindKey, Kind, Label, Next, NextISO, Detail, CancelAction string
	}
	Alarms []struct {
		ID, Title, Schedule                         string
		SoundCurrent, VolumeCurrent, RampCurrent    string
		SoundOverride, VolumeOverride, RampOverride bool
		HasOverride                                 bool
		Sounds                                      []struct {
			Value, Label string
			Selected     bool
		}
		Edit struct {
			AlarmID, Sound, Volume, Ramp string
			VolumeReset, RampReset       bool
		}
	}
	Missed []struct{ Kind, Label, EndedBy string }
}

// get reads the alerts JSON (default view or any ?flash= variant).
func (f *alertsFixture) get(target string) (alertsResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	var v alertsResp
	rec := getJSON(f.t, f.handler, target, &v)
	return v, rec
}

// view is get for the common case: a 200 /api/ui/alerts read.
func (f *alertsFixture) view() alertsResp {
	f.t.Helper()
	v, rec := f.get("/api/ui/alerts")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET alerts = %d: %s", rec.Code, rec.Body.String())
	}
	return v
}

// postView posts a write and decodes the answered view.
func (f *alertsFixture) postView(target string, values url.Values) (alertsResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.post(target, values)
	var v alertsResp
	decodeJSON(f.t, rec, &v)
	return v, rec
}

// alertRows returns the active alert rows of one kind key.
func (v alertsResp) kindLabels(kind string) []string {
	var labels []string
	for _, row := range v.Alerts {
		if row.KindKey == kind {
			labels = append(labels, row.Label)
		}
	}
	return labels
}

func (v alertsResp) hasLabel(label string) bool {
	for _, row := range v.Alerts {
		if row.Label == label {
			return true
		}
	}
	return false
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
	var v alertsResp
	rec := getJSON(t, handler, "/api/ui/alerts", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if v.HasManagers || len(v.Alerts) != 0 || len(v.Alarms) != 0 {
		t.Errorf("view without managers = %+v, want an empty read-only view", v)
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

	v, rec := f.get("/api/ui/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !v.HasManagers || v.ZoneName != "Europe/Berlin" {
		t.Errorf("HasManagers = %v, ZoneName = %q", v.HasManagers, v.ZoneName)
	}
	type row struct{ Kind, Label, Detail, Next, NextISO string }
	byLabel := map[string]row{}
	for _, a := range v.Alerts {
		byLabel[a.Label] = row{a.Kind, a.Label, a.Detail, a.Next, a.NextISO}
	}
	for label, want := range map[string]row{
		"pasta":    {Kind: "Timer", Detail: "10m0s of 10m0s left", NextISO: "2026-11-12T05:10:00Z"},
		"work":     {Kind: "Alarm", Detail: "Every day", NextISO: "2026-11-12T06:00:00Z"},
		"call mum": {Kind: "Reminder", NextISO: "2026-11-12T16:00:00Z"},
	} {
		got, ok := byLabel[label]
		if !ok {
			t.Errorf("view lacks the %q alert: %+v", label, v.Alerts)
			continue
		}
		if got.Kind != want.Kind || !strings.Contains(got.Detail, want.Detail) || got.NextISO != want.NextISO {
			t.Errorf("%s = %+v, want kind %q detail %q iso %q", label, got, want.Kind, want.Detail, want.NextISO)
		}
	}
	// The German local time is one hour ahead of UTC in November.
	if !strings.Contains(byLabel["pasta"].Next, "06:10") {
		t.Errorf("the Timer's next time %q was not shown in the live zone", byLabel["pasta"].Next)
	}
	if !strings.Contains(byLabel["work"].Next, "07:00") || !strings.Contains(byLabel["call mum"].Next, "17:00") {
		t.Errorf("alarm/reminder next times = %q / %q", byLabel["work"].Next, byLabel["call mum"].Next)
	}
	if len(v.Alarms) != 1 || !strings.Contains(v.Alarms[0].Title, "work") {
		t.Errorf("alarm editors = %+v, want one for the work alarm", v.Alarms)
	}
	// A hostile reminder message round-trips exactly and cannot be raw markup.
	if !v.hasLabel("Read <script>alert(1)</script>") {
		t.Error("the hostile reminder message did not round-trip through JSON")
	}
	noRawScript(t, body)
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

	v, rec := f.postView(target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(v.Flash, "Cancelled.") || v.hasLabel("pasta") {
		t.Fatalf("cancel view = flash %q alerts %+v, want the Timer gone", v.Flash, v.Alerts)
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
	v, rec = f.postView(target, nil)
	if rec.Code != http.StatusOK || !strings.Contains(v.Flash, "already gone") {
		t.Fatalf("second cancel = %d flash %q", rec.Code, v.Flash)
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

	saved, rec := f.postView(target, url.Values{"sound": {"birds"}, "volume": {"0"}, "ramp": {"10"}})
	if rec.Code != http.StatusOK || !strings.HasPrefix(saved.Flash, "Saved.") {
		t.Fatalf("save status = %d flash %q, body = %s", rec.Code, saved.Flash, rec.Body.String())
	}
	row := f.alarm.List()
	if len(row) != 1 {
		t.Fatalf("alarms = %+v, want one", row)
	}
	if row[0].Sound != "birds" || row[0].Volume == nil || *row[0].Volume != 0 ||
		row[0].RampSeconds == nil || *row[0].RampSeconds != 10 {
		t.Fatalf("stored overrides = %+v, want birds / explicit 0 / 10s", row[0])
	}
	if len(saved.Alarms) != 1 {
		t.Fatalf("saved view alarms = %+v", saved.Alarms)
	}
	if got := saved.Alarms[0]; got.SoundCurrent != "birds" || got.VolumeCurrent != "0%" || got.RampCurrent != "10s" || !got.HasOverride {
		t.Errorf("saved editor = %+v, want birds / 0%% / 10s with overrides set", got)
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
	if v := f.view(); len(v.Alarms) != 1 || v.Alarms[0].SoundCurrent != "rain" {
		t.Errorf("the view did not show the voice-written sound: %+v", v.Alarms)
	}

	// Reset clears to NULL/kind defaults, never to an explicit 0.
	reset, rec := f.postView(target+"/reset", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(reset.Flash, "Reset.") {
		t.Fatalf("reset status = %d flash %q, body = %s", rec.Code, reset.Flash, rec.Body.String())
	}
	row = f.alarm.List()
	if row[0].Sound != "" || row[0].Volume != nil || row[0].RampSeconds != nil {
		t.Fatalf("reset row = %+v, want all overrides cleared", row[0])
	}
	if len(reset.Alarms) != 1 || reset.Alarms[0].HasOverride || reset.Alarms[0].SoundCurrent != "Default" ||
		reset.Alarms[0].VolumeCurrent != "Default" || reset.Alarms[0].RampCurrent != "Default" {
		t.Errorf("the view did not show the cleared state: %+v", reset.Alarms)
	}

	// An explicit 0 after a reset is a value, not a default; a second reset
	// returns it to NULL.
	zero, rec := f.postView(target, url.Values{"volume": {"0"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit-zero save status = %d", rec.Code)
	}
	row = f.alarm.List()
	if row[0].Volume == nil || *row[0].Volume != 0 || row[0].RampSeconds != nil {
		t.Fatalf("explicit-zero row = %+v, want volume 0 and ramp NULL", row[0])
	}
	if len(zero.Alarms) != 1 || zero.Alarms[0].VolumeCurrent != "0%" || !zero.Alarms[0].VolumeOverride {
		t.Errorf("the view did not show the explicit 0%% override: %+v", zero.Alarms)
	}
	if rec := f.post(target+"/reset", nil); rec.Code != http.StatusOK {
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

	rejected, rec := f.postView(target, url.Values{"sound": {"birds"}, "volume": {"150"}, "ramp": {"7"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid volume status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rejected.Error, "between 0 and 100") {
		t.Errorf("rejected error = %q", rejected.Error)
	}
	if len(rejected.Alarms) != 1 {
		t.Fatalf("rejected view alarms = %+v", rejected.Alarms)
	}
	edit := rejected.Alarms[0].Edit
	if edit.AlarmID != set.ID || edit.Volume != "150" || edit.Ramp != "7" || edit.Sound != "birds" {
		t.Errorf("rejected edit state = %+v, want the submitted input preserved", edit)
	}
	selected := ""
	for _, option := range rejected.Alarms[0].Sounds {
		if option.Selected {
			selected = option.Value
		}
	}
	if selected != "birds" {
		t.Errorf("the rejected form selected %q, want the submitted sound", selected)
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

	// A non-numeric volume is a 400 too, with the text preserved verbatim.
	rejected, rec = f.postView(target, url.Values{"volume": {"abc"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric volume status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rejected.Error, "Volume must be a whole number") || len(rejected.Alarms) != 1 || rejected.Alarms[0].Edit.Volume != "abc" {
		t.Errorf("non-numeric view = error %q alarms %+v", rejected.Error, rejected.Alarms)
	}
	if row := f.alarm.List(); row[0].Volume != nil {
		t.Fatalf("invalid submission wrote %+v", row[0])
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
	if got := len(f.view().kindLabels("alarm")); got != 1 {
		t.Fatalf("active Alarm rows while ringing = %d, want 1 (deduped)", got)
	}

	// A second Alarm is created while the first Ring owns the slot. It waits
	// on the same one-speaker queue; cancelling the first by id must let this
	// unrelated new Ring start, not cancel it too.
	second, err := f.alarm.Set(f.ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "second"})
	if err != nil {
		t.Fatalf("set second: %v", err)
	}
	rec := f.post("/alerts/alarm/"+url.PathEscape(first.ID)+"/cancel", nil)
	if rec.Code != http.StatusOK {
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
	if rec := f.post("/alerts/alarm/"+url.PathEscape(second.ID)+"/cancel", nil); rec.Code != http.StatusOK {
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

	v := f.view()
	details := map[string]string{}
	for _, a := range v.Alerts {
		details[a.Label] = a.Detail
	}
	if details["wake once"] != "Ringing now" || details["kettle"] != "Fired; waiting for the speaker" {
		t.Fatalf("active alerts = %v, want the ringing one-off and the queued Timer", details)
	}
	if got := len(v.kindLabels("alarm")); got != 1 {
		t.Fatalf("active Alarm rows = %d, want the one firing Alarm", got)
	}
	// A fired one-off has no next occurrence, so it must not get an override
	// editor; only its active cancel row belongs on the page.
	if len(v.Alarms) != 0 {
		t.Fatalf("fired one-off Alarm got a nonexistent pending editor: %+v", v.Alarms)
	}

	// Cancelling the ringing one-off frees the speaker for the queued Timer.
	if rec := f.post("/alerts/alarm/"+url.PathEscape(fired.ID)+"/cancel", nil); rec.Code != http.StatusOK {
		t.Fatalf("cancel ringing one-off status = %d", rec.Code)
	}
	f.player.waitStart(t)

	// The Timer now rings and stays on the page; cancel by its id ends it and
	// records the user's cancel rather than a shutdown.
	if v := f.view(); !v.hasLabel("kettle") {
		t.Fatalf("ringing Timer missing after it started: %+v", v.Alerts)
	}
	if rec := f.post("/alerts/timer/"+url.PathEscape(timerItem.ID)+"/cancel", nil); rec.Code != http.StatusOK {
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

	if v := f.view(); v.hasLabel("wake once") || v.hasLabel("kettle") {
		t.Fatalf("cancelled alerts stayed in the view: %+v", v.Alerts)
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

	v, rec := f.postView("/alerts/alarm/"+url.PathEscape(alarmSet.ID)+"/cancel", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(v.Flash, "Cancelled.") || v.hasLabel("work") {
		t.Fatalf("alarm cancel status = %d, flash %q, alerts %+v", rec.Code, v.Flash, v.Alerts)
	}
	if rows := f.alarm.List(); len(rows) != 0 {
		t.Fatalf("Alarm cancel left %+v", rows)
	}
	v, rec = f.postView("/alerts/reminder/"+url.PathEscape(reminderItem.ID)+"/cancel", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(v.Flash, "Cancelled.") || v.hasLabel("call mum") {
		t.Fatalf("reminder cancel status = %d, flash %q, alerts %+v", rec.Code, v.Flash, v.Alerts)
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

// TestAlertsWritesAnswerTheRefreshedViewAsJSON pins the write contract the
// React app relies on: save, reset and cancel each answer 200 with the
// refreshed view and a Flash, never a redirect.
func TestAlertsWritesAnswerTheRefreshedViewAsJSON(t *testing.T) {
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

	v, rec := f.postView("/alerts/alarms/"+url.PathEscape(set.ID), url.Values{"sound": {"birds"}})
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(v.Flash, "Saved.") || len(v.Alarms) != 1 || v.Alarms[0].SoundCurrent != "birds" {
		t.Errorf("save view = flash %q alarms %+v", v.Flash, v.Alarms)
	}
	if rows := f.alarm.List(); rows[0].Sound != "birds" {
		t.Fatalf("save stored sound %q", rows[0].Sound)
	}

	v, rec = f.postView("/alerts/alarms/"+url.PathEscape(set.ID)+"/reset", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(v.Flash, "Reset.") {
		t.Fatalf("reset status = %d flash %q", rec.Code, v.Flash)
	}
	if rows := f.alarm.List(); rows[0].Sound != "" {
		t.Fatalf("reset left %q", rows[0].Sound)
	}

	v, rec = f.postView("/alerts/timer/"+url.PathEscape(timerItem.ID)+"/cancel", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(v.Flash, "Cancelled.") || v.hasLabel("pasta") {
		t.Fatalf("cancel status = %d flash %q alerts %+v", rec.Code, v.Flash, v.Alerts)
	}
	if list := f.timer.List(); len(list) != 0 {
		t.Fatalf("cancel left %+v", list)
	}
}

// TestAlertsReadHonoursTheFlashQuery keeps ?flash= working on the JSON read,
// for a client that reloads after a redirecting form.
func TestAlertsReadHonoursTheFlashQuery(t *testing.T) {
	f := newAlertsFixture(t)
	v, rec := f.get("/api/ui/alerts?flash=alert-cancelled")
	if rec.Code != http.StatusOK || !strings.HasPrefix(v.Flash, "Cancelled.") {
		t.Fatalf("flash read = %d %q", rec.Code, v.Flash)
	}
	v, _ = f.get("/api/ui/alerts?flash=%3Cscript%3E")
	if v.Flash != "" {
		t.Errorf("an unknown flash name produced text %q", v.Flash)
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

	v, rec := f.get("/api/ui/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	missed := map[string]string{}
	count := map[string]int{}
	for _, m := range v.Missed {
		missed[m.Label] = m.Kind + "/" + m.EndedBy
		count[m.Label]++
	}
	if !strings.HasPrefix(missed["gym"], "Alarm/") {
		t.Errorf("missed rows = %v, want the Alarm gym row", missed)
	}
	if !strings.HasPrefix(missed["oldtimer"], "Timer/") || !strings.HasSuffix(missed["oldtimer"], "timeout") {
		t.Errorf("missed rows = %v, want the Timer oldtimer row ended by timeout", missed)
	}
	if count["oldtimer"] != 1 {
		t.Fatalf("the missed history row appeared %d times, want once", count["oldtimer"])
	}
	// The pending daily Alarm moved to tomorrow and is still listed.
	if got := v.kindLabels("alarm"); len(got) != 1 || got[0] != "gym" {
		t.Errorf("the repeating Alarm disappeared from the active list after being missed: %+v", v.Alerts)
	}
	remaining := f.alarm.List()
	if len(remaining) != 1 || remaining[0].ID != set.ID {
		t.Fatalf("alarm list = %+v, want the repeating Alarm", remaining)
	}
}

// Dashboard scheduling, editing and recovery all share the managers' audio field.
func TestDashboardCompletionAudio(t *testing.T) {
	f := newAlertsFixture(t)
	rec := f.post("/alerts/create", url.Values{"kind": {"timer"}, "seconds": {"900"}, "audio": {"song"}})
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	timers := f.timer.List()
	if len(timers) != 1 || timers[0].Audio != "song" {
		t.Fatalf("timers=%+v", timers)
	}
	rec = f.post("/alerts/timer/"+timers[0].ID+"/audio", url.Values{"audio": {"playlist: running"}})
	if rec.Code != 200 || f.timer.List()[0].Audio != "playlist: running" {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.post("/alerts/create", url.Values{"kind": {"alarm"}, "clock": {"07:00"}, "audio": {"https://youtu.be/abc"}})
	if rec.Code != 200 {
		t.Fatalf("alarm create: %d %s", rec.Code, rec.Body.String())
	}
	alarms := f.alarm.List()
	if len(alarms) != 1 || alarms[0].Audio != "https://youtu.be/abc" {
		t.Fatalf("alarms=%+v", alarms)
	}
	empty := ""
	out, err := f.alarm.Update(t.Context(), alarm.UpdateIn{ID: alarms[0].ID, Audio: &empty})
	if err != nil || out.Alarm.Audio != "" {
		t.Fatalf("clear=%+v %v", out, err)
	}
}
