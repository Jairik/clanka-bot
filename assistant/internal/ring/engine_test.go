package ring_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/playback"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

// fakeClock is the engine's deterministic time source. Sleep jumps the clock
// forward, so max_ring and gap are exercised without wall-clock sleeps.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

// newFakeClock starts at the real clock's value so bounded contexts have a
// future real-time deadline while the loop's Now still advances deterministically.
func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// recordingPlayer records every rendered pass. When block is true it holds
// until the Ring context ends, which is how the stop tests observe a live Ring.
type recordingPlayer struct {
	mu      sync.Mutex
	clips   []sound.Clip
	started chan struct{}
	once    sync.Once
	block   bool
}

func newRecordingPlayer(block bool) *recordingPlayer {
	return &recordingPlayer{started: make(chan struct{}), block: block}
}

func (p *recordingPlayer) Play(ctx context.Context, clip sound.Clip) error {
	p.mu.Lock()
	p.clips = append(p.clips, clip)
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (p *recordingPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clips)
}

func (p *recordingPlayer) first() sound.Clip {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clips) == 0 {
		return sound.Clip{}
	}
	return p.clips[0]
}

// recordingSpeaker records label and fallback speech in order.
type recordingSpeaker struct {
	mu     sync.Mutex
	texts  []string
	notify chan string
}

func newRecordingSpeaker() *recordingSpeaker {
	return &recordingSpeaker{notify: make(chan string, 8)}
}

func (s *recordingSpeaker) SayText(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.texts = append(s.texts, text)
	s.mu.Unlock()
	select {
	case s.notify <- text:
	default:
	}
	return nil
}

func (s *recordingSpeaker) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// fakeMusic records the duck/restore order.
type fakeMusic struct {
	mu      sync.Mutex
	events  []string
	restore bool
}

func (m *fakeMusic) Duck(context.Context) (func(), error) {
	m.mu.Lock()
	m.events = append(m.events, "duck")
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.events = append(m.events, "restore")
		m.restore = true
		m.mu.Unlock()
	}, nil
}

func (m *fakeMusic) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

// gatedBoundary lets a test hold the Ring at the reply-sentence seam.
type gatedBoundary struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedBoundary() *gatedBoundary {
	return &gatedBoundary{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *gatedBoundary) WaitForReplySentence(ctx context.Context) (func(), error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return func() {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tone builds a short mono PCM16 clip.
func tone(rate, samples int, amplitude int16) sound.Clip {
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(amplitude))
	}
	return sound.Clip{
		PCM:      pcm,
		Format:   audio.Format{Rate: rate, Width: 2, Channels: 1},
		Duration: time.Duration(samples) * time.Second / time.Duration(rate),
	}
}

// engine builds an engine over fixed settings and a recording clip seam.
func engineFor(settings ring.Settings, player *recordingPlayer, opts ...func(*ring.Deps)) *ring.Engine {
	deps := ring.Deps{
		Settings: func(ring.Kind) ring.Settings { return settings },
		Clip:     func(ring.Kind) (sound.Clip, bool) { return tone(1000, 4, 10000), true },
		Play:     player.Play,
		Clock:    newFakeClock(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(&deps)
	}
	return ring.New(deps)
}

// TestVolumeScalesTheCueAbsolutely proves the Ring volume is applied to the
// PCM, independent of the media volume.
func TestVolumeScalesTheCueAbsolutely(t *testing.T) {
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 50, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player)
	result := engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, Label: "pasta"})
	if result.Outcome != ring.OutcomeMissed {
		t.Fatalf("outcome = %q, want missed after max_ring", result.Outcome)
	}
	if player.count() < 1 {
		t.Fatal("no pass was played")
	}
	got := int16(binary.LittleEndian.Uint16(player.first().PCM))
	if got != 5000 {
		t.Fatalf("first sample = %d, want 5000 at 50%% volume", got)
	}
}

