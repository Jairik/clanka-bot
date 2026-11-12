// Package latency records one request's per-stage timings and writes the
// single request log line. Response latency follows the glossary definition:
// from the user's last voiced frame to the first sample of the actual reply.
// Chimes and Fillers never set that endpoint.
package latency

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Source says where a request entered the pipeline.
type Source string

const (
	// SourceVoice is the microphone path: wake word, endpointing and STT.
	SourceVoice Source = "voice"
	// SourceDebug is the localhost typed-transcript entry. It has no
	// endpointing or STT stages, so it never reports response latency.
	SourceDebug Source = "debug"
)

// Stage is one measured pipeline stage. Each value is a duration measured by
// the layer that owns it.
type Stage string

const (
	// StageEndpointing is the last voiced frame to the endpointer firing
	// (the fixed end-of-speech silence window plus one frame).
	StageEndpointing Stage = "endpointing"
	// StageSTTFinalize is the endpointer firing to the final transcript.
	StageSTTFinalize Stage = "stt_finalize"
	// StageRouting is the transcript to the chosen path (Confirmation,
	// Control phrase, semantic match or Agent path).
	StageRouting Stage = "routing"
	// StageToolExecute is the total time spent executing Tools for the
	// request, across every call and Agent step.
	StageToolExecute Stage = "tool_execute"
	// StageAgentFirstToken is the model request to its first streamed
	// event, Agent path only.
	StageAgentFirstToken Stage = "agent_first_token"
	// StageTTSFirstAudio is the first reply text handed to TTS to the first
	// PCM chunk successfully submitted to the sink's stdin. It is not the
	// instant the DAC emits sound: pacat buffers (--latency-msec=100) and the
	// host adds its own latency, so physical onset is later by an amount this
	// plumbing cannot measure.
	StageTTSFirstAudio Stage = "tts_first_audio"
)

// Checkpoint names are absolute times since the turn start (the user's last
// voiced frame, or the debug entry's arrival).
const (
	// CheckpointFirstChunk is the first reply text unit handed to TTS.
	CheckpointFirstChunk = "first_chunk"
)

// Outcome says how a request ended. Only OutcomeOK requests carry a response
// latency; the others are counted but never averaged as if they succeeded.
type Outcome string

const (
	// OutcomeOK means reply audio actually started playing.
	OutcomeOK Outcome = "ok"
	// OutcomeNoAudio means the turn finished without reply audio (for
	// example a text-only entry, or a reply with no spoken text).
	OutcomeNoAudio Outcome = "no_audio"
	// OutcomeFailed means an error stopped the turn before reply audio.
	OutcomeFailed Outcome = "failed"
	// OutcomeCancelled means the request context was cancelled, as Barge-in
	// does, before reply audio.
	OutcomeCancelled Outcome = "cancelled"
)

// timelineStage is one stage's accumulated duration.
type timelineStage struct {
	total time.Duration
	count int
}

// Timeline is one request's timings. It is safe for concurrent use: the
// pipeline, Agent path and playback goroutine all mark the same request.
// Methods return early after Complete so a late stage cannot rewrite a logged
// request.
type Timeline struct {
	id     string
	source Source

	mu         sync.Mutex
	created    time.Time
	now        func() time.Time
	lastVoice  time.Time
	replyAudio time.Time
	stages     map[Stage]timelineStage
	checkpoint map[string]time.Duration
	attrs      map[string]any
	path       string
	tool       string
	transcript string
	failure    string
	failed     bool
	done       bool
}

// New opens a timeline. The request ID is the one every layer logs, so voice,
// session, Agent and playback lines correlate.
func New(id string, source Source) *Timeline {
	now := time.Now()
	return &Timeline{
		id:         id,
		source:     source,
		created:    now,
		now:        time.Now,
		stages:     make(map[Stage]timelineStage),
		checkpoint: make(map[string]time.Duration),
		attrs:      make(map[string]any),
	}
}

// ID is the request ID shared by every log line for this request.
func (t *Timeline) ID() string { return t.id }

// Source is where the request entered.
func (t *Timeline) Source() Source { return t.source }

