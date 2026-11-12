// Package announce queues speech the Assistant starts without being asked and
// plays it in the gaps between turns: never while a reply is playing, never
// while a Confirmation is waiting for its answer, and never while a hold such
// as Quiet hours is on.
package announce

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// blockedRecheck bounds how long a queued item waits for an opaque blocked
// predicate to clear without a queue event, such as a Ring that times out.
const blockedRecheck = 250 * time.Millisecond

// Speaker speaks one line of unprompted speech. speech.Speaker satisfies it.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// Option configures a Queue.
type Option func(*Queue)

// WithBlocked makes the queue wait while blocked reports true. The Assistant
// passes the pending Confirmation and a ringing alert here: both must resolve
// before unprompted speech or a preview can play. Blocked runs on the playback
// goroutine and must not call back into the Queue.
func WithBlocked(blocked func() bool) Option {
	return func(q *Queue) { q.blocked = blocked }
}

// WithPreempt cancels a playing audio preview when preempt reports true, so a
// higher-priority Ring never waits behind a long unprompted sound. Spoken
// announcements are not preempted; a Ring waits for that line instead.
func WithPreempt(preempt func() bool) Option {
	return func(q *Queue) { q.preempt = preempt }
}

// WithPlayback starts the worker even when no speaker is configured, so a
// text-only Dashboard can still queue a sound preview on the Pi. Text
// announcements remain unavailable without a speaker.
func WithPlayback() Option {
	return func(q *Queue) { q.audio = true }
}

// WithHoldSummary collapses the held text announcements into one spoken summary
// on Release. Audio preview items keep their own queue slots and are never
// narrated as text. Quiet hours installs this on the one Announcement queue;
// without it, Release keeps the previous one-line-at-a-time behaviour.
func WithHoldSummary() Option {
	return func(q *Queue) { q.summarize = true }
}

// item is one queued unprompted action: a line to speak, or a sound to play.
// Both obey the same turn, hold and blocked gates, so a Pi preview cannot cut
// a reply or a pending Confirmation. urgent marks a scheduled alert (a
// Reminder) that must still sound inside Quiet hours; it bypasses only the
// hold, never a turn or a blocked Confirmation.
type item struct {
	text   string
	play   func(context.Context) error
	urgent bool
}

// Queue plays Announcements one at a time, in the order they were queued. The
// worker waits for the last reply to finish, a hold to be released and the
// caller's blocked state to clear.
type Queue struct {
	speaker Speaker
	logger  *slog.Logger
	blocked func() bool
	// preempt cancels a playing audio preview when a Ring needs the speaker.
	preempt func() bool
	// audio keeps the worker alive without a speaker so a Dashboard can queue
	// a Pi preview on a text-only setup (WithPlayback).
	audio bool
	// summarize collapses held text announcements on Release (WithHoldSummary).
	summarize bool

	mu      sync.Mutex
	items   []item
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
	if speaker != nil {
		// A speaker implies sound playback; WithPlayback covers the nil case.
		q.audio = true
	}
	if speaker == nil && !q.audio {
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
	q.items = append(q.items, item{text: text})
	q.mu.Unlock()
	q.notify()
	return true
}

// EnqueuePlay queues one sound to play through the same between-turns gates as
// an Announcement. The play callback runs on the worker; it must respect the
// context, which TurnStarted cancels for Barge-in.
func (q *Queue) EnqueuePlay(play func(context.Context) error) bool {
	if q == nil || play == nil {
		return false
	}
	q.mu.Lock()
	if q.closed || !q.audio {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, item{play: play})
	q.mu.Unlock()
	q.notify()
	return true
}

// EnqueueAlert queues one scheduled alert whose Cue and spoken text are a
// single item. A Quiet-hours hold does not delay it, but the current reply, a
// pending Confirmation and a ringing alert still do. It is never collapsed
// into the held-text summary, so an alert cannot become part of a morning
// digest and an audio preview is still a sound rather than a narration.
func (q *Queue) EnqueueAlert(play func(context.Context) error) bool {
	if q == nil || play == nil {
		return false
	}
	q.mu.Lock()
	if q.closed || !q.audio {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, item{play: play, urgent: true})
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

// Release lifts a hold and lets the queued announcements play. With
// WithHoldSummary, all text items held during the hold are replaced by one
// summary line placed where the first text item was; audio previews stay
// separate and play in their original order.
func (q *Queue) Release() {
	if q == nil {
		return
	}
	q.mu.Lock()
	wasHeld := q.held
	q.held = false
	if wasHeld && q.summarize {
		q.collapseTextLocked()
	}
	q.mu.Unlock()
	q.notify()
}

// collapseTextLocked replaces every pending text item with one summary. The
// caller holds q.mu. Audio items are copied through untouched so a Dashboard
// preview is still a sound, not a spoken description of one.
func (q *Queue) collapseTextLocked() {
	lines := make([]string, 0, len(q.items))
	for _, queued := range q.items {
		if queued.play == nil && queued.text != "" {
			lines = append(lines, queued.text)
		}
	}
	if len(lines) == 0 {
		return
	}
	summary := summarizeLines(lines)
	collapsed := make([]item, 0, len(q.items))
	placed := false
	for _, queued := range q.items {
		if queued.play == nil {
			if !placed {
				collapsed = append(collapsed, item{text: summary})
				placed = true
			}
			continue
		}
		collapsed = append(collapsed, queued)
	}
	q.items = collapsed
}

// summarizeLines builds one meaningful line without a model call. Duplicate
// lines are dropped; a single held line stays as it was spoken.
func summarizeLines(lines []string) string {
	seen := make(map[string]bool, len(lines))
	unique := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		unique = append(unique, line)
	}
	if len(unique) == 1 {
		return unique[0]
	}
	return "While it was quiet: " + strings.Join(unique, " ")
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
		for {
			next, ok := q.next()
			if !ok {
				break
			}
			q.playItem(next)
		}
		// Every other gate wakes the queue when it changes. The blocked
		// predicate is opaque, so recheck it while it is the only reason to
		// wait; a Ring ending without a turn must still resume a preview.
		var recheck <-chan time.Time
		if q.blockedWaiting() {
			recheck = time.After(blockedRecheck)
		}
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
		case <-recheck:
		}
	}
}