// TestRampRisesFromSilenceAcrossThePass proves ramp is measured from the Ring
// start and scales the samples within one pass.
func TestRampRisesFromSilenceAcrossThePass(t *testing.T) {
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 100, Ramp: 10 * time.Millisecond,
		Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) {
		deps.Clip = func(ring.Kind) (sound.Clip, bool) { return tone(1000, 10, 10000), true }
	})
	engine.Ring(context.Background(), ring.Request{Kind: ring.Timer})
	clip := player.first()
	if len(clip.PCM) != 20 {
		t.Fatalf("first pass PCM = %d bytes, want 20", len(clip.PCM))
	}
	first := int16(binary.LittleEndian.Uint16(clip.PCM[0:]))
	last := int16(binary.LittleEndian.Uint16(clip.PCM[18:]))
	if first != 0 {
		t.Fatalf("first ramp sample = %d, want silence", first)
	}
	if last < 8000 || last > 10000 {
		t.Fatalf("last ramp sample = %d, want near full volume", last)
	}
}

// TestPerAlertClipVolumeAndRampOverrideOnlyThatRing proves a Request's Sound,
// Volume and Ramp are applied to one Ring without changing the kind settings
// used by the next Ring.
func TestPerAlertClipVolumeAndRampOverrideOnlyThatRing(t *testing.T) {
	player := newRecordingPlayer(false)
	kind := tone(1000, 12, 20000)
	birds := tone(1000, 8, 10000)
	engine := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 80, Ramp: 30 * time.Second,
		Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) {
		deps.Clip = func(ring.Kind) (sound.Clip, bool) { return kind, true }
		deps.ClipName = func(name string) (sound.Clip, bool) {
			if name == "birds" {
				return birds, true
			}
			return sound.Clip{}, false
		}
	})

	volume := 40
	ramp := time.Duration(0)
	engine.Ring(context.Background(), ring.Request{
		Kind: ring.Alarm, Sound: "birds", Volume: &volume, Ramp: &ramp,
	})
	first := player.first()
	if len(first.PCM) != len(birds.PCM) {
		t.Fatalf("overridden pass has %d bytes, want the birds clip %d", len(first.PCM), len(birds.PCM))
	}
	if got := int16(binary.LittleEndian.Uint16(first.PCM)); got != 4000 {
		t.Fatalf("overridden first sample = %d, want 4000 at 40%% with no ramp", got)
	}

	// The next Ring has no per-alert overrides: it must use the kind clip and
	// the kind 30 s ramp again, proving the override was local.
	player2 := newRecordingPlayer(false)
	plain := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 80, Ramp: 30 * time.Second,
		Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player2, func(deps *ring.Deps) {
		deps.Clip = func(ring.Kind) (sound.Clip, bool) { return kind, true }
		deps.ClipName = func(string) (sound.Clip, bool) {
			t.Fatal("ClipName called for a Ring without a per-alert sound")
			return sound.Clip{}, false
		}
	})
	plain.Ring(context.Background(), ring.Request{Kind: ring.Alarm})
	if got := player2.first(); len(got.PCM) != len(kind.PCM) {
		t.Fatalf("plain pass has %d bytes, want the kind clip %d", len(got.PCM), len(kind.PCM))
	}
	if got := int16(binary.LittleEndian.Uint16(player2.first().PCM)); got != 0 {
		t.Fatalf("plain first sample = %d, want the 30 s ramp to start at silence", got)
	}
}

// TestPerAlertVolumeZeroMutesAndMissingSoundFallsBack covers the two edge
// cases a stored value can have: an explicit mute, and a named sound that has
// disappeared from the library.
func TestPerAlertVolumeZeroMutesAndMissingSoundFallsBack(t *testing.T) {
	kind := tone(1000, 8, 20000)
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) {
		deps.Clip = func(ring.Kind) (sound.Clip, bool) { return kind, true }
		deps.ClipName = func(string) (sound.Clip, bool) { return sound.Clip{}, false }
	})
	zero := 0
	engine.Ring(context.Background(), ring.Request{Kind: ring.Alarm, Volume: &zero})
	if got := peakSample(player.first().PCM); got != 0 {
		t.Fatalf("explicit 0 volume peak = %d, want silence", got)
	}

	// A missing named sound keeps the kind clip rather than substituting
	// another entry.
	player2 := newRecordingPlayer(false)
	engine2 := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player2, func(deps *ring.Deps) {
		deps.Clip = func(ring.Kind) (sound.Clip, bool) { return kind, true }
		deps.ClipName = func(string) (sound.Clip, bool) { return sound.Clip{}, false }
	})
	engine2.Ring(context.Background(), ring.Request{Kind: ring.Alarm, Sound: "gone"})
	if got := player2.first(); len(got.PCM) != len(kind.PCM) {
		t.Fatalf("missing-sound pass has %d bytes, want the kind clip %d", len(got.PCM), len(kind.PCM))
	}
}