// SetLastVoice records the user's last voiced frame. Only the voice path has
// one; it is the start of Response latency.
func (t *Timeline) SetLastVoice(at time.Time) {
	if at.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done || !t.lastVoice.IsZero() {
		return
	}
	t.lastVoice = at
}

// start is the turn start used by checkpoints.
func (t *Timeline) start() time.Time {
	if !t.lastVoice.IsZero() {
		return t.lastVoice
	}
	return t.created
}

// Set records a stage's duration. The first mark wins, which keeps a single
// model call's first token even when the Agent runs several steps.
func (t *Timeline) Set(stage Stage, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	if _, exists := t.stages[stage]; exists {
		return
	}
	t.stages[stage] = timelineStage{total: clamp(d), count: 1}
}

// Add accumulates a stage's duration over several calls, such as Tool
// executions across Agent steps.
func (t *Timeline) Add(stage Stage, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	current := t.stages[stage]
	current.total += clamp(d)
	current.count++
	t.stages[stage] = current
}

// MarkReplyAudio records the first sample of the actual reply. Chimes and
// Fillers are played by other paths and never call it.
func (t *Timeline) MarkReplyAudio() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done || !t.replyAudio.IsZero() {
		return
	}
	t.replyAudio = t.now()
}

// MarkCheckpoint records an absolute time since the turn start, once.
func (t *Timeline) MarkCheckpoint(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	if _, exists := t.checkpoint[name]; exists {
		return
	}
	if d := t.now().Sub(t.start()); d >= 0 {
		t.checkpoint[name] = d
	}
}

// SetRoute records the fields the request log has always carried.
func (t *Timeline) SetRoute(path, tool, transcript string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.path, t.tool, t.transcript = path, tool, transcript
}

// SetAttr adds an extra structured field to the request line, such as the
// semantic router's scores.
func (t *Timeline) SetAttr(key string, value any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.attrs[key] = value
}

// SetFailure records why the request failed, for the request line.
func (t *Timeline) SetFailure(message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.failure = message
}

// MarkFailed says the request failed even if a failure message was spoken.
// Such a turn is counted apart from successful replies, never as a fast one.
func (t *Timeline) MarkFailed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.failed = true
}

// Failed reports whether the request failed before its answer.
func (t *Timeline) Failed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}

// Duration returns a stage's measured duration, if it was measured.
func (t *Timeline) Duration(stage Stage) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.stages[stage]
	return value.total, ok
}

// Count returns how many marks a stage received; zero means missing.
func (t *Timeline) Count(stage Stage) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stages[stage].count
}

// Checkpoint returns a checkpoint's time since the turn start, if recorded.
func (t *Timeline) Checkpoint(name string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.checkpoint[name]
	return value, ok
}

// Response returns Response latency: last voiced frame to first reply audio.
// It is available only for a voice request whose reply audio actually started.
// A missing value is not zero latency.
func (t *Timeline) Response() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.responseLocked()
}

// HasReplyAudio reports whether the reply actually started playing.
func (t *Timeline) HasReplyAudio() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.replyAudio.IsZero()
}

// Complete writes the one request log line for this timeline. Later calls, and
// stage marks after the first call, are ignored.
func (t *Timeline) Complete(logger *slog.Logger, outcome Outcome, err error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	line := t.line(outcome, err)
	t.mu.Unlock()

	if logger == nil {
		logger = slog.Default()
	}
	attrs := line.attrs
	switch outcome {
	case OutcomeFailed:
		logger.Error("request", attrs...)
	case OutcomeCancelled:
		logger.Warn("request", attrs...)
	default:
		logger.Info("request", attrs...)
	}
}

type logLine struct {
	attrs []any
}

