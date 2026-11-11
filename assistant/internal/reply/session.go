// Package reply answers one transcript out loud: it routes through the
// pipeline, plays the Agent path Filler as soon as that path is chosen, and
// streams generated text into TTS chunk by chunk while generation continues.
package reply

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// FillerAfter is how long a request may stay silent before the cached Filler
// plays. The clock starts when the endpointer fires (see AskFrom), not when Ask
// is called, so a slow transcription does not push the cue back.
const FillerAfter = 1500 * time.Millisecond

// Pipeline resolves a transcript. pipeline.Pipeline satisfies it.
type Pipeline interface {
	HandleEvents(ctx context.Context, req tool.Request, transcript string, events pipeline.Events) pipeline.Reply
}

// Speaker speaks one phrase. speech.Speaker satisfies it through SayText.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// Session is the seam for one spoken turn: #18's capture loop calls Ask, and
// #25 cancels playback through the request context.
type Session struct {
	pipeline Pipeline
	speaker  Speaker
	sink     speech.Sink
	filler   *speech.Filler
	logger   *slog.Logger
}

// Option configures a Session.
type Option func(*Session)

// WithLogger sets where the one request log line is written.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Session) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// New builds a session. A nil speaker leaves replies text-only.
func New(p Pipeline, speaker Speaker, sink speech.Sink, filler *speech.Filler, opts ...Option) *Session {
	s := &Session{pipeline: p, speaker: speaker, sink: sink, filler: filler, logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Ask resolves transcript and speaks the reply, timing the Filler from now.
// The debug entry uses it; the voice loop uses AskFrom.
func (s *Session) Ask(ctx context.Context, req tool.Request, transcript string) (pipeline.Reply, error) {
	return s.ask(ctx, req, transcript, time.Now())
}

// AskFrom is Ask with the Filler clock starting at from: the voice loop passes
// the moment the endpointer fired, which is before STT finalises the transcript.
func (s *Session) AskFrom(ctx context.Context, req tool.Request, transcript string, from time.Time) (pipeline.Reply, error) {
	if from.IsZero() {
		from = time.Now()
	}
	return s.ask(ctx, req, transcript, from)
}

// Ask resolves transcript and speaks the reply. It returns once playback has
// finished, with the text reply even when playback failed.
func (s *Session) ask(ctx context.Context, req tool.Request, transcript string, from time.Time) (pipeline.Reply, error) {
	if s == nil || s.pipeline == nil {
		return pipeline.Reply{RequestID: req.ID, Text: pipeline.PlaceholderReply}, nil
	}
	if s.speaker == nil {
		reply := s.pipeline.HandleEvents(ctx, req, transcript, pipeline.Events{})
		s.complete(ctx, nil)
		return reply, nil
	}

	player := newPlayer(ctx, s.speaker, s.sink)
	chunker := speech.NewChunker()
	// The request has one sound slot. Whichever of the Agent path cue, the
	// slow-reply cue or the first streamed chunk claims it is heard first, so a
	// request never carries two Fillers and a Filler never talks over a reply.
	var claimed atomic.Bool
	firstSound := func() bool { return claimed.CompareAndSwap(false, true) }
	var prefix string
	streamed := 0

	events := pipeline.Events{
		// The path is known before the model call, so the cached phrase is heard
		// while the first tokens are still pending.
		AgentPath: func() {
			if s.filler == nil || s.sink == nil || !firstSound() {
				return
			}
			player.play(item{filler: s.filler})
		},
		// A dropped Confirmation's "Cancelled." is spoken before any answer.
		Prefix: func(text string) {
			prefix = text
			firstSound()
			player.play(item{text: text})
		},
		Delta: func(delta string) {
			for _, chunk := range chunker.Write(delta) {
				firstSound()
				player.play(item{text: chunk})
				streamed++
			}
		},
	}

	stopCue := s.cueSlowReply(player, from, firstSound)
	reply := s.pipeline.HandleEvents(ctx, req, transcript, events)
	// The cue must not be queued after the player closes its queue.
	stopCue()
	if rest := chunker.Flush(); rest != "" {
		firstSound()
		player.play(item{text: rest})
		streamed++
	}
	// The Fast path and Agent failures answer without deltas; speak their text.
	// A prefix already spoken by the Prefix event is not repeated. A pending
	// Confirmation question is always spoken, even if the model said something
	// before proposing the call.
	if streamed == 0 || reply.Pending != nil {
		text := reply.Text
		if prefix != "" {
			text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
		}
		if text != "" {
			firstSound()
			player.play(item{text: text})
		}
	}
	err := player.finish()
	s.complete(ctx, err)
	return reply, err
}

// complete writes the one request log line for this turn, if the caller
// attached a timeline. Missing reply audio is never recorded as success.
func (s *Session) complete(ctx context.Context, err error) {
	timeline := latency.FromContext(ctx)
	if timeline == nil {
		return
	}
	latency.Complete(ctx, s.logger, outcome(ctx, timeline, err), err)
}

// outcome classifies the turn: a cancelled request is Barge-in or shutdown,
// not a slow failure, and a turn with no reply audio is not a success.
func outcome(ctx context.Context, timeline *latency.Timeline, err error) latency.Outcome {
	switch {
	case err != nil && isCancellation(ctx, err):
		return latency.OutcomeCancelled
	case ctx.Err() != nil:
		return latency.OutcomeCancelled
	case err != nil || timeline.Failed():
		return latency.OutcomeFailed
	case !timeline.HasReplyAudio():
		return latency.OutcomeNoAudio
	default:
		return latency.OutcomeOK
	}
}

// isCancellation treats a cancelled request as Barge-in or shutdown, even when
// TTS reports the cancellation as a plain error.
func isCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// cueSlowReply plays the one Filler when no reply audio has started by
// FillerAfter, measured from the endpointer. The returned stop function must run
// before playback drains; it waits for the timer goroutine to exit.
func (s *Session) cueSlowReply(player *player, from time.Time, claim func() bool) func() {
	if s.filler == nil || s.sink == nil {
		return func() {}
	}
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(FillerAfter - time.Since(from))
		defer timer.Stop()
		select {
		case <-stopped:
		case <-player.ctx.Done():
		case <-timer.C:
			// Nothing is being said yet; the cue keeps the user company.
			if s.filler != nil && s.sink != nil && claim() {
				player.play(item{filler: s.filler})
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stopped) })
		<-done
	}
}

