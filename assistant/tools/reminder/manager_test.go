package reminder_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// traceRecorder is both the speech seam and the Cue-order recorder. One lock
// keeps the Cue and the spoken line in the order the queue played them.
type traceRecorder struct {
	mu     sync.Mutex
	events []string
	gains  []int
	said   chan string
}

func newTraceRecorder() *traceRecorder {
	return &traceRecorder{said: make(chan string, 64)}
}

func (r *traceRecorder) SayText(ctx context.Context, text string) error {
	r.mu.Lock()
	r.events = append(r.events, "say:"+text)
	r.gains = append(r.gains, speech.GainFrom(ctx))
	r.mu.Unlock()
	// The events slice is the complete record; the channel only wakes a test
	// that is waiting for one line and must never block the queue worker.
	select {
	case r.said <- text:
	default:
	}
	return nil
}

func (r *traceRecorder) record(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *traceRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *traceRecorder) lastGain() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.gains) == 0 {
		return -1
	}
	return r.gains[len(r.gains)-1]
}

func (r *traceRecorder) wait(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case text := <-r.said:
		return text
	case <-time.After(timeout):
		t.Fatalf("reminder did not speak within %s; events=%v", timeout, r.snapshot())
		return ""
	}
}

func (r *traceRecorder) expectSilence(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case text := <-r.said:
		t.Fatalf("unexpected reminder speech %q", text)
	case <-time.After(wait):
	}
}

// traceCue is the Cue seam. It records one event and the peak PCM it received,
// so the test can see both order and Quiet-hours scaling.
type traceCue struct {
	rec  *traceRecorder
	clip sound.Clip
	mu   sync.Mutex
	peak int
}

func (c *traceCue) Clip(sound.Role) (sound.Clip, bool) { return c.clip, true }

func (c *traceCue) PlayClip(_ context.Context, clip sound.Clip) error {
	c.rec.record("cue")
	c.mu.Lock()
	c.peak = pcmPeak(clip.PCM)
	c.mu.Unlock()
	return nil
}

func (c *traceCue) peakValue() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

func pcmPeak(pcm []byte) int {
	peak := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		value := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
	}
	return peak
}

// staticSettings is the live settings read seam with fixed values. The Quiet
// service only needs Get; a missing key falls back to the code default.
type staticSettings map[string]any

func (s staticSettings) Get(key string) (settings.Value, bool) {
	value, ok := s[key]
	if !ok {
		return settings.Value{}, false
	}
	return settings.Value{Key: key, Value: value}, true
}

// zoneName is a mutable tz.Reader, so the test can change the live zone.
type zoneName struct {
	mu   sync.Mutex
	name string
}

func (z *zoneName) String(string) string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.name
}

func (z *zoneName) set(name string) {
	z.mu.Lock()
	z.name = name
	z.mu.Unlock()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// TestDurationReminderSpeaksOnceAtTheDueTime is the first acceptance
// criterion at the manager seam: one message, once, and no repeat.
func TestDurationReminderSpeaksOnceAtTheDueTime(t *testing.T) {
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 30*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the reminder message", got)
	}
	rec.expectSilence(t, 200*time.Millisecond)
	if got := manager.List(); len(got) != 0 {
		t.Fatalf("reminders after firing = %+v, want none", got)
	}
}

// TestCuePlaysImmediatelyBeforeTheSpokenMessage keeps the Cue and text in one
// queued item: the order is Cue then message, with no reply slot between.
func TestCuePlaysImmediatelyBeforeTheSpokenMessage(t *testing.T) {
	rec := newTraceRecorder()
	cue := &traceCue{
		rec:  rec,
		clip: sound.Clip{PCM: []byte{0xE8, 0x03, 0xE8, 0x03}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}, Duration: time.Millisecond},
	}
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Cues: cue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	rec.wait(t, 2*time.Second)
	deadline := time.Now().Add(time.Second)
	for len(rec.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := rec.snapshot()
	if len(got) != 2 || got[0] != "cue" || got[1] != "say:check the oven" {
		t.Fatalf("playback order = %v, want cue then the spoken message", got)
	}
}

