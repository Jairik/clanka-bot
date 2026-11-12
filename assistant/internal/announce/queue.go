// Package announce queues speech the Assistant starts without being asked and
// plays it in the gaps between turns: never while a reply is playing, never
// while a Confirmation is waiting for its answer, and never while a hold such
// as Quiet hours is on.
package announce

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Speaker speaks one line of unprompted speech. speech.Speaker satisfies it.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// Option configures a Queue.
type Option func(*Queue)

// WithBlocked makes the queue wait while blocked reports true. The Assistant
// passes the pending Confirmation here: a Confirmation must resolve before an
// Announcement can speak. Blocked runs on the playback goroutine and must not
// call back into the Queue.
func WithBlocked(blocked func() bool) Option {
	return func(q *Queue) { q.blocked = blocked }
}

// Queue plays Announcements one at a time, in the order they were queued. The
// worker waits for the last reply to finish, a hold to be released and the
// caller's blocked state to clear.
type Queue struct {
	speaker Speaker
	logger  *slog.Logger
	blocked func() bool

	mu      sync.Mutex
	items   []string
	held    bool
	closed  bool
	turns   int
	playing bool
	// play is the current announcement's context; TurnStarted cancels it so a
	// reply never waits behind unprompted speech.
	play       context.Context
	cancelPlay context.CancelFunc

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
}

// New starts a queue around one speaker. A nil speaker keeps the queue inert:
// Enqueue reports false and nothing plays.
func New(speaker Speaker, logger *slog.Logger, opts ...Option) *Queue {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		speaker: speaker,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(q)
	}
	if speaker == nil {
		close(q.done)
		return q
	}
	go q.run()
	return q
}

// Enqueue adds one announcement and returns false when it cannot be queued: a
// blank line, a closed queue or no speech seam.
func (q *Queue) Enqueue(text string) bool {
	if q == nil {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	q.mu.Lock()
	if q.closed || q.speaker == nil {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, text)
	q.mu.Unlock()
	q.notify()
	return true
}

// TurnStarted marks the start of a reply: announcements wait for it, and one
// already playing is cancelled so the user is answered promptly.
func (q *Queue) TurnStarted() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.turns++
	cancelPlay := q.cancelPlay
	q.mu.Unlock()
	if cancelPlay != nil {
		cancelPlay()
	}
}

// TurnFinished marks the end of a reply and lets the queue resume.
func (q *Queue) TurnFinished() {
	if q == nil {
		return
	}
	q.mu.Lock()
	if q.turns > 0 {
		q.turns--
	}
	q.mu.Unlock()
	q.notify()
}

// Hold stops announcements from starting and keeps them queued. An announcement
// already playing is not interrupted; Quiet hours uses this hook.
func (q *Queue) Hold() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.held = true
	q.mu.Unlock()
}

// Release lifts a hold and lets the queued announcements play.
func (q *Queue) Release() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.held = false
	q.mu.Unlock()
	q.notify()
}

// Held reports whether a hold is on.
func (q *Queue) Held() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held
}

// Len returns how many announcements are waiting.
func (q *Queue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Close stops the queue, cancels any announcement playing and drops the rest.
// It waits for playback to return, so it is safe to call during shutdown.
func (q *Queue) Close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.closed = true
	q.items = nil
	cancelPlay := q.cancelPlay
	q.mu.Unlock()
	q.cancel()
	if cancelPlay != nil {
		cancelPlay()
	}
	<-q.done
}

// run plays ready announcements until the queue closes.
func (q *Queue) run() {
	defer close(q.done)
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
		}
		for {
			text, ok := q.next()
			if !ok {
				break
			}
			q.speak(text)
		}
	}
}

// next claims the oldest announcement when one may start now.
func (q *Queue) next() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.held || q.turns > 0 || q.playing || len(q.items) == 0 {
		return "", false
	}
	if q.blocked != nil && q.blocked() {
		return "", false
	}
	text := q.items[0]
	q.items = q.items[1:]
	q.playing = true
	q.play, q.cancelPlay = context.WithCancel(q.ctx)
	return text, true
}

// speak plays one announcement and records whether it finished.
func (q *Queue) speak(text string) {
	q.mu.Lock()
	ctx := q.play
	q.mu.Unlock()

	err := q.speaker.SayText(ctx, text)

	q.mu.Lock()
	if q.cancelPlay != nil {
		q.cancelPlay()
	}
	q.play, q.cancelPlay = nil, nil
	q.playing = false
	q.mu.Unlock()

	switch {
	case err == nil:
		q.logger.Info("announcement", "text_chars", len(text))
	case ctx.Err() != nil:
		q.logger.Info("announcement interrupted", "text_chars", len(text))
	default:
		q.logger.Error("announcement", "text_chars", len(text), "error", err.Error())
	}
}

// notify wakes the worker without blocking the caller.
func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
