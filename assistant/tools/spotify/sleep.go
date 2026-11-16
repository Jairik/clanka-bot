package spotify

import (
	"context"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
)

// oneShot is the cancel handle for a scheduled callback. *time.Timer satisfies
// it; fake timers in tests do too.
type oneShot interface {
	Stop() bool
}

// scheduler is the one-shot timer seam. time.AfterFunc is the default; tests
// inject a fake so the callback can be fired without waiting on wall time.
type scheduler func(d time.Duration, f func()) oneShot

func afterFunc(d time.Duration, f func()) oneShot { return time.AfterFunc(d, f) }

// armSleep arms the single sleep timer for d, replacing any earlier deadline.
// It reports whether it replaced a timer that was already set.
func (s *Service) armSleep(d time.Duration) (replaced bool) {
	sched := s.scheduler
	if sched == nil {
		sched = afterFunc
	}
	now := s.now
	if now == nil {
		now = time.Now
	}

	s.mu.Lock()
	s.sleepGen++
	gen := s.sleepGen
	replaced = s.sleepStop != nil
	if s.sleepStop != nil {
		s.sleepStop.Stop()
	}
	s.sleepDeadline = now().Add(d)
	s.sleepStop = sched(d, func() { s.fireSleep(gen) })
	s.mu.Unlock()
	return replaced
}

// cancelSleepLocked clears the sleep timer; the caller must hold s.mu.
func (s *Service) cancelSleepLocked() bool {
	was := s.sleepStop != nil
	if s.sleepStop != nil {
		s.sleepStop.Stop()
	}
	s.sleepStop = nil
	s.sleepDeadline = time.Time{}
	s.sleepGen++
	return was
}

// cancelSleep cancels the sleep timer. It reports whether one was actually
// set, so the spoken reply never claims a cancellation that did not happen.
func (s *Service) cancelSleep() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelSleepLocked()
}

// musicHeld reports whether music still holds the Service's Focus claim. A
// sleep timer can only be armed while it does.
func (s *Service) musicHeld() bool {
	return s != nil && s.focus != nil && s.focus.Held(focus.Music)
}

// fireSleep pauses music once for the generation that scheduled it. A cancel
// or a replacement stops the handle and bumps the generation, so a stale
// callback does nothing. The music Focus claim is deliberately kept, so a
// bare "resume" still works after the timer fires.
func (s *Service) fireSleep(gen uint64) {
	s.mu.Lock()
	if gen != s.sleepGen || s.sleepStop == nil {
		s.mu.Unlock()
		return
	}
	s.sleepStop = nil
	s.sleepDeadline = time.Time{}
	s.sleepGen++ // one pause per arm, even if a scheduler double-fires
	queue := s.announce
	s.mu.Unlock()

	if s.player != nil {
		_ = s.player.Pause(context.Background())
	}
	if queue != nil {
		queue.Enqueue("Music stopped.")
	}
}