// TestDueMidReplyWaitsForTheGap is the third acceptance criterion: a Reminder
// due while a reply is playing waits until the turn ends.
func TestDueMidReplyWaitsForTheGap(t *testing.T) {
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	queue.TurnStarted()
	if _, err := manager.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	rec.expectSilence(t, 150*time.Millisecond)

	queue.TurnFinished()
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the reminder after the turn", got)
	}
}

// TestDueWhileConfirmationPendingWaits keeps the Confirmation priority ahead
// of a Reminder.
func TestDueWhileConfirmationPendingWaits(t *testing.T) {
	rec := newTraceRecorder()
	var blocked atomic.Bool
	blocked.Store(true)
	queue := announce.New(rec, testLogger(), announce.WithBlocked(func() bool { return blocked.Load() }))
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	rec.expectSilence(t, 150*time.Millisecond)
	blocked.Store(false)
	if got := rec.wait(t, 3*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the reminder after the Confirmation cleared", got)
	}
}

// TestClockReminderUsesTheLiveTimezone proves the IANA read is per scheduling
// call and that 5 pm is interpreted in the effective zone.
func TestClockReminderUsesTheLiveTimezone(t *testing.T) {
	zone := tz.New("", testLogger())
	reader := &zoneName{name: "Europe/Berlin"}
	zone.Bind(reader)
	now := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC) // 16:04 in Berlin
	manager := reminder.NewManager(reminder.Options{
		Now: func() time.Time { return now }, Zone: zone, Logger: testLogger(),
	})
	t.Cleanup(manager.Close)

	first, err := manager.StartClock("call mum", 17, 0, false)
	if err != nil {
		t.Fatalf("StartClock Berlin: %v", err)
	}
	if want := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC); !first.FiresAt.Equal(want) {
		t.Fatalf("Berlin 5 pm = %s, want %s", first.FiresAt, want)
	}
	if first.When != "at 5:00 pm" {
		t.Fatalf("When = %q, want the Berlin clock", first.When)
	}

	// The same live service now reports New York: 17:00 there is 22:00 UTC.
	reader.set("America/New_York")
	second, err := manager.StartClock("water the plants", 17, 0, false)
	if err != nil {
		t.Fatalf("StartClock New York: %v", err)
	}
	if want := time.Date(2026, 11, 11, 22, 0, 0, 0, time.UTC); !second.FiresAt.Equal(want) {
		t.Fatalf("New York 5 pm = %s, want %s", second.FiresAt, want)
	}
	manager.Cancel("")
}

// TestClockTimeAlreadyPassedSchedulesTomorrow pins the future-if-past policy
// shared with Alarms, including the exact boundary.
func TestClockTimeAlreadyPassedSchedulesTomorrow(t *testing.T) {
	zone := tz.New("Europe/Berlin", testLogger())
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC) // exactly 17:00 Berlin
	manager := reminder.NewManager(reminder.Options{
		Now: func() time.Time { return now }, Zone: zone, Logger: testLogger(),
	})
	t.Cleanup(manager.Close)

	r, err := manager.StartClock("call mum", 17, 0, false)
	if err != nil {
		t.Fatalf("StartClock: %v", err)
	}
	if want := time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC); !r.FiresAt.Equal(want) {
		t.Fatalf("exactly-now clock = %s, want tomorrow %s", r.FiresAt, want)
	}
	if r.When != "tomorrow at 5:00 pm" {
		t.Fatalf("When = %q, want tomorrow", r.When)
	}

	tomorrow, err := manager.StartClock("take the bins out", 9, 0, true)
	if err != nil {
		t.Fatalf("StartClock tomorrow: %v", err)
	}
	if want := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC); !tomorrow.FiresAt.Equal(want) {
		t.Fatalf("tomorrow 9 am = %s, want %s", tomorrow.FiresAt, want)
	}
}