// item is one queued sound: streamed text for the speaker, or cached filler PCM.
type item struct {
	text   string
	filler *speech.Filler
}

// player plays queued sounds in order on its own goroutine, so generation never
// waits for playback.
type player struct {
	speaker Speaker
	sink    speech.Sink
	ctx     context.Context
	cancel  context.CancelFunc
	items   chan item
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func newPlayer(ctx context.Context, speaker Speaker, sink speech.Sink) *player {
	playCtx, cancel := context.WithCancel(ctx)
	p := &player{
		speaker: speaker,
		sink:    sink,
		ctx:     playCtx,
		cancel:  cancel,
		items:   make(chan item, 16),
		done:    make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *player) run() {
	defer close(p.done)
	for next := range p.items {
		if p.ctx.Err() != nil {
			continue // Drain on cancellation or after a playback failure.
		}
		var err error
		if next.filler != nil {
			err = next.filler.Play(p.ctx, p.sink)
		} else {
			// The first reply text unit handed to TTS, after any Filler.
			latency.Checkpoint(p.ctx, latency.CheckpointFirstChunk)
			err = p.speaker.SayText(p.ctx, next.text)
		}
		if err != nil {
			p.mu.Lock()
			if p.err == nil {
				p.err = err
			}
			p.mu.Unlock()
			p.cancel()
		}
	}
}

func (p *player) play(next item) {
	select {
	case p.items <- next:
	case <-p.ctx.Done():
	}
}

// finish closes the queue, waits for playback to drain and returns the first error.
func (p *player) finish() error {
	close(p.items)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
