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

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

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

// Filler returns the cached Agent path Filler. It is the one filler slot per
// request: the Agent path claims it first and #25 may reuse it for slow replies.
func (s *Session) Filler() *speech.Filler { return s.filler }

// Ask resolves transcript and speaks the reply. It returns once playback has
// finished, with the text reply even when playback failed.
func (s *Session) Ask(ctx context.Context, req tool.Request, transcript string) (pipeline.Reply, error) {
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
	var fillerClaimed atomic.Bool
	var prefix string
	streamed := 0

	events := pipeline.Events{
		// The path is known before the model call, so the cached phrase is heard
		// while the first tokens are still pending.
		AgentPath: func() {
			if s.filler == nil || s.sink == nil || !fillerClaimed.CompareAndSwap(false, true) {
				return
			}
			player.play(item{filler: s.filler})
		},
		// A dropped Confirmation's "Cancelled." is spoken before any answer.
		Prefix: func(text string) {
			prefix = text
			player.play(item{text: text})
		},
		Delta: func(delta string) {
			for _, chunk := range chunker.Write(delta) {
				player.play(item{text: chunk})
				streamed++
			}
		},
	}

	reply := s.pipeline.HandleEvents(ctx, req, transcript, events)
	if rest := chunker.Flush(); rest != "" {
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