// TestQuietHoursReminderSoundsAtQuietVolumeWhileOrdinaryAnnouncementsHold is
// the #61 integration: the Reminder bypasses the hold, uses quiet.volume for
// the Cue and the message, and the ordinary Announcement stays held.
func TestQuietHoursReminderSoundsAtQuietVolumeWhileOrdinaryAnnouncementsHold(t *testing.T) {
	rec := newTraceRecorder()
	cue := &traceCue{
		rec:  rec,
		clip: sound.Clip{PCM: []byte{0xE8, 0x03, 0xE8, 0x03}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}, Duration: time.Millisecond},
	}
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	quietSvc := quiet.New(quiet.Options{
		Reader: staticSettings{
			quiet.EnabledKey: true,
			quiet.StartKey:   "22:30",
			quiet.EndKey:     "07:00",
			quiet.VolumeKey:  20,
		},
		Now:      func() time.Time { return now },
		Location: func() *time.Location { return time.UTC },
	})
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{
		Now: func() time.Time { return now }, Speaker: rec, Queue: queue, Cues: cue, Quiet: quietSvc, Logger: testLogger(),
	})
	t.Cleanup(manager.Close)

	queue.Hold() // what the Quiet controller does while the window is active
	queue.Enqueue("The bins go out tonight.")
	if _, err := manager.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the reminder while quiet", got)
	}
	if peak := cue.peakValue(); peak < 180 || peak > 220 {
		t.Fatalf("quiet Cue peak = %d, want about 20%% of 1000", peak)
	}
	if gain := rec.lastGain(); gain != 20 {
		t.Fatalf("speech gain = %d, want quiet.volume 20", gain)
	}
	rec.expectSilence(t, 150*time.Millisecond)

	queue.Release()
	if got := rec.wait(t, 2*time.Second); got != "The bins go out tonight." {
		t.Fatalf("ordinary announcement = %q, want it only after Release", got)
	}
}

// TestQuietReminderStillWaitsForTheTurn keeps the priority order inside the
// urgent path: the hold is bypassed, the turn is not.
func TestQuietReminderStillWaitsForTheTurn(t *testing.T) {
	rec := newTraceRecorder()
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	quietSvc := quiet.New(quiet.Options{
		Reader: staticSettings{
			quiet.EnabledKey: true, quiet.StartKey: "22:30", quiet.EndKey: "07:00", quiet.VolumeKey: 20,
		},
		Now:      func() time.Time { return now },
		Location: func() *time.Location { return time.UTC },
	})
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{
		Now: func() time.Time { return now }, Speaker: rec, Queue: queue, Quiet: quietSvc, Logger: testLogger(),
	})
	t.Cleanup(manager.Close)

	queue.Hold()
	queue.TurnStarted()
	if _, err := manager.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	rec.expectSilence(t, 150*time.Millisecond)
	queue.TurnFinished()
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the reminder after the turn", got)
	}
	if gain := rec.lastGain(); gain != 20 {
		t.Fatalf("speech gain = %d, want quiet.volume 20", gain)
	}
}

// TestSetToolReadsTheUtteranceAndSpeaksOnce runs the real Tool contract:
// Extract reads the duration and message, Execute schedules, Speak confirms.
func TestSetToolReadsTheUtteranceAndSpeaksOnce(t *testing.T) {
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)
	registry := tool.NewRegistry(2 * time.Second)
	if err := reminder.Register(registry, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	args, ok := registry.Extract(reminder.SetName, "remind me in 1 second to check the oven")
	if !ok {
		t.Fatal("Extract refused a real reminder utterance")
	}
	var in reminder.SetIn
	if err := json.Unmarshal(args, &in); err != nil {
		t.Fatalf("unmarshal extracted args: %v", err)
	}
	if in.Message != "check the oven" || in.DurationSeconds != 1 {
		t.Fatalf("extracted = %+v, want check the oven in 1 second", in)
	}
	result, err := registry.Execute(context.Background(), reminder.SetName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text, ok := registry.Speak(reminder.SetName, result)
	if !ok || !strings.Contains(text, "check the oven") || !strings.Contains(text, "in 1 second") {
		t.Fatalf("Speak = %q, %v; want the confirmation", text, ok)
	}
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the message at the due time", got)
	}
}

