package agent

import (
	"encoding/json"
	"sync"
	"time"
)

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
// before the turn starts a fresh conversation.
func (h *History) Add(t Turn) {
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