// peakSample returns the largest absolute PCM16 sample.
func peakSample(pcm []byte) int16 {
	var peak int16
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	return peak
}

// TestSpeakLabelModes covers start, every_loop and never.
func TestSpeakLabelModes(t *testing.T) {
	for _, test := range []struct {
		name string
		mode ring.SpeakLabel
		want int
	}{
		{name: "start", mode: ring.SpeakStart, want: 1},
		{name: "every loop", mode: ring.SpeakEveryLoop, want: 2},
		{name: "never", mode: ring.SpeakNever, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			speaker := newRecordingSpeaker()
			player := newRecordingPlayer(false)
			engine := engineFor(ring.Settings{
				MaxRing: 2 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: test.mode,
			}, player, func(deps *ring.Deps) { deps.Speaker = speaker })
			engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, Label: "pasta"})
			texts := speaker.all()
			if len(texts) != test.want {
				t.Fatalf("label speech = %q, want %d", texts, test.want)
			}
			for _, text := range texts {
				if text != "Pasta timer." {
					t.Fatalf("label text = %q", text)
				}
			}
		})
	}
}

// TestGapIsSilenceBetweenPasses proves the gap setting is the actual sleep
// between loop passes.
func TestGapIsSilenceBetweenPasses(t *testing.T) {
	clock := newFakeClock()
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 4 * time.Millisecond, Volume: 100, Gap: 2 * time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Clock = clock })
	engine.Ring(context.Background(), ring.Request{Kind: ring.Timer})
	sleeps := clock.slept()
	if len(sleeps) < 2 || sleeps[0] != 2*time.Millisecond || sleeps[1] != 2*time.Millisecond {
		t.Fatalf("gap sleeps = %v, want two 2ms gaps", sleeps)
	}
	if player.count() != 2 {
		t.Fatalf("passes = %d, want 2", player.count())
	}
}

// TestLiveSettingsChangeTheNextRing proves the Dashboard/voice write applies
// on the next Ring through the same settings service main uses.
func TestLiveSettingsChangeTheNextRing(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t)
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "max_ring"), []byte(`"5ms"`)); err != nil {
		t.Fatalf("set max_ring: %v", err)
	}
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "gap"), []byte(`"1ms"`)); err != nil {
		t.Fatalf("set gap: %v", err)
	}
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "speak_label"), []byte(`"never"`)); err != nil {
		t.Fatalf("set speak_label: %v", err)
	}
	run := func(volume int) sound.Clip {
		t.Helper()
		if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "volume"), []byte(fmt.Sprintf("%d", volume))); err != nil {
			t.Fatalf("set volume: %v", err)
		}
		player := newRecordingPlayer(false)
		engine := engineFor(ring.Settings{}, player, func(deps *ring.Deps) {
			deps.Settings = func(kind ring.Kind) ring.Settings { return ring.ReadSettings(svc, kind) }
		})
		engine.Ring(ctx, ring.Request{Kind: ring.Timer})
		return player.first()
	}
	first := int16(binary.LittleEndian.Uint16(run(50).PCM))
	if first != 5000 {
		t.Fatalf("first sample at 50%% = %d, want 5000", first)
	}
	loud := int16(binary.LittleEndian.Uint16(run(100).PCM))
	if loud != 10000 {
		t.Fatalf("first sample at 100%% = %d, want 10000", loud)
	}
}

// TestStopEndsTheRingAndRestoresMusic is the stop/music contract: the Focus
// holder cancels the active pass, the result names the intent, and restore runs.
func TestStopEndsTheRingAndRestoresMusic(t *testing.T) {
	var registry focus.Registry
	player := newRecordingPlayer(true)
	music := &fakeMusic{}
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) {
		deps.Focus = &registry
		deps.Music = music
	})
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, Label: "pasta"}) }()
	select {
	case <-player.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring never played")
	}
	if got := music.all(); len(got) != 1 || got[0] != "duck" {
		t.Fatalf("music before stop = %v, want duck", got)
	}
	text, handled := registry.HandleIntent(context.Background(), focus.Stop)
	if !handled || text != "Timer stopped." {
		t.Fatalf("stop = %q, %v", text, handled)
	}
	result := <-done
	if result.Outcome != ring.OutcomeStopped || result.EndedBy != "stop" {
		t.Fatalf("result = %+v, want stopped/stop", result)
	}
	if got := music.all(); len(got) != 2 || got[1] != "restore" || !music.restore {
		t.Fatalf("music after stop = %v, want restore", got)
	}
}

