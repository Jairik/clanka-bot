// Package playback arbitrates the one speaker between a reply and a Ring.
// Music can be ducked, but speech cannot: a Ring waits for the current reply
// sentence (one speakable chunk) to finish, then takes the floor. The reply is
// not cancelled; its next sentence waits until the Ring ends and then resumes.
package playback

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed reports that the gate was closed during shutdown.
var ErrClosed = errors.New("playback: gate is closed")

// Gate is the one-speaker arbiter. The zero value is not usable; build one with
// NewGate. Every method is nil-safe, so a text-only setup can omit the gate.
type Gate struct {
	mu sync.Mutex

	// changed is closed and replaced on every state change; waiters take the
	// current channel and select on it, which lets a context cancel the wait.
	changed chan struct{}

	speaking bool
	// ringWaiting counts Rings that have asked for the floor. A reply must not
	// start a new sentence once a Ring is waiting, even before the current
	// sentence has ended, so the Ring cannot be starved by a fast speaker.
	ringWaiting int
	ringActive  bool
	closed      bool
}

// NewGate builds an open gate.
func NewGate() *Gate {
	return &Gate{changed: make(chan struct{})}
}

// BeginReply marks the start of one speakable reply unit. A unit that starts a
// new sentence blocks while a Ring is waiting or active, so the reply yields at
// the sentence boundary. A continuation unit of the sentence already being
// spoken does not block: the Ring must wait for the whole sentence, not just
// its first chunk. The returned function takes whether this unit ends the
// sentence and must always be called.
func (g *Gate) BeginReply(ctx context.Context) (func(endSentence bool), error) {
	if g == nil {
		return func(bool) {}, nil
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, ErrClosed
	}
	// speaking already true means this unit continues the current sentence,
	// so a waiting Ring must not stop it mid-sentence.
	if !g.speaking {
		for {
			if g.closed {
				g.mu.Unlock()
				return nil, ErrClosed
			}
			if g.ringWaiting == 0 && !g.ringActive {
				break
			}
			if err := g.waitLocked(ctx); err != nil {
				g.mu.Unlock()
				return nil, err
			}
		}
		g.speaking = true
		g.notifyLocked()
	}
	g.mu.Unlock()

	var once sync.Once
	return func(endSentence bool) {
		once.Do(func() {
			if !endSentence {
				return
			}
			g.mu.Lock()
			g.speaking = false
			g.notifyLocked()
			g.mu.Unlock()
		})
	}, nil
}

// EndReplySpeech closes a sentence left open by the final chunk (a reply that
// ended without punctuation). The player calls it on every exit so a Ring can
// never wait forever.
func (g *Gate) EndReplySpeech() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.speaking {
		g.speaking = false
		g.notifyLocked()
	}
	g.mu.Unlock()
}

// WaitForReplySentence waits until the current reply chunk finishes, then takes
// the floor. A Ring that arrives while the reply is idle takes it immediately.
// The returned release function must run when the Ring ends; until then no new
// reply chunk can start.
func (g *Gate) WaitForReplySentence(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	g.mu.Lock()
	g.ringWaiting++
	g.notifyLocked()
	for {
		if g.closed {
			g.ringWaiting--
			g.notifyLocked()
			g.mu.Unlock()
			return nil, ErrClosed
		}
		if !g.speaking {
			break
		}
		if err := g.waitLocked(ctx); err != nil {
			g.ringWaiting--
			g.notifyLocked()
			g.mu.Unlock()
			return nil, err
		}
	}
	g.ringWaiting--
	g.ringActive = true
	g.notifyLocked()
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.ringActive = false
			g.notifyLocked()
			g.mu.Unlock()
		})
	}, nil
}

// Speaking reports whether a reply chunk is currently playing.
func (g *Gate) Speaking() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.speaking
}

// RingActive reports whether a Ring currently owns the floor.
func (g *Gate) RingActive() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ringActive
}

// Speaker is the speech seam a GatedSpeaker wraps.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// GatedSpeaker makes unprompted speech (the Announcement queue) obey the same
// Ring priority as a reply: a Ring waits for the current announcement line to
// finish, and a new announcement waits while a Ring owns the floor.
type GatedSpeaker struct {
	inner Speaker
	gate  *Gate
}

// NewGatedSpeaker wraps a speaker. A nil gate or speaker keeps the original
// behaviour, so a text-only setup needs no wiring change.
func NewGatedSpeaker(inner Speaker, gate *Gate) *GatedSpeaker {
	return &GatedSpeaker{inner: inner, gate: gate}
}

// SayText speaks one announcement after the current reply sentence and behind
// any active Ring.
func (s *GatedSpeaker) SayText(ctx context.Context, text string) error {
	if s == nil || s.inner == nil {
		return nil
	}
	done, err := s.gate.BeginReply(ctx)
	if err != nil {
		return err
	}
	defer done(true)
	return s.inner.SayText(ctx, text)
}

// Close wakes every waiter and makes later claims fail. It is for shutdown
// only; a closed gate does not reopen.
func (g *Gate) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.closed = true
	g.notifyLocked()
	g.mu.Unlock()
}

// waitLocked releases the lock until the next state change or ctx end. The
// caller holds g.mu and re-acquires it before returning.
func (g *Gate) waitLocked(ctx context.Context) error {
	changed := g.changed
	g.mu.Unlock()
	select {
	case <-changed:
		g.mu.Lock()
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		return ctx.Err()
	}
}

// notifyLocked wakes every waiter. The caller holds g.mu.
func (g *Gate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}