// line renders the request log line. It must be called with the lock held.
func (t *Timeline) line(outcome Outcome, err error) logLine {
	attrs := []any{
		"request_id", t.id,
		"source", string(t.source),
		"path", t.path,
		"tool", t.tool,
		"transcript", t.transcript,
		"outcome", string(outcome),
	}
	if d, ok := t.responseLocked(); ok {
		attrs = append(attrs, "response_ms", d.Milliseconds())
	}

	// Missing stages are absent keys, never a fabricated zero. A zero would
	// read as "instant" instead of "not measured".
	stages := make(map[string]any, len(t.stages))
	for stage, value := range t.stages {
		stages[string(stage)+"_ms"] = value.total.Milliseconds()
	}
	attrs = append(attrs, "stages", stages)

	if len(t.checkpoint) > 0 {
		checkpoints := make(map[string]any, len(t.checkpoint))
		for name, value := range t.checkpoint {
			checkpoints[name+"_ms"] = value.Milliseconds()
		}
		attrs = append(attrs, "checkpoints", checkpoints)
	}

	keys := make([]string, 0, len(t.attrs))
	for key := range t.attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attrs = append(attrs, key, t.attrs[key])
	}

	failure := t.failure
	if failure == "" && err != nil {
		failure = err.Error()
	}
	if failure != "" {
		attrs = append(attrs, "error", failure)
	}
	return logLine{attrs: attrs}
}

func (t *Timeline) responseLocked() (time.Duration, bool) {
	if t.lastVoice.IsZero() || t.replyAudio.IsZero() {
		return 0, false
	}
	d := t.replyAudio.Sub(t.lastVoice)
	if d < 0 {
		return 0, false
	}
	return d, true
}

func clamp(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

type contextKey struct{}

// WithContext attaches a timeline to ctx. Every layer below reads it, so one
// request ID and one set of timings span voice, session, Agent and playback.
func WithContext(ctx context.Context, t *Timeline) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, t)
}

// FromContext returns the request timeline, or nil when there is none. A nil
// timeline is normal: text-only callers and tests do not need one.
func FromContext(ctx context.Context) *Timeline {
	t, _ := ctx.Value(contextKey{}).(*Timeline)
	return t
}

// RequestID returns the request ID carried by ctx, or "" without a timeline.
func RequestID(ctx context.Context) string {
	if t := FromContext(ctx); t != nil {
		return t.ID()
	}
	return ""
}

// SetStage marks a first-wins stage duration on the context's timeline.
func SetStage(ctx context.Context, stage Stage, d time.Duration) {
	if t := FromContext(ctx); t != nil {
		t.Set(stage, d)
	}
}

// AddStage accumulates a stage duration on the context's timeline.
func AddStage(ctx context.Context, stage Stage, d time.Duration) {
	if t := FromContext(ctx); t != nil {
		t.Add(stage, d)
	}
}

// MarkReplyAudio records the first actual reply sample on ctx's timeline.
func MarkReplyAudio(ctx context.Context) {
	if t := FromContext(ctx); t != nil {
		t.MarkReplyAudio()
	}
}

// Checkpoint records a turn-start-relative checkpoint on ctx's timeline.
func Checkpoint(ctx context.Context, name string) {
	if t := FromContext(ctx); t != nil {
		t.MarkCheckpoint(name)
	}
}

// SetRoute stores the request log's routing fields on ctx's timeline.
func SetRoute(ctx context.Context, path, tool, transcript string) {
	if t := FromContext(ctx); t != nil {
		t.SetRoute(path, tool, transcript)
	}
}

// SetAttr stores an extra request log field on ctx's timeline.
func SetAttr(ctx context.Context, key string, value any) {
	if t := FromContext(ctx); t != nil {
		t.SetAttr(key, value)
	}
}

// SetFailure stores the failure reason on ctx's timeline.
func SetFailure(ctx context.Context, message string) {
	if t := FromContext(ctx); t != nil {
		t.SetFailure(message)
	}
}

// MarkFailed marks ctx's request as failed.
func MarkFailed(ctx context.Context) {
	if t := FromContext(ctx); t != nil {
		t.MarkFailed()
	}
}

// Complete writes the request line for ctx's timeline and is a no-op without
// one.
func Complete(ctx context.Context, logger *slog.Logger, outcome Outcome, err error) {
	if t := FromContext(ctx); t != nil {
		t.Complete(logger, outcome, err)
	}
}