// TestMissedRingRestoresMusic proves the duck is restored when max_ring ends
// the Ring, not only on an explicit stop.
func TestMissedRingRestoresMusic(t *testing.T) {
	player := newRecordingPlayer(false)
	music := &fakeMusic{}
	engine := engineFor(ring.Settings{
		MaxRing: 2 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Music = music })
	result := engine.Ring(context.Background(), ring.Request{Kind: ring.Timer})
	if result.Outcome != ring.OutcomeMissed {
		t.Fatalf("result = %+v, want missed", result)
	}
	events := music.all()
	if len(events) != 2 || events[0] != "duck" || events[1] != "restore" {
		t.Fatalf("music events = %v, want duck then restore", events)
	}
}

// TestSnoozeOnTimerActsAsStop covers the Timer-specific snooze rule.
func TestSnoozeOnTimerActsAsStop(t *testing.T) {
	var registry focus.Registry
	player := newRecordingPlayer(true)
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Focus = &registry })
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, Label: "tea"}) }()
	<-player.started
	text, handled := registry.HandleIntent(context.Background(), focus.Snooze)
	if !handled || text != "Timer stopped." {
		t.Fatalf("snooze = %q, %v; want it to act as stop", text, handled)
	}
	result := <-done
	if result.Outcome != ring.OutcomeStopped || result.EndedBy != "snooze" {
		t.Fatalf("result = %+v, want stopped/snooze", result)
	}
}

// TestAlarmSnoozeSeam defers the re-ring to #53 but proves the engine can end
// an Alarm with the snooze outcome and text when the Alarm opts in.
func TestAlarmSnoozeSeam(t *testing.T) {
	var registry focus.Registry
	player := newRecordingPlayer(true)
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Focus = &registry })
	done := make(chan ring.Result, 1)
	go func() {
		done <- engine.Ring(context.Background(), ring.Request{
			Kind: ring.Alarm, Label: "wake", AllowSnooze: true,
		})
	}()
	<-player.started
	text, handled := registry.HandleIntent(context.Background(), focus.Snooze)
	if !handled || text != "Alarm snoozed." {
		t.Fatalf("alarm snooze = %q, %v", text, handled)
	}
	result := <-done
	if result.Outcome != ring.OutcomeStopped || result.EndedBy != "snooze" {
		t.Fatalf("result = %+v, want stopped/snooze for #53 to schedule", result)
	}
}

// TestAlarmWithoutTheOptInDeclinesSnooze proves #52 does not silently ship
// Alarm snooze behaviour before #53 owns its re-ring schedule.
func TestAlarmWithoutTheOptInDeclinesSnooze(t *testing.T) {
	var registry focus.Registry
	player := newRecordingPlayer(true)
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Focus = &registry })
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Alarm}) }()
	<-player.started
	if _, handled := registry.HandleIntent(context.Background(), focus.Snooze); handled {
		t.Fatal("an Alarm without the #53 opt-in handled snooze")
	}
	if _, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop did not reach the Alarm")
	}
	if result := <-done; result.EndedBy != "stop" {
		t.Fatalf("result = %+v", result)
	}
}

// TestRingWaitsForTheReplySentence proves the Ring does not start before the
// sentence ends, and a stop while waiting never sounds.
func TestRingWaitsForTheReplySentence(t *testing.T) {
	boundary := newGatedBoundary()
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Boundary = boundary })
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Timer}) }()
	select {
	case <-boundary.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring never asked for the boundary")
	}
	select {
	case <-player.started:
		t.Fatal("the Ring played before the sentence ended")
	case <-time.After(30 * time.Millisecond):
	}
	close(boundary.release)
	result := <-done
	if result.Outcome != ring.OutcomeMissed && result.Outcome != ring.OutcomeStopped {
		t.Fatalf("result = %+v", result)
	}
	if player.count() == 0 {
		t.Fatal("the Ring never played after the sentence ended")
	}
}