// blockedWaiting reports that only the blocked predicate keeps a queued item
// from starting. An urgent alert ignores a hold, so it is still waiting when
// the blocked gate is the only thing left.
func (q *Queue) blockedWaiting() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.turns > 0 || q.playing || len(q.items) == 0 {
		return false
	}
	if q.held && !q.hasUrgentLocked() {
		return false
	}
	return q.blocked != nil && q.blocked()
}

// hasUrgentLocked reports whether an alert is waiting behind the hold. The
// caller holds q.mu.
func (q *Queue) hasUrgentLocked() bool {
	for _, queued := range q.items {
		if queued.urgent {
			return true
		}
	}
	return false
}

// next claims the oldest item when one may start now. Inside a hold it skips
// ordinary announcements but takes the first urgent alert, so Quiet hours
// cannot delay a Reminder.
func (q *Queue) next() (item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.turns > 0 || q.playing || len(q.items) == 0 {
		return item{}, false
	}
	if q.blocked != nil && q.blocked() {
		return item{}, false
	}
	index := 0
	if q.held {
		index = -1
		for i, queued := range q.items {
			if queued.urgent {
				index = i
				break
			}
		}
		if index < 0 {
			return item{}, false
		}
	}
	next := q.items[index]
	copy(q.items[index:], q.items[index+1:])
	q.items[len(q.items)-1] = item{}
	q.items = q.items[:len(q.items)-1]
	q.playing = true
	q.play, q.cancelPlay = context.WithCancel(q.ctx)
	return next, true
}

// playItem speaks or plays one queued item and records whether it finished.
func (q *Queue) playItem(next item) {
	q.mu.Lock()
	ctx := q.play
	q.mu.Unlock()

	var err error
	if next.play != nil {
		err = q.runPlayback(ctx, next.play)
	} else if q.speaker == nil {
		err = errors.New("announce: no speaker configured")
	} else {
		err = q.speaker.SayText(ctx, next.text)
	}

	q.mu.Lock()
	if q.cancelPlay != nil {
		q.cancelPlay()
	}
	q.play, q.cancelPlay = nil, nil
	q.playing = false
	q.mu.Unlock()

	switch {
	case err == nil && next.play != nil:
		q.logger.Info("audio item")
	case err == nil:
		q.logger.Info("announcement", "text_chars", len(next.text))
	case ctx.Err() != nil:
		q.logger.Info("unprompted item interrupted", "audio", next.play != nil)
	default:
		q.logger.Error("unprompted item", "audio", next.play != nil, "error", err.Error())
	}
}

// runPlayback runs one audio item and cancels it when a Ring preempts, so a
// long preview cannot hold the one speaker or delay an alert.
func (q *Queue) runPlayback(ctx context.Context, play func(context.Context) error) error {
	if q.preempt == nil {
		return play(ctx)
	}
	done := make(chan error, 1)
	go func() { done <- play(ctx) }()
	ticker := time.NewTicker(blockedRecheck)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if q.preempt() {
				q.cancelCurrent()
			}
		}
	}
}

// cancelCurrent stops the item playing now without touching queued items.
func (q *Queue) cancelCurrent() {
	q.mu.Lock()
	cancel := q.cancelPlay
	q.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// notify wakes the worker without blocking the caller.
func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
