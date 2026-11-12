package quiet_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
)

// fakeClock is the thread-safe injected clock the window tests advance.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// quietFixture is one real settings service plus the Quiet service over it.
type quietFixture struct {
	service  *quiet.Service
	settings *settings.Service
	clock    *fakeClock
}

// newFixture opens a fresh database at the public settings seam and binds the
// Dashboard/voice writer, so the tests exercise the production composition.
func newFixture(t *testing.T, cfg quiet.Config, at time.Time, location *time.Location) quietFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	svc, err := settings.New(ctx, st, quiet.Definitions(cfg)...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	clock := &fakeClock{now: at}
	service := quiet.New(quiet.Options{
		Reader:   svc,
		Now:      clock.Now,
		Location: func() *time.Location { return location },
	})
	service.BindWriter(svc)
	return quietFixture{service: service, settings: svc, clock: clock}
}

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestDailyWindowAcrossMidnight(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 21, 0), time.UTC)
	service := fixture.service

	status := service.Status()
	if status.Quiet || status.Reason != quiet.ReasonOff {
		t.Fatalf("21:00 status = %+v, want off", status)
	}
	if !status.NextChange.Equal(utc(2026, 11, 12, 22, 30)) {
		t.Fatalf("next change = %v, want tonight's start", status.NextChange)
	}

	fixture.clock.Set(utc(2026, 11, 12, 23, 0))
	status = service.Status()
	if !status.Quiet || status.Reason != quiet.ReasonWindow {
		t.Fatalf("23:00 status = %+v, want the window", status)
	}
	if !status.WindowEnd.Equal(utc(2026, 11, 13, 7, 0)) {
		t.Fatalf("window end = %v, want tomorrow 07:00", status.WindowEnd)
	}
	if !status.NextChange.Equal(status.WindowEnd) {
		t.Fatalf("next change = %v, want the window end", status.NextChange)
	}

	fixture.clock.Set(utc(2026, 11, 13, 0, 30))
	status = service.Status()
	if !status.Quiet || !status.WindowStart.Equal(utc(2026, 11, 12, 22, 30)) {
		t.Fatalf("00:30 status = %+v, want yesterday's window start", status)
	}

	fixture.clock.Set(utc(2026, 11, 13, 6, 59))
	if !service.IsQuiet() {
		t.Fatal("06:59 is not quiet")
	}
	fixture.clock.Set(utc(2026, 11, 13, 7, 0))
	if service.IsQuiet() {
		t.Fatal("07:00 is quiet; the end boundary is exclusive")
	}
}

func TestWindowUsesTheConfiguredTimezone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load Tokyo: %v", err)
	}
	at := utc(2026, 11, 12, 14, 0) // 23:00 in Tokyo, 14:00 UTC
	local := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, at, tokyo)
	if !local.service.IsQuiet() {
		t.Fatalf("23:00 Tokyo status = %+v, want quiet", local.service.Status())
	}
	utcFixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, at, time.UTC)
	if utcFixture.service.IsQuiet() {
		t.Fatalf("14:00 UTC status = %+v, want not quiet", utcFixture.service.Status())
	}
}

func TestDaylightSavingWindowEndIsTheLocalClock(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Berlin: %v", err)
	}
	// Spring forward: 29 March 2026 at 03:30 local, inside the overnight
	// window. The end must still be 07:00 local, not a fixed 8.5-hour span.
	spring := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, time.Date(2026, 3, 29, 3, 30, 0, 0, berlin), berlin)
	status := spring.service.Status()
	if !status.Quiet {
		t.Fatalf("spring status = %+v, want quiet", status)
	}
	end := status.WindowEnd.In(berlin)
	if end.Hour() != 7 || end.Minute() != 0 || end.Day() != 29 {
		t.Fatalf("spring window end = %v, want 29 March 07:00 Berlin", end)
	}

	// Fall back: 25 October 2026 at 01:30 local, before the repeated hour.
	fall := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, time.Date(2026, 10, 25, 1, 30, 0, 0, berlin), berlin)
	status = fall.service.Status()
	end = status.WindowEnd.In(berlin)
	if !status.Quiet || end.Hour() != 7 || end.Minute() != 0 || end.Day() != 25 {
		t.Fatalf("fall status = %+v, end = %v, want 25 October 07:00 Berlin", status, end)
	}
}