// TestStopWhileWaitingNeverSounds covers the stop racing the sentence wait.
func TestStopWhileWaitingNeverSounds(t *testing.T) {
	var registry focus.Registry
	boundary := newGatedBoundary()
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) {
		deps.Boundary = boundary
		deps.Focus = &registry
	})
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Timer}) }()
	<-boundary.started
	if _, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop did not reach the waiting Ring")
	}
	result := <-done
	if result.Outcome != ring.OutcomeStopped || result.EndedBy != "stop" {
		t.Fatalf("result = %+v", result)
	}
	if player.count() != 0 {
		t.Fatalf("played %d passes after stop while waiting", player.count())
	}
}

// TestMissingSoundSpeaksTheFallback keeps a none/missing Cue as a single
// spoken line under the same Focus and boundary lifecycle.
func TestMissingSoundSpeaksTheFallback(t *testing.T) {
	speaker := newRecordingSpeaker()
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakStart,
	}, player, func(deps *ring.Deps) {
		deps.Speaker = speaker
		deps.Clip = func(ring.Kind) (sound.Clip, bool) { return sound.Clip{}, false }
	})
	result := engine.Ring(context.Background(), ring.Request{
		Kind: ring.Timer, Label: "pasta", FallbackText: "Pasta timer is done.",
	})
	if !result.Played || result.EndedBy != "spoken" {
		t.Fatalf("result = %+v, want spoken fallback", result)
	}
	if got := speaker.all(); len(got) != 1 || got[0] != "Pasta timer is done." {
		t.Fatalf("fallback speech = %q", got)
	}
	if player.count() != 0 {
		t.Fatal("fallback still played the clip")
	}
}

// TestQueuedRingsStayControllable is the serialisation contract: the second
// Ring waits, does not claim Focus early, and rings after the first stops.
func TestQueuedRingsStayControllable(t *testing.T) {
	var registry focus.Registry
	player := newRecordingPlayer(true)
	starts := make(chan string, 4)
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Focus = &registry })
	done := make(chan ring.Result, 2)
	run := func(label string) {
		go func() {
			done <- engine.Ring(context.Background(), ring.Request{
				Kind: ring.Timer, Label: label,
				OnStart: func() { starts <- label },
			})
		}()
	}
	run("first")
	select {
	case label := <-starts:
		if label != "first" {
			t.Fatalf("first start = %q", label)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first Ring never started")
	}
	<-player.started
	run("second")
	select {
	case label := <-starts:
		t.Fatalf("%q started while the first Ring still owned the speaker", label)
	case <-time.After(50 * time.Millisecond):
	}
	if _, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop did not reach the active Ring")
	}
	first := <-done
	if first.EndedBy != "stop" {
		t.Fatalf("first result = %+v", first)
	}
	select {
	case label := <-starts:
		if label != "second" {
			t.Fatalf("next start = %q", label)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the queued Ring never started")
	}
	if _, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop did not reach the second Ring")
	}
	second := <-done
	if second.EndedBy != "stop" {
		t.Fatalf("second result = %+v", second)
	}
}

// TestInterruptedOnShutdown keeps a parent cancel distinct from a user stop.
func TestInterruptedOnShutdown(t *testing.T) {
	player := newRecordingPlayer(true)
	engine := engineFor(ring.Settings{
		MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(ctx, ring.Request{Kind: ring.Timer}) }()
	<-player.started
	cancel()
	result := <-done
	if result.Outcome != ring.OutcomeInterrupted || result.EndedBy != "shutdown" {
		t.Fatalf("result = %+v, want interrupted/shutdown", result)
	}
}

// TestGateIntegrationUsesTheRealSentenceSeam proves the engine works against
// playback.Gate, not only a fake boundary.
func TestGateIntegrationUsesTheRealSentenceSeam(t *testing.T) {
	gate := playback.NewGate()
	doneSentence, err := gate.BeginReply(context.Background())
	if err != nil {
		t.Fatalf("BeginReply: %v", err)
	}
	player := newRecordingPlayer(false)
	engine := engineFor(ring.Settings{
		MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, player, func(deps *ring.Deps) { deps.Boundary = gate })
	done := make(chan ring.Result, 1)
	go func() { done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Timer}) }()
	time.Sleep(20 * time.Millisecond)
	if player.count() != 0 {
		t.Fatal("the Ring played while the reply sentence was active")
	}
	doneSentence(true)
	<-done
	if player.count() == 0 {
		t.Fatal("the Ring never played after the sentence ended")
	}
}
