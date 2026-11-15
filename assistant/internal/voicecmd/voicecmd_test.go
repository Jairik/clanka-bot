package voicecmd_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/voicecmd"
)

// fakeSink records every volume read and write, and can fail either one.
type fakeSink struct {
	mu      sync.Mutex
	percent int
	reads   int
	sets    []int
	readErr error
	setErr  error
}

func (s *fakeSink) Volume(context.Context) (audio.VolumeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.readErr != nil {
		return audio.VolumeState{}, s.readErr
	}
	return audio.VolumeState{Percent: s.percent}, nil
}

func (s *fakeSink) SetVolume(_ context.Context, percent int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.percent = percent
	s.sets = append(s.sets, percent)
	return nil
}

func (s *fakeSink) state() (int, int, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.percent, s.reads, append([]int(nil), s.sets...)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type focusState struct {
	mu    sync.Mutex
	reply bool
	music bool
}

func (f *focusState) set(reply, music bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply, f.music = reply, music
}

func (f *focusState) get() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reply, f.music
}

type fixture struct {
	handler *voicecmd.Handler
	sink    *fakeSink
	clock   *clock
	focus   *focusState
	cues    int
}

func newFixture(t *testing.T, step int, cooldown time.Duration, percent int, reply, music bool) *fixture {
	t.Helper()
	f := &fixture{
		sink:  &fakeSink{percent: percent},
		clock: &clock{now: time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)},
		focus: &focusState{reply: reply, music: music},
	}
	handler, err := voicecmd.New(
		voicecmd.Config{Step: step, Cooldown: cooldown},
		voicecmd.Deps{
			Sink:      f.sink,
			Focus:     f.focus.get,
			MaxVolume: func() int { return 0 },
			Cue: func(context.Context) {
				f.cues++
			},
			Now: f.clock.Now,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.handler = handler
	return f
}

// handle dispatches one detection and joins the command goroutine, so the
// assertions that follow see the volume step it performed. Handle itself
// returns before any of that work starts (#209).
func (f *fixture) handle(name string) {
	f.handler.Handle(context.Background(), name)
	f.handler.Wait()
}

func TestStepsOnlyWhileFocusIsHeld(t *testing.T) {
	tests := []struct {
		name     string
		reply    bool
		music    bool
		model    string
		start    int
		want     int
		wantSets int
		wantCues int
	}{
		{name: "reply focus up", reply: true, model: voicecmd.VolumeUpModel, start: 40, want: 50, wantSets: 1, wantCues: 1},
		{name: "music focus down", music: true, model: voicecmd.VolumeDownModel, start: 40, want: 30, wantSets: 1, wantCues: 1},
		{name: "both hold focus", reply: true, music: true, model: voicecmd.VolumeUpModel, start: 40, want: 50, wantSets: 1, wantCues: 1},
		{name: "nothing holds focus", model: voicecmd.VolumeUpModel, start: 40, want: 40, wantSets: 0, wantCues: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, 10, 1500*time.Millisecond, test.start, test.reply, test.music)
			f.handle(test.model)

			got, _, sets := f.sink.state()
			if got != test.want {
				t.Fatalf("volume = %d, want %d", got, test.want)
			}
			if len(sets) != test.wantSets {
				t.Fatalf("set calls = %v, want %d", sets, test.wantSets)
			}
			if f.cues != test.wantCues {
				t.Fatalf("cues = %d, want %d", f.cues, test.wantCues)
			}
		})
	}
}

func TestCooldownIgnoresASecondDetection(t *testing.T) {
	f := newFixture(t, 10, 1500*time.Millisecond, 40, true, false)

	f.handle(voicecmd.VolumeUpModel)
	f.clock.Advance(time.Second)
	f.handle(voicecmd.VolumeUpModel)

	if got, _, sets := f.sink.state(); got != 50 || len(sets) != 1 {
		t.Fatalf("volume/sets = %d/%v after 1s, want 50 and one step", got, sets)
	}

	f.clock.Advance(500 * time.Millisecond)
	f.handle(voicecmd.VolumeUpModel)
	if got, _, sets := f.sink.state(); got != 60 || len(sets) != 2 {
		t.Fatalf("volume/sets = %d/%v after the cooldown, want 60 and two steps", got, sets)
	}
}

func TestOneStepPerDetection(t *testing.T) {
	f := newFixture(t, 10, time.Second, 43, true, false)
	f.handle(voicecmd.VolumeUpModel)

	got, reads, sets := f.sink.state()
	if got != 53 {
		t.Fatalf("volume = %d, want exactly one 10%% step to 53", got)
	}
	if reads != 1 || len(sets) != 1 || sets[0] != 53 {
		t.Fatalf("reads/sets = %d/%v, want one read and SetVolume(53)", reads, sets)
	}
}

