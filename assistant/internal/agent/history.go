package agent

import (
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxResultBytes is the most room one Tool result may take in the shared
// history. Every later Agent turn resends the whole history, so an uncapped
// web page, search list or calendar payload would be paid for again on each
// request. A result at or below the cap is stored byte for byte; a larger one
// is replaced by a compact JSON envelope. With the default ten-turn history
// this bounds the history part of a request to roughly ten turns' worth of
// calls at MaxResultBytes each, instead of whatever the Tools happened to
// return (#202).
const MaxResultBytes = 600

// MaxPreviewBytes is how much of an oversized result survives in its envelope.
// The preview is cut on a UTF-8 boundary and trimmed further when JSON escaping
// would push the envelope past MaxResultBytes.
const MaxPreviewBytes = 500

// truncatedResult is the valid JSON envelope that replaces an oversized Tool
// result. It is marshalled, never hand-concatenated, so the marker is always
// valid JSON and the preview is always escaped.
type truncatedResult struct {
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
	Preview   string `json:"preview"`
}

// ToolCall is one Tool call recorded in a turn. A Local-only call keeps the
// turn on the Pi: it is never sent to the model.
type ToolCall struct {
	Name      string          `json:"tool"`
	Args      json.RawMessage `json:"args,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	LocalOnly bool            `json:"-"`
}

// Turn is one utterance and its reply, shared by the Fast path and the Agent
// path. Calls holds the compact Tool-call JSON.
type Turn struct {
	Utterance string
	Reply     string
	Calls     []ToolCall
}

// localOnly reports whether any call in the turn must stay on the Pi.
func (t Turn) localOnly() bool {
	for _, c := range t.Calls {
		if c.LocalOnly {
			return true
		}
	}
	return false
}

// History is the rolling conversation history. Both paths append to the same
// History; it keeps the last maxTurns turns and clears after a quiet window.
type History struct {
	mu       sync.Mutex
	maxTurns int
	quiet    time.Duration
	now      func() time.Time
	last     time.Time
	turns    []Turn
}

// NewHistory builds a rolling history. A nil now uses time.Now.
func NewHistory(maxTurns int, quiet time.Duration, now func() time.Time) *History {
	if now == nil {
		now = time.Now
	}
	return &History{maxTurns: maxTurns, quiet: quiet, now: now}
}

// Add appends one turn, dropping the oldest ones past the cap. A quiet gap
// before the turn starts a fresh conversation. Every Tool result is bounded to
// MaxResultBytes on the way in, so no consumer can reintroduce an unbounded
// payload onto the Pi or into a later request. The caller's own slice keeps
// its full results: Activity and Result.Calls still show the turn that ran.
func (h *History) Add(t Turn) {
	t.Calls = compactCalls(t.Calls)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.quiet > 0 && !h.last.IsZero() && now.Sub(h.last) > h.quiet {
		h.turns = nil
	}
	h.turns = append(h.turns, t)
	if h.maxTurns > 0 && len(h.turns) > h.maxTurns {
		h.turns = append([]Turn(nil), h.turns[len(h.turns)-h.maxTurns:]...)
	}
	h.last = now
}

// Recent returns a copy of the live turns. A quiet gap clears the history.
func (h *History) Recent() []Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.quiet > 0 && !h.last.IsZero() && h.now().Sub(h.last) > h.quiet {
		h.turns = nil
		h.last = time.Time{}
	}
	return append([]Turn(nil), h.turns...)
}

// Reset drops every turn.
func (h *History) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turns = nil
	h.last = time.Time{}
}

// compactCalls returns a copy of calls with every oversized result bounded, so
// the caller's slice is never mutated and the full payload stays available to
// Activity. An empty slice stays nil.
func compactCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(calls))
	copy(out, calls)
	for i := range out {
		out[i].Result = compactResult(out[i].Result)
	}
	return out
}

// compactResult bounds one Tool result for the shared history. A result of at
// most MaxResultBytes is returned unchanged, so a small conversation is
// byte-for-byte what it was. A larger one becomes the truncated envelope: the
// model still sees that the call ran, how big the answer was, and how it
// started.
func compactResult(result json.RawMessage) json.RawMessage {
	if len(result) <= MaxResultBytes {
		return result
	}
	preview := previewText(result, MaxPreviewBytes)
	envelope := marshalTruncated(len(result), preview)
	// Escaping can double or triple the preview's bytes. Trim whole runes until
	// the envelope fits the same budget, so the cap is a real ceiling rather
	// than a hint.
	for len(envelope) > MaxResultBytes && preview != "" {
		_, size := utf8.DecodeLastRuneInString(preview)
		preview = preview[:len(preview)-size]
		envelope = marshalTruncated(len(result), preview)
	}
	return envelope
}

// previewText returns the first n bytes of raw, cut back to a rune boundary so
// the preview never holds half a character.
func previewText(raw json.RawMessage, n int) string {
	if len(raw) > n {
		raw = raw[:n]
	}
	for len(raw) > 0 && !utf8.Valid(raw) {
		raw = raw[:len(raw)-1]
	}
	return string(raw)
}

// marshalTruncated builds the envelope. A struct of a bool, an int and a
// string cannot fail to marshal, so the fallback only documents that.
func marshalTruncated(size int, preview string) json.RawMessage {
	envelope, err := json.Marshal(truncatedResult{Truncated: true, Bytes: size, Preview: preview})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return envelope
}
