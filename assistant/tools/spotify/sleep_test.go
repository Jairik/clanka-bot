package spotify

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
)

// sleepPlayer records Pause calls without touching the network. The embedded
// Player keeps the fake narrow: only the seam the sleep timer drives is real.
type sleepPlayer struct {
	Player
	pauses atomic.Int32
}

func (p *sleepPlayer) Pause(context.Context) error {
	p.pauses.Add(1)
	return nil
}

// fakeSleeper is a one-shot timer the test fires by hand.
type fakeSleeper struct {
	d       time.Duration
	f       func()
	mu      sync.Mutex
	stopped bool
}

func (s *fakeSleeper) Stop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.stopped = true
	return true
}

func (s *fakeSleeper) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *fakeSleeper) fire() {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if !stopped {
		s.f()
	}
}

type fakeScheduler struct {
	mu    sync.Mutex
	all   []*fakeSleeper
	calls int
}

func (sch *fakeScheduler) schedule(d time.Duration, f func()) oneShot {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	s := &fakeSleeper{d: d, f: f}
	sch.all = append(sch.all, s)
	sch.calls++
	return s
}

func (sch *fakeScheduler) last() *fakeSleeper {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	return sch.all[len(sch.all)-1]
}

func (sch *fakeScheduler) count() int {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	return sch.calls
}

// sleepSpeaker is the Announcements speaker; said receives "Music stopped.".
type sleepSpeaker struct {
	said chan string
}

func (s *sleepSpeaker) SayText(_ context.Context, text string) error {
	s.said <- text
	return nil
}

type sleepRig struct {
	svc     *Service
	player  *sleepPlayer
	focus   *focus.Registry
	sched   *fakeScheduler
	speaker *sleepSpeaker
}