func TestMusicCapClampsAnUpStep(t *testing.T) {
	f := newFixture(t, 10, time.Second, 60, false, true)
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:      f.sink,
			Focus:     f.focus.get,
			MaxVolume: func() int { return 65 },
			Cue:       func(context.Context) { f.cues++ },
			Now:       f.clock.Now,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()
	if got, _, _ := f.sink.state(); got != 65 {
		t.Fatalf("volume = %d, want the 65%% music cap", got)
	}

	// Already at the cap: another up step changes nothing and stays silent.
	f.clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()
	if got, _, sets := f.sink.state(); got != 65 || len(sets) != 1 {
		t.Fatalf("volume/sets = %d/%v at the cap, want 65 and no second write", got, sets)
	}
}

func TestDownStepIsNeverBlockedByTheMusicCap(t *testing.T) {
	f := newFixture(t, 10, time.Second, 75, false, true)
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:      f.sink,
			Focus:     f.focus.get,
			MaxVolume: func() int { return 60 },
			Now:       f.clock.Now,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handler.Handle(context.Background(), voicecmd.VolumeDownModel)
	handler.Wait()
	if got, _, _ := f.sink.state(); got != 65 {
		t.Fatalf("volume = %d, want the down step to 65 even above the cap", got)
	}

	// An up step above the cap is a no-op, never a drop to the cap.
	f.clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()
	if got, _, sets := f.sink.state(); got != 65 || len(sets) != 1 {
		t.Fatalf("volume/sets = %d/%v above the cap, want 65 and no write", got, sets)
	}
}

func TestUpStepBelowTheCapStopsAtIt(t *testing.T) {
	f := newFixture(t, 10, time.Second, 58, false, true)
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:      f.sink,
			Focus:     f.focus.get,
			MaxVolume: func() int { return 60 },
			Now:       f.clock.Now,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()
	if got, _, _ := f.sink.state(); got != 60 {
		t.Fatalf("volume = %d, want the 60%% cap", got)
	}
}

func TestCuePlaysOnlyWhenTheVolumeChanges(t *testing.T) {
	// At 100 an up step cannot change anything, so it should not chime.
	f := newFixture(t, 10, time.Second, 100, true, false)
	f.handle(voicecmd.VolumeUpModel)
	if got, _, sets := f.sink.state(); got != 100 || len(sets) != 0 {
		t.Fatalf("volume/sets = %d/%v at 100, want no write", got, sets)
	}
	if f.cues != 0 {
		t.Fatalf("cues = %d at the ceiling, want none", f.cues)
	}
}

func TestUpStepAboveTheCeilingNeverLowersTheLevel(t *testing.T) {
	// Something else set the sink above 100. Clamping to 100 would make
	// "volume up" a drop, so above the ceiling an up step is a no-op.
	f := newFixture(t, 10, time.Second, 105, true, false)
	f.handle(voicecmd.VolumeUpModel)
	if got, _, sets := f.sink.state(); got != 105 || len(sets) != 0 {
		t.Fatalf("volume/sets = %d/%v above the ceiling, want 105 and no write", got, sets)
	}
	if f.cues != 0 {
		t.Fatalf("cues = %d above the ceiling, want none", f.cues)
	}
}

func TestReadAndWriteFailuresDoNotPanicOrChime(t *testing.T) {
	for _, test := range []struct {
		name string
		sink *fakeSink
	}{
		{name: "read fails", sink: &fakeSink{percent: 40, readErr: errors.New("pactl: gone")}},
		{name: "write fails", sink: &fakeSink{percent: 40, setErr: errors.New("pactl: gone")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cues := 0
			handler, err := voicecmd.New(
				voicecmd.Config{Step: 10, Cooldown: time.Second},
				voicecmd.Deps{
					Sink:  test.sink,
					Focus: func() (bool, bool) { return true, false },
					Cue:   func(context.Context) { cues++ },
				},
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			handler.Handle(context.Background(), voicecmd.VolumeUpModel)
			handler.Wait()
			if cues != 0 {
				t.Fatalf("cues = %d after a sink failure, want none", cues)
			}
		})
	}
}

// hangingSink blocks like a wedged pactl until its context ends.
type hangingSink struct{}

func (hangingSink) Volume(ctx context.Context) (audio.VolumeState, error) {
	<-ctx.Done()
	return audio.VolumeState{}, ctx.Err()
}

func (hangingSink) SetVolume(ctx context.Context, _ int) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAHangingSinkDoesNotStallTheVoiceLoop(t *testing.T) {
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:        hangingSink{},
			Focus:       func() (bool, bool) { return true, false },
			SinkTimeout: 400 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		handler.Handle(context.Background(), voicecmd.VolumeUpModel)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle still blocked on a hanging sink; the wake stream would stop being read")
	}
	// Well below the sink timeout: the read happened, or is happening, on the
	// command's own goroutine, not this one.
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Handle took %v with a 400ms sink timeout, so it waited for the sink", elapsed)
	}

	// The command goroutine ends on its own sink timeout, without a shutdown.
	waited := make(chan struct{})
	go func() {
		handler.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the command goroutine outlived its sink timeout")
	}
}

