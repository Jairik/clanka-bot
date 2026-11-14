package voicecmd_test

import (
	"context"
	"errors"
	"sync"
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
			f.handler.Handle(context.Background(), test.model)

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

	f.handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	f.clock.Advance(time.Second)
	f.handler.Handle(context.Background(), voicecmd.VolumeUpModel)

	if got, _, sets := f.sink.state(); got != 50 || len(sets) != 1 {
		t.Fatalf("volume/sets = %d/%v after 1s, want 50 and one step", got, sets)
	}

	f.clock.Advance(500 * time.Millisecond)
	f.handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	if got, _, sets := f.sink.state(); got != 60 || len(sets) != 2 {
		t.Fatalf("volume/sets = %d/%v after the cooldown, want 60 and two steps", got, sets)
	}
}

func TestOneStepPerDetection(t *testing.T) {
	f := newFixture(t, 10, time.Second, 43, true, false)
	f.handler.Handle(context.Background(), voicecmd.VolumeUpModel)

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
	if got, _, _ := f.sink.state(); got != 65 {
		t.Fatalf("volume = %d, want the 65%% music cap", got)
	}

	// Already at the cap: another up step changes nothing and stays silent.
	f.clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
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
	if got, _, _ := f.sink.state(); got != 65 {
		t.Fatalf("volume = %d, want the down step to 65 even above the cap", got)
	}

	// An up step above the cap is a no-op, never a drop to the cap.
	f.clock.Advance(2 * time.Second)
	handler.Handle(context.Background(), voicecmd.VolumeUpModel)
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
	if got, _, _ := f.sink.state(); got != 60 {
		t.Fatalf("volume = %d, want the 60%% cap", got)
	}
}

func TestCuePlaysOnlyWhenTheVolumeChanges(t *testing.T) {
	// At 100 an up step cannot change anything, so it should not chime.
	f := newFixture(t, 10, time.Second, 100, true, false)
	f.handler.Handle(context.Background(), voicecmd.VolumeUpModel)
	if got, _, sets := f.sink.state(); got != 100 || len(sets) != 0 {
		t.Fatalf("volume/sets = %d/%v at 100, want no write", got, sets)
	}
	if f.cues != 0 {
		t.Fatalf("cues = %d at the ceiling, want none", f.cues)
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
			SinkTimeout: 50 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
}

func TestUnknownModelIsIgnored(t *testing.T) {
	f := newFixture(t, 10, time.Second, 40, true, false)
	f.handler.Handle(context.Background(), "play_jazz")
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
