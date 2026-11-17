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

// WithNow sets the time source for recording enqueue times, used in tests.
func WithNow(now func() time.Time) Option {
	return func(q *Queue) { q.now = now }
}

// RingSize is the bounded capacity of the announcements ring.
const RingSize = 20

// Kind identifies the category of an Announcement.
type Kind string

const (
	KindGeneric  Kind = "generic"
	KindReminder Kind = "reminder"
	KindSummary  Kind = "summary"
)

// State tracks the lifecycle state of an Announcement line in the ring.
type State string

const (
	StateQueued      State = "queued"
	StateHeld        State = "held"
	StateSpoken      State = "spoken"
	StateInterrupted State = "interrupted"
)

// Line represents one spoken announcement line tracked in the ring.
type Line struct {
	Text       string    `json:"text"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Kind       Kind      `json:"kind"`
	State      State     `json:"state"`
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
	kind   Kind
	line   *Line
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
	// OnChange is called after any queue-state change the Dashboard cares
	// about: an item queued, a hold taken or lifted, and an item starting or
	// finishing playback. It is read under the lock and run after it, so a
	// callback may call Snapshot but must not re-enter a mutating method.
	OnChange func()

	mu          sync.Mutex
	items       []item
	ring        []*Line
	currentLine *Line
	now         func() time.Time
	held        bool
	closed      bool
	turns       int
	playing     bool
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
	if q.now == nil {
		q.now = time.Now
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

// recordLineLocked adds a line to the bounded ring, evicting oldest when full.
// The caller holds q.mu.
func (q *Queue) recordLineLocked(line *Line) {
	q.ring = append(q.ring, line)
	if len(q.ring) > RingSize {
		q.ring = append([]*Line(nil), q.ring[len(q.ring)-RingSize:]...)
	}
}

// Recent returns a copy of the announcements in the ring, oldest first.
func (q *Queue) Recent() []Line {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	res := make([]Line, len(q.ring))
	for i, l := range q.ring {
		res[i] = *l
	}
	return res
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
	state := StateQueued
	if q.held {
		state = StateHeld
	}
	line := &Line{
		Text:       text,
		EnqueuedAt: q.now(),
		Kind:       KindGeneric,
		State:      state,
	}
	q.recordLineLocked(line)
	q.items = append(q.items, item{text: text, kind: KindGeneric, line: line})
	q.mu.Unlock()
	q.notify()
	q.notifyChange()
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
	q.notifyChange()
	return true
}

// EnqueueAlert queues one scheduled alert whose Cue and spoken text are a
// single item. A Quiet-hours hold does not delay it, but the current reply, a
// pending Confirmation and a ringing alert still do. It is never collapsed
// into the held-text summary, so an alert cannot become part of a morning
// digest and an audio preview is still a sound rather than a narration.
func (q *Queue) EnqueueAlert(text string, play func(context.Context) error) bool {
	if q == nil || play == nil {
		return false
	}
	text = strings.TrimSpace(text)
	q.mu.Lock()
	if q.closed || !q.audio {
		q.mu.Unlock()
		return false
	}
	it := item{text: text, play: play, urgent: true, kind: KindReminder}
	if text != "" {
		line := &Line{
			Text:       text,
			EnqueuedAt: q.now(),
			Kind:       KindReminder,
			State:      StateQueued,
		}
		q.recordLineLocked(line)
		it.line = line
	}
	q.items = append(q.items, it)
	q.mu.Unlock()
	q.notify()
	q.notifyChange()
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
	if cancelPlay != nil && q.currentLine != nil {
		q.currentLine.State = StateInterrupted
	}
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
	for _, it := range q.items {
		if !it.urgent && it.line != nil && it.line.State == StateQueued {
			it.line.State = StateHeld
		}
	}
	q.mu.Unlock()
	q.notifyChange()
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
	} else if wasHeld {
		for _, it := range q.items {
			if it.line != nil && it.line.State == StateHeld {
				it.line.State = StateQueued
			}
		}
	}
	q.mu.Unlock()
	q.notify()
	q.notifyChange()
}

// collapseTextLocked replaces every pending text item with one summary. The
// caller holds q.mu. Audio items are copied through untouched so a Dashboard
// preview is still a sound, not a spoken description of one.
func (q *Queue) collapseTextLocked() {
	lines := make([]string, 0, len(q.items))
	heldLines := make(map[*Line]bool)
	for _, queued := range q.items {
		if queued.play == nil && queued.text != "" {
			lines = append(lines, queued.text)
			if queued.line != nil {
				heldLines[queued.line] = true
			}
		}
	}
	if len(lines) == 0 {
		return
	}
	summary := summarizeLines(lines)
	summaryLine := &Line{
		Text:       summary,
		EnqueuedAt: q.now(),
		Kind:       KindSummary,
		State:      StateQueued,
	}

	newRing := make([]*Line, 0, len(q.ring))
	placedRing := false
	for _, l := range q.ring {
		if heldLines[l] {
			if !placedRing {
				newRing = append(newRing, summaryLine)
				placedRing = true
			}
			continue
		}
		newRing = append(newRing, l)
	}
	if !placedRing {
		newRing = append(newRing, summaryLine)
	}
	if len(newRing) > RingSize {
		newRing = append([]*Line(nil), newRing[len(newRing)-RingSize:]...)
	}
	q.ring = newRing

	collapsed := make([]item, 0, len(q.items))
	placed := false
	for _, queued := range q.items {
		if queued.play == nil {
			if !placed {
				collapsed = append(collapsed, item{
					text: summary,
					kind: KindSummary,
					line: summaryLine,
				})
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

// Snapshot is the queue state with no message content, taken under the lock.
// The Dashboard uses the counts and flags to show what is waiting and whether
// a hold is on, without ever receiving announcement text (#217).
func (q *Queue) Snapshot() struct {
	Len     int
	Held    bool
	Playing bool
} {
	if q == nil {
		return struct {
			Len     int
			Held    bool
			Playing bool
		}{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return struct {
		Len     int
		Held    bool
		Playing bool
	}{Len: len(q.items), Held: q.held, Playing: q.playing}
}

// notifyChange runs OnChange after a state change. It is always called without
// q.mu held so a callback may read the queue without deadlocking.
func (q *Queue) notifyChange() {
	if q == nil {
		return
	}
	q.mu.Lock()
	cb := q.OnChange
	q.mu.Unlock()
	if cb != nil {
		cb()
	}
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

// blockedWaiting reports that the blocked predicate is the only gate left
// before a queued item starts. An urgent alert ignores a hold, so it is still
// waiting when the blocked gate is the only thing left. The predicate is
// opaque and may change without a queue event (a Confirmation TTL, a Ring
// timing out), so the caller rechecks while this holds rather than trusting
// one evaluation.
func (q *Queue) blockedWaiting() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.turns > 0 || q.playing || len(q.items) == 0 {
		return false
	}
	if q.held && !q.hasUrgentLocked() {
		return false
	}
	return q.blocked != nil
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
	if q.closed || q.turns > 0 || q.playing || len(q.items) == 0 {
		q.mu.Unlock()
		return item{}, false
	}
	if q.blocked != nil && q.blocked() {
		q.mu.Unlock()
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
			q.mu.Unlock()
			return item{}, false
		}
	}
	next := q.items[index]
	copy(q.items[index:], q.items[index+1:])
	q.items[len(q.items)-1] = item{}
	q.items = q.items[:len(q.items)-1]
	q.playing = true
	q.currentLine = next.line
	q.play, q.cancelPlay = context.WithCancel(q.ctx)
	q.mu.Unlock()
	q.notifyChange()
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

	interrupted := ctx.Err() != nil

	q.mu.Lock()
	if q.cancelPlay != nil {
		q.cancelPlay()
	}
	q.play, q.cancelPlay = nil, nil
	q.playing = false
	if next.line != nil {
		if interrupted {
			next.line.State = StateInterrupted
		} else if err == nil {
			next.line.State = StateSpoken
		}
	}
	q.currentLine = nil
	q.mu.Unlock()
	q.notifyChange()

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