// slowSink is the fake pactl that sleeps: Volume blocks until the test closes
// release, or until the command's context or sink timeout ends. It records
// every call, so a test can prove how many commands reached Pulse.
type slowSink struct {
	entered chan struct{} // one wake-up per Volume call, never blocks the call
	release chan struct{}

	mu      sync.Mutex
	percent int
	reads   int
	sets    []int
	err     error
}

func newSlowSink(percent int) *slowSink {
	return &slowSink{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		percent: percent,
	}
}

func (s *slowSink) Volume(ctx context.Context) (audio.VolumeState, error) {
	s.mu.Lock()
	s.reads++
	percent, err := s.percent, s.err
	s.mu.Unlock()

	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return audio.VolumeState{}, ctx.Err()
	}
	if err != nil {
		return audio.VolumeState{}, err
	}
	return audio.VolumeState{Percent: percent}, nil
}

func (s *slowSink) SetVolume(_ context.Context, percent int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.percent = percent
	s.sets = append(s.sets, percent)
	return nil
}

func (s *slowSink) state() (int, int, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.percent, s.reads, append([]int(nil), s.sets...)
}

// awaitEntered waits until the command goroutine reached Volume.
func (s *slowSink) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the command never reached the sink")
	}
}

func newSlowHandler(t *testing.T, sink *slowSink, clock *clock, timeout time.Duration) *voicecmd.Handler {
	t.Helper()
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:        sink,
			Focus:       func() (bool, bool) { return true, false },
			SinkTimeout: timeout,
			Now:         clock.Now,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

func TestHandleReturnsWhileTheSinkIsStillBlocked(t *testing.T) {
	sink := newSlowSink(40)
	handler := newSlowHandler(t, sink, &clock{now: time.Now()}, 10*time.Second)

	var elapsed time.Duration
	done := make(chan struct{})
	go func() {
		start := time.Now()
		handler.Handle(context.Background(), voicecmd.VolumeUpModel)
		elapsed = time.Since(start)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Handle blocked on a slow sink; the wake stream would stop being read")
	}
	// The sink is released only below, so this proves Handle returned while
	// pactl was still wedged rather than after a timeout.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("Handle took %v before the sink was released", elapsed)
	}
	sink.awaitEntered(t)

	close(sink.release)
	handler.Wait()
	if got, reads, sets := sink.state(); got != 50 || reads != 1 || len(sets) != 1 {
		t.Fatalf("volume/reads/sets = %d/%d/%v, want the deferred step to 50", got, reads, sets)
	}
}

func TestASecondDetectionDuringASlowCommandIsDropped(t *testing.T) {
	sink := newSlowSink(40)
	clock := &clock{now: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)}
	handler := newSlowHandler(t, sink, clock, 10*time.Second)

	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	sink.awaitEntered(t)

	// Past the cooldown, so the drop can only be the in-flight rule.
	clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Handle(context.Background(), voicecmd.VolumeDownModel)

	close(sink.release)
	handler.Wait()

	got, reads, sets := sink.state()
	if reads != 1 || len(sets) != 1 || sets[0] != 50 {
		t.Fatalf("reads/sets = %d/%v, want one dropped burst to make one step to 50", reads, sets)
	}
	if got != 50 {
		t.Fatalf("volume = %d, want 50", got)
	}
}

func TestASlowSinkDoesNotStopTheLoopConsumingFrames(t *testing.T) {
	sink := newSlowSink(40)
	handler := newSlowHandler(t, sink, &clock{now: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)}, 10*time.Second)

	// One goroutine plays the part of the voice loop's reading goroutine: it
	// takes a frame and dispatches whatever detection came with it.
	frames := make(chan struct{})
	consumed := make(chan struct{})
	go func() {
		for range frames {
			handler.Handle(context.Background(), voicecmd.VolumeUpModel)
			consumed <- struct{}{}
		}
	}()

	frames <- struct{}{} // the volume detection
	<-consumed           // the loop goroutine dispatched it and is free again
	sink.awaitEntered(t) // ...and its command is now stuck in pactl

	for i := 0; i < 50; i++ {
		select {
		case frames <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Fatalf("the loop goroutine stopped taking frames after %d while pactl was slow", i)
		}
		select {
		case <-consumed:
		case <-time.After(2 * time.Second):
			t.Fatalf("the loop goroutine stopped dispatching after %d frames while pactl was slow", i)
		}
	}
	close(frames)

	close(sink.release)
	handler.Wait()
	if _, reads, sets := sink.state(); reads != 1 || len(sets) != 1 {
		t.Fatalf("reads/sets = %d/%v, want the burst to leave one volume step", reads, sets)
	}
}