// TestListAndCancelToolsShareTheManager covers the read and cancel Tools at
// the registry seam, including the spoken list and a message query.
func TestListAndCancelToolsShareTheManager(t *testing.T) {
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)
	registry := tool.NewRegistry(2 * time.Second)
	if err := reminder.Register(registry, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := manager.StartDuration("check the oven", time.Hour); err != nil {
		t.Fatalf("StartDuration oven: %v", err)
	}
	if _, err := manager.StartDuration("call mum", 2*time.Hour); err != nil {
		t.Fatalf("StartDuration mum: %v", err)
	}

	list, err := registry.Execute(context.Background(), reminder.ListName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list Execute: %v", err)
	}
	if text, ok := registry.Speak(reminder.ListName, list); !ok || !strings.Contains(text, "2 reminders") {
		t.Fatalf("list Speak = %q, %v", text, ok)
	}

	args, ok := registry.Extract(reminder.CancelName, "cancel the oven reminder")
	if !ok {
		t.Fatal("cancel Extract refused a real phrase")
	}
	cancelled, err := registry.Execute(context.Background(), reminder.CancelName, args)
	if err != nil {
		t.Fatalf("cancel Execute: %v", err)
	}
	if text, ok := registry.Speak(reminder.CancelName, cancelled); !ok || text != "Reminder cancelled." {
		t.Fatalf("cancel Speak = %q, %v", text, ok)
	}
	if got := manager.List(); len(got) != 1 || got[0].Message != "call mum" {
		t.Fatalf("reminders after cancel = %+v", got)
	}
}

// TestToolPrivacyMatchesTheTicket pins the Local-only decision: list is
// LocalOnlyYes, while set and cancel serve the Agent path.
func TestToolPrivacyMatchesTheTicket(t *testing.T) {
	registry := tool.NewRegistry(time.Second)
	if err := reminder.Register(registry, reminder.NewManager(reminder.Options{Logger: testLogger()})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for name, want := range map[string]bool{
		reminder.SetName:    false,
		reminder.CancelName: false,
		reminder.ListName:   true,
	} {
		info, ok := registry.Info(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if info.LocalOnly != want {
			t.Errorf("%s LocalOnly = %v, want %v", name, info.LocalOnly, want)
		}
	}
}

// TestLoggerNeverContainsTheReminderMessage keeps personal content out of the
// structured logs on set, list, fire and cancel.
func TestLoggerNeverContainsTheReminderMessage(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rec := newTraceRecorder()
	queue := announce.New(rec, logger)
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: logger})
	t.Cleanup(manager.Close)

	const message = "buy a birthday present for Sam"
	if _, err := manager.StartDuration(message, 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	_ = manager.List()
	rec.wait(t, 2*time.Second)
	// A cancel query can be a piece of the message; it must not be logged.
	if _, err := manager.StartDuration(message, time.Hour); err != nil {
		t.Fatalf("StartDuration second: %v", err)
	}
	if _, err := manager.CancelQuery("birthday"); err != nil {
		t.Fatalf("CancelQuery: %v", err)
	}
	// Let the queue finish its own log line before the buffer is read.
	queue.Close()
	if output := logs.String(); strings.Contains(output, message) || strings.Contains(output, "birthday") || strings.Contains(output, "Sam") {
		t.Fatalf("logs leaked the reminder message:\n%s", output)
	}
}

// lockedBuffer makes the slog output safe to read while the queue worker is
// still writing a trailing line.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