func newSleepRig(t *testing.T) *sleepRig {
	t.Helper()
	player := &sleepPlayer{}
	registry := focus.New()
	svc := NewService(player, registry)
	sched := &fakeScheduler{}
	svc.scheduler = sched.schedule
	speaker := &sleepSpeaker{said: make(chan string, 8)}
	queue := announce.New(speaker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(queue.Close)
	svc.announce = queue
	return &sleepRig{svc: svc, player: player, focus: registry, sched: sched, speaker: speaker}
}

// withMusic claims Music Focus, the state spotify.play leaves behind.
func (r *sleepRig) withMusic() {
	r.svc.takeFocus()
}

func expectAnnounced(t *testing.T, r *sleepRig, want string) {
	t.Helper()
	select {
	case got := <-r.speaker.said:
		if got != want {
			t.Fatalf("announcement = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("announcement = none, want %q", want)
	}
}

func expectSilent(t *testing.T, r *sleepRig) {
	t.Helper()
	select {
	case got := <-r.speaker.said:
		t.Fatalf("unexpected announcement %q", got)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestSleepFiresOnceAndAnnounces(t *testing.T) {
	r := newSleepRig(t)
	r.withMusic()
	sleep := NewSleep(r.svc)
	out, err := sleep.Handler(t.Context(), SleepIn{Minutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Armed || out.Replaced || out.Minutes != 30 || out.Duration != "30 minutes" {
		t.Fatalf("out = %+v", out)
	}
	if got := sleep.Speak(out); got != "Stopping the music in 30 minutes." {
		t.Fatalf("Speak = %q", got)
	}
	sleeper := r.sched.last()
	if sleeper.d != 30*time.Minute {
		t.Fatalf("deadline = %s, want 30m", sleeper.d)
	}

	sleeper.fire()
	expectAnnounced(t, r, "Music stopped.")
	if got := r.player.pauses.Load(); got != 1 {
		t.Fatalf("pauses = %d, want 1", got)
	}
	// A duplicate callback from the scheduler must never pause or announce twice.
	sleeper.fire()
	if got := r.player.pauses.Load(); got != 1 {
		t.Fatalf("pauses after second fire = %d, want 1", got)
	}
	expectSilent(t, r)
	// Pausing keeps the music Focus claim so a bare "resume" still reaches it.
	if !r.focus.Held(focus.Music) {
		t.Fatal("expiry dropped the music Focus claim")
	}
}

func TestSleepCancelPreventsPause(t *testing.T) {
	r := newSleepRig(t)
	r.withMusic()
	sleep := NewSleep(r.svc)
	if _, err := sleep.Handler(t.Context(), SleepIn{Minutes: 20}); err != nil {
		t.Fatal(err)
	}
	sleeper := r.sched.last()

	out, err := sleep.Handler(t.Context(), SleepIn{Minutes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Cancelled || out.NoTimer {
		t.Fatalf("out = %+v, want cancelled", out)
	}
	if got := sleep.Speak(out); got != "Sleep timer cancelled." {
		t.Fatalf("Speak = %q", got)
	}
	if !sleeper.isStopped() {
		t.Fatal("cancel did not stop the scheduled timer")
	}
	sleeper.fire() // a callback already queued by the scheduler must be inert
	if got := r.player.pauses.Load(); got != 0 {
		t.Fatalf("pauses after cancel = %d, want 0", got)
	}
	expectSilent(t, r)

	// Cancelling again is honest: there is no timer any more.
	out, err = sleep.Handler(t.Context(), SleepIn{Minutes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !out.NoTimer || out.Cancelled {
		t.Fatalf("out = %+v, want no timer", out)
	}
	if got := sleep.Speak(out); got != "There is no sleep timer set." {
		t.Fatalf("Speak = %q", got)
	}
}

func TestSleepRejectsUnknownAndNonPositiveDurations(t *testing.T) {
	accepted := []struct {
		text    string
		minutes int
	}{
		{"set a sleep timer for 30 minutes", 30},
		{"sleep timer for 20 minutes", 20},
		{"stop the music in 20 minutes", 20},
		{"stop the music after 30 minutes", 30},
		{"turn the music off after 20 minutes", 20},
		{"stop playing in an hour", 60},
		{"stop playback in 45 minutes", 45},
		{"sleep timer for an hour", 60},
		{"pause the music in an hour", 60},
	}
	for _, tc := range accepted {
		in, ok := extractSleep(tc.text)
		if !ok || in.Minutes != tc.minutes {
			t.Errorf("extractSleep(%q) = %+v, %v; want %d minutes", tc.text, in, ok, tc.minutes)
		}
	}
	for _, text := range []string{"cancel the sleep timer", "cancel the music sleep timer", "clear the music sleep timer"} {
		cancelled, ok := extractSleep(text)
		if !ok || cancelled.Minutes != 0 {
			t.Errorf("extractSleep(%q) = %+v, %v; want zero minutes", text, cancelled, ok)
		}
	}
	refused := []string{
		"stop the music",
		"set a timer for 20 minutes",
		"sleep timer for a while",
		"sleep timer for 0 minutes",
		"stop the music in zero minutes",
		"stop the music in 0 minutes",
	}
	for _, text := range refused {
		if in, ok := extractSleep(text); ok {
			t.Errorf("extractSleep(%q) accepted %+v", text, in)
		}
	}
	// The handler refuses a negative duration even if an Agent supplies one.
	r := newSleepRig(t)
	r.withMusic()
	out, err := NewSleep(r.svc).Handler(t.Context(), SleepIn{Minutes: -5})
	if err == nil || out.Armed {
		t.Fatalf("negative minutes: out=%+v err=%v", out, err)
	}
	if r.sched.count() != 0 {
		t.Fatal("negative minutes armed a timer")
	}
}

func TestSleepSecondSetReplacesFirst(t *testing.T) {
	r := newSleepRig(t)
	r.withMusic()
	sleep := NewSleep(r.svc)
	if _, err := sleep.Handler(t.Context(), SleepIn{Minutes: 30}); err != nil {
		t.Fatal(err)
	}
	first := r.sched.last()

	out, err := sleep.Handler(t.Context(), SleepIn{Minutes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Armed || !out.Replaced {
		t.Fatalf("out = %+v, want replaced", out)
	}
	if got := sleep.Speak(out); got != "Stopping the music in 10 minutes instead." {
		t.Fatalf("Speak = %q", got)
	}
	second := r.sched.last()
	if second == first || second.d != 10*time.Minute {
		t.Fatalf("second deadline = %+v (%s)", second, second.d)
	}
	if !first.isStopped() {
		t.Fatal("replacement did not cancel the first deadline")
	}

	first.fire() // stale generation: no pause, no announcement
	if got := r.player.pauses.Load(); got != 0 {
		t.Fatalf("pauses after stale fire = %d, want 0", got)
	}
	expectSilent(t, r)

	second.fire()
	expectAnnounced(t, r, "Music stopped.")
	if got := r.player.pauses.Load(); got != 1 {
		t.Fatalf("pauses = %d, want 1", got)
	}
}

func TestSleepArmingNeedsMusicFocus(t *testing.T) {
	r := newSleepRig(t)
	// No spotify.play happened, so nothing holds Music Focus.
	sleep := NewSleep(r.svc)
	out, err := sleep.Handler(t.Context(), SleepIn{Minutes: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Idle || out.Armed {
		t.Fatalf("out = %+v, want idle", out)
	}
	if got := sleep.Speak(out); got != "Nothing is playing." {
		t.Fatalf("Speak = %q", got)
	}
	if r.sched.count() != 0 {
		t.Fatal("armed a timer while nothing was playing")
	}
}

func TestSleepCancelledByPauseStopAndClose(t *testing.T) {
	t.Run("manual pause tool", func(t *testing.T) {
		r := newSleepRig(t)
		r.withMusic()
		_, _ = NewSleep(r.svc).Handler(t.Context(), SleepIn{Minutes: 20})
		sleeper := r.sched.last()
		if _, err := NewPause(r.svc).Handler(t.Context(), PauseIn{}); err != nil {
			t.Fatal(err)
		}
		sleeper.fire()
		if got := r.player.pauses.Load(); got != 1 { // the one manual Pause only
			t.Fatalf("pauses = %d, want 1", got)
		}
		expectSilent(t, r)
	})

	t.Run("focus pause", func(t *testing.T) {
		r := newSleepRig(t)
		r.withMusic()
		_, _ = NewSleep(r.svc).Handler(t.Context(), SleepIn{Minutes: 20})
		sleeper := r.sched.last()
		r.focus.HandleIntent(t.Context(), focus.Pause)
		sleeper.fire()
		if got := r.player.pauses.Load(); got != 1 {
			t.Fatalf("pauses = %d, want 1", got)
		}
		expectSilent(t, r)
	})

	t.Run("focus stop", func(t *testing.T) {
		r := newSleepRig(t)
		r.withMusic()
		_, _ = NewSleep(r.svc).Handler(t.Context(), SleepIn{Minutes: 20})
		sleeper := r.sched.last()
		r.focus.HandleIntent(t.Context(), focus.Stop)
		sleeper.fire()
		if got := r.player.pauses.Load(); got != 1 {
			t.Fatalf("pauses = %d, want 1", got)
		}
		expectSilent(t, r)
	})

	t.Run("close path", func(t *testing.T) {
		r := newSleepRig(t)
		r.withMusic()
		_, _ = NewSleep(r.svc).Handler(t.Context(), SleepIn{Minutes: 20})
		sleeper := r.sched.last()
		m := &Module{service: r.svc}
		m.Close()
		sleeper.fire()
		if got := r.player.pauses.Load(); got != 0 {
			t.Fatalf("pauses after close = %d, want 0", got)
		}
		expectSilent(t, r)
	})
}

// TestSleepSurvivesToolReconstruction models a Dashboard page change: tools are
// rebuilt for a fresh request, but the deadline lives in the one Service and
// still fires.
func TestSleepSurvivesToolReconstruction(t *testing.T) {
	r := newSleepRig(t)
	r.withMusic()
	firstTool := NewSleep(r.svc)
	if _, err := firstTool.Handler(t.Context(), SleepIn{Minutes: 20}); err != nil {
		t.Fatal(err)
	}
	sleeper := r.sched.last()

	// A page change hands the user a brand-new Tool value over the same Service.
	freshTool := NewSleep(r.svc)
	sleeper.fire()
	expectAnnounced(t, r, "Music stopped.")
	if got := r.player.pauses.Load(); got != 1 {
		t.Fatalf("pauses = %d, want 1", got)
	}
	// The fresh Tool can also cancel the timer the old one armed.
	if _, err := freshTool.Handler(t.Context(), SleepIn{Minutes: 15}); err != nil {
		t.Fatal(err)
	}
	out, err := freshTool.Handler(t.Context(), SleepIn{Minutes: 0})
	if err != nil || !out.Cancelled {
		t.Fatalf("cancel from fresh tool: out=%+v err=%v", out, err)
	}
}