func TestTimedDoNotDisturbIsIndependentAndExpires(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: false, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 12, 0), time.UTC)
	until, err := fixture.service.StartDND(context.Background(), 2*time.Hour)
	if err != nil {
		t.Fatalf("StartDND: %v", err)
	}
	if !until.Equal(utc(2026, 11, 12, 14, 0)) {
		t.Fatalf("DND end = %v, want 14:00", until)
	}
	status := fixture.service.Status()
	if !status.Quiet || status.Reason != quiet.ReasonDND {
		t.Fatalf("DND status = %+v, want quiet for do not disturb", status)
	}
	if !status.NextChange.Equal(until) {
		t.Fatalf("next change = %v, want the DND end", status.NextChange)
	}
	fixture.clock.Set(utc(2026, 11, 12, 13, 59))
	if !fixture.service.IsQuiet() {
		t.Fatal("13:59 is not quiet during DND")
	}
	fixture.clock.Set(utc(2026, 11, 12, 14, 0))
	status = fixture.service.Status()
	if status.Quiet || status.Reason != quiet.ReasonOff {
		t.Fatalf("14:00 status = %+v, want DND expired", status)
	}
}

func TestAwakeBypassesOnlyTheCurrentWindow(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)

	before, err := fixture.service.Awake(context.Background())
	if err != nil {
		t.Fatalf("Awake: %v", err)
	}
	if !before.InWindow {
		t.Fatalf("before = %+v, want an active window", before)
	}
	status := fixture.service.Status()
	if status.Quiet || !status.Bypassed || status.Reason != quiet.ReasonAwake {
		t.Fatalf("awake status = %+v, want a bypass", status)
	}
	if !status.AwakeUntil.Equal(utc(2026, 11, 13, 7, 0)) {
		t.Fatalf("awake until = %v, want the current window end", status.AwakeUntil)
	}
	fixture.clock.Set(utc(2026, 11, 13, 6, 0))
	if fixture.service.IsQuiet() {
		t.Fatal("06:00 is quiet after the awake bypass")
	}
	// The next night's window is a new window and holds again.
	fixture.clock.Set(utc(2026, 11, 13, 23, 0))
	if !fixture.service.IsQuiet() {
		t.Fatal("the next night is not quiet")
	}
}

func TestAwakeOutsideAWindowClearsDND(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 12, 0), time.UTC)
	if _, err := fixture.service.StartDND(context.Background(), time.Hour); err != nil {
		t.Fatalf("StartDND: %v", err)
	}
	before, err := fixture.service.Awake(context.Background())
	if err != nil {
		t.Fatalf("Awake: %v", err)
	}
	if before.InWindow || !before.Quiet {
		t.Fatalf("before = %+v, want a DND-only quiet status", before)
	}
	status := fixture.service.Status()
	if status.Quiet {
		t.Fatalf("status = %+v, want awake to clear DND", status)
	}
}

func TestDisableClearsDNDAndBypass(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	if _, err := fixture.service.Awake(context.Background()); err != nil {
		t.Fatalf("Awake: %v", err)
	}
	if _, err := fixture.service.StartDND(context.Background(), time.Hour); err != nil {
		t.Fatalf("StartDND: %v", err)
	}
	if err := fixture.service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	status := fixture.service.Status()
	if status.Quiet || status.Enabled {
		t.Fatalf("status = %+v, want everything off", status)
	}
	for _, key := range []string{quiet.DNDUntilKey, quiet.AwakeUntilKey} {
		if value := fixture.settings.String(key); value != "" {
			t.Fatalf("%s = %q, want cleared", key, value)
		}
	}
}