func TestShutdownEndsAnInFlightCommandAndReleasesTheSlot(t *testing.T) {
	sink := newSlowSink(40)
	clock := &clock{now: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)}
	handler := newSlowHandler(t, sink, clock, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	handler.Handle(ctx, voicecmd.VolumeUpModel)
	sink.awaitEntered(t)

	cancel() // the voice loop shutting down while pactl is wedged
	waited := make(chan struct{})
	go func() {
		handler.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the command goroutine outlived a cancelled context; that is a leak")
	}
	if _, _, sets := sink.state(); len(sets) != 0 {
		t.Fatalf("sets = %v after a cancelled read, want no write", sets)
	}

	// The in-flight slot is free again: a command on a live context still runs.
	close(sink.release)
	clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()
	if got, _, sets := sink.state(); got != 50 || len(sets) != 1 || sets[0] != 50 {
		t.Fatalf("volume/sets = %d/%v after shutdown, want one step to 50", got, sets)
	}
}

func TestAFailedSlowCommandFailsCleanlyAndReleasesTheSlot(t *testing.T) {
	sink := newSlowSink(40)
	sink.err = errors.New("pactl: gone")
	clock := &clock{now: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)}
	var cues atomic.Int32
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:        sink,
			Focus:       func() (bool, bool) { return true, false },
			SinkTimeout: 10 * time.Second,
			Cue:         func(context.Context) { cues.Add(1) },
			Now:         clock.Now,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	sink.awaitEntered(t)
	close(sink.release)
	handler.Wait()

	if _, _, sets := sink.state(); len(sets) != 0 {
		t.Fatalf("sets = %v after a failed read, want no write", sets)
	}
	if got := cues.Load(); got != 0 {
		t.Fatalf("cues = %d after a failed read, want none", got)
	}

	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()
	clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()
	if got, _, sets := sink.state(); got != 50 || len(sets) != 1 {
		t.Fatalf("volume/sets = %d/%v after recovery, want one step to 50", got, sets)
	}
	if got := cues.Load(); got != 1 {
		t.Fatalf("cues = %d after the recovery step, want one", got)
	}
}

func TestFocusReleasedBeforeTheSinkCallLeavesTheVolumeAlone(t *testing.T) {
	sink := newSlowSink(40)
	var calls atomic.Int32
	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink: sink,
			// Focus is held when Handle accepts the detection and released
			// before the command goroutine reaches the sink.
			Focus: func() (bool, bool) {
				return calls.Add(1) == 1, false
			},
			SinkTimeout: time.Second,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	handler.Wait()

	if got, reads, sets := sink.state(); got != 40 || reads != 0 || len(sets) != 0 {
		t.Fatalf("volume/reads/sets = %d/%d/%v after Focus was released, want untouched", got, reads, sets)
	}
}

func TestUnknownModelIsIgnored(t *testing.T) {
	f := newFixture(t, 10, time.Second, 40, true, false)
	f.handle("play_jazz")
	if got, reads, sets := f.sink.state(); got != 40 || reads != 0 || len(sets) != 0 {
		t.Fatalf("volume/reads/sets = %d/%d/%v for an unknown model, want untouched", got, reads, sets)
	}
	if f.cues != 0 {
		t.Fatalf("cues = %d for an unknown model, want none", f.cues)
	}
}

func TestNewRejectsAnInvalidConfig(t *testing.T) {
	valid := voicecmd.Deps{Sink: &fakeSink{}, Focus: func() (bool, bool) { return true, false }}
	for _, cfg := range []voicecmd.Config{
		{Step: 0, Cooldown: time.Second},
		{Step: 51, Cooldown: time.Second},
		{Step: 10, Cooldown: 0},
	} {
		if _, err := voicecmd.New(cfg, valid); err == nil {
			t.Fatalf("New accepted %+v", cfg)
		}
	}
	if _, err := voicecmd.New(voicecmd.Config{Step: 10, Cooldown: time.Second}, voicecmd.Deps{Focus: valid.Focus}); err == nil {
		t.Fatal("New accepted a nil sink")
	}
	if _, err := voicecmd.New(voicecmd.Config{Step: 10, Cooldown: time.Second}, voicecmd.Deps{Sink: valid.Sink}); err == nil {
		t.Fatal("New accepted a nil focus")
	}
}