func TestDashboardWriteAppliesWithoutARestart(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: false, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	if fixture.service.IsQuiet() {
		t.Fatal("quiet is on before the Dashboard write")
	}
	if _, err := fixture.settings.Set(context.Background(), quiet.EnabledKey, true); err != nil {
		t.Fatalf("Dashboard-style Set: %v", err)
	}
	if !fixture.service.IsQuiet() {
		t.Fatal("the enabled override did not apply on the next Status read")
	}
	if _, err := fixture.settings.SetJSON(context.Background(), quiet.StartKey, []byte(`"21:00"`)); err != nil {
		t.Fatalf("SetJSON start: %v", err)
	}
	status := fixture.service.Status()
	if status.Start != "21:00" || !status.InWindow {
		t.Fatalf("status = %+v, want the new start applied live", status)
	}
}

func TestAllowDeliveryMutesNotifications(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	allowed, reason := fixture.service.AllowDelivery()
	if allowed || reason != "quiet hours" {
		t.Fatalf("AllowDelivery = %v, %q, want muted by quiet hours", allowed, reason)
	}
	if err := fixture.service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	allowed, reason = fixture.service.AllowDelivery()
	if !allowed || reason != "" {
		t.Fatalf("AllowDelivery = %v, %q, want delivery outside quiet hours", allowed, reason)
	}
	if _, err := fixture.service.StartDND(context.Background(), time.Hour); err != nil {
		t.Fatalf("StartDND: %v", err)
	}
	allowed, reason = fixture.service.AllowDelivery()
	if allowed || reason != "do not disturb" {
		t.Fatalf("AllowDelivery = %v, %q, want muted by do not disturb", allowed, reason)
	}
}

func TestDefinitionsRejectBadWindowsAndVolume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := settings.New(ctx, st, quiet.Definitions(quiet.DefaultConfig())...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	for key, value := range map[string]any{
		quiet.StartKey:  "25:00",
		quiet.EndKey:    "not a clock",
		quiet.VolumeKey: 101,
	} {
		if _, err := svc.Set(ctx, key, value); err == nil {
			t.Fatalf("Set(%s, %v) succeeded, want a validation error", key, value)
		}
	}
	if err := svc.SetMany(ctx, map[string]any{quiet.StartKey: "08:00", quiet.EndKey: "08:00"}); err != nil {
		t.Fatalf("equal start and end are stored as two valid clocks: %v", err)
	}
	// The settings service validates fields, not pairs; a pair that becomes
	// equal through two writes simply leaves the window inactive rather than
	// holding every Announcement forever. Config.yaml validates the pair.
	service := quiet.New(quiet.Options{Reader: svc, Now: func() time.Time { return utc(2026, 11, 12, 8, 0) }})
	if service.IsQuiet() {
		t.Fatal("an equal start/end pair must not hold Quiet hours")
	}
	if _, err := svc.Set(ctx, quiet.DNDUntilKey, "soon"); err == nil {
		t.Fatal("a non-RFC3339 dnd instant was accepted")
	}
}

func TestParseClockAndHumanClock(t *testing.T) {
	cases := map[string]string{
		"22:30":   "22:30",
		"7":       "07:00",
		"7:05":    "07:05",
		"10 pm":   "22:00",
		"10:30pm": "22:30",
		"12 am":   "00:00",
		"12 pm":   "12:00",
	}
	for input, want := range cases {
		got, ok := quiet.ParseClock(input)
		if !ok || got != want {
			t.Errorf("ParseClock(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "24:00", "12:60", "7:5", "soon"} {
		if _, ok := quiet.ParseClock(input); ok {
			t.Errorf("ParseClock(%q) accepted an invalid clock", input)
		}
	}
	if got := quiet.HumanClock("22:30"); got != "10:30 pm" {
		t.Fatalf("HumanClock(22:30) = %q", got)
	}
	if got := quiet.HumanClock("07:00"); got != "7:00 am" {
		t.Fatalf("HumanClock(07:00) = %q", got)
	}
}

func TestSpeechAndClipUseTheQuietVolumeWithoutTouchingMediaVolume(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	speaker := &gainSpeaker{}
	if err := fixture.service.Speak(context.Background(), speaker, "A reminder."); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if speaker.gain != 35 || speaker.text != "A reminder." {
		t.Fatalf("speaker = %+v, want gain 35", speaker)
	}
	clip := sound.Clip{PCM: pcm16(1000, 1000), Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}}
	scaled := fixture.service.Clip(clip)
	if got := sampleAt(scaled.PCM, 0); got != 350 {
		t.Fatalf("scaled sample = %d, want 350", got)
	}
	// A non-quiet instant leaves the source untouched.
	if err := fixture.service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if got := sampleAt(fixture.service.Clip(clip).PCM, 0); got != 1000 {
		t.Fatalf("outside quiet hours sample = %d, want 1000", got)
	}
	if fixture.service.Gain() != 100 {
		t.Fatal("Gain outside quiet hours is not 100")
	}
}

func TestRingSettingsKeepAlarmsExempt(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	normal := ring.Settings{MaxRing: 5 * time.Minute, Volume: 80, Gap: time.Second, SpeakLabel: ring.SpeakStart}
	if got := fixture.service.RingSettings(ring.Timer, normal); got.Volume != 35 {
		t.Fatalf("Timer volume = %d, want the quiet volume", got.Volume)
	}
	if got := fixture.service.RingSettings(ring.Alarm, normal); got.Volume != 80 {
		t.Fatalf("Alarm volume = %d, want the unchanged Ring volume", got.Volume)
	}
	if got := fixture.service.RingSpeechGain(ring.Timer); got != 35 {
		t.Fatalf("Timer speech gain = %d, want 35", got)
	}
	if got := fixture.service.RingSpeechGain(ring.Alarm); got != 100 {
		t.Fatalf("Alarm speech gain = %d, want 100", got)
	}
}

func TestConcurrentStatusAndWrites(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "00:00", End: "23:59", Volume: 35}, utc(2026, 11, 12, 12, 0), time.UTC)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				_ = fixture.service.Status()
				_ = fixture.service.Gain()
				_, _ = fixture.service.AllowDelivery()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				volume := 30 + (i+n)%50
				if _, err := fixture.settings.Set(ctx, quiet.VolumeKey, volume); err != nil {
					t.Errorf("Set volume: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// gainSpeaker records the software gain attached to the speech context.
type gainSpeaker struct {
	gain int
	text string
}

func (s *gainSpeaker) SayText(ctx context.Context, text string) error {
	s.gain = speech.GainFrom(ctx)
	s.text = text
	return nil
}

// pcm16 builds a mono PCM16 buffer of repeated samples.
func pcm16(samples ...int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, sample := range samples {
		out[i*2] = byte(sample)
		out[i*2+1] = byte(uint16(sample) >> 8)
	}
	return out
}

// sampleAt reads one little-endian PCM16 sample.
func sampleAt(pcm []byte, index int) int16 {
	if index*2+1 >= len(pcm) {
		return 0
	}
	return int16(uint16(pcm[index*2]) | uint16(pcm[index*2+1])<<8)
}

func TestTimezoneSettingChangeReevaluatesTheWindow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	defs := append(quiet.Definitions(quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}), tz.Definition(""))
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New("", nil)
	zone.Bind(svc)
	clock := &fakeClock{now: utc(2026, 11, 12, 14, 0)}
	service := quiet.New(quiet.Options{Reader: svc, Now: clock.Now, Location: zone.Location})
	if service.IsQuiet() {
		t.Fatal("14:00 UTC is quiet before the timezone change")
	}
	if _, err := svc.Set(ctx, tz.Key, "Asia/Tokyo"); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	// 14:00 UTC is 23:00 in Tokyo; the same instant is now inside the window
	// without rebuilding the service or reading a request first.
	if !service.IsQuiet() {
		t.Fatalf("status after timezone change = %+v, want quiet", service.Status())
	}
}
