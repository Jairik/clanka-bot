// Package voice's false-wake counters are the measurement surface for wake
// threshold tuning (#299). They are in-memory only: a process restart resets
// them, nothing is persisted and no setting is added. Only model names and
// numbers are kept — no transcript or private text ever enters a counter.
package voice

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

// ModelFalseWakes is one wake model's false-wake counts since process start.
// Wakes is the denominator: every wake word that started a turn. NoSpeech and
// Unroutable are the two false-wake kinds, both a subset of Wakes.
type ModelFalseWakes struct {
	Wakes      int64
	NoSpeech   int64
	Unroutable int64
}

// Rate is this model's false-wake share as a rounded whole percent. A model
// with no wakes has an unknown rate, never a misleading 0%.
func (m ModelFalseWakes) Rate() string {
	if m.Wakes <= 0 {
		return "unknown"
	}
	pct := 100 * float64(m.NoSpeech+m.Unroutable) / float64(m.Wakes)
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// Stats holds race-safe false-wake counters keyed by wake-word model. The loop
// is the main writer (Wake on detection, NoSpeech on an empty transcript); the
// main fan-out observer calls Observe for the unroutable kind. The Dashboard
// reads a Snapshot. A new Stats — like a process restart — starts empty.
type Stats struct {
	mu     sync.Mutex
	models map[string]*ModelFalseWakes
}

// NewStats returns an empty counter set.
func NewStats() *Stats {
	return &Stats{models: make(map[string]*ModelFalseWakes)}
}

// Wake records a wake word that started a turn. An empty model (a no-wake
// follow-up turn) is not counted.
func (s *Stats) Wake(model string) {
	if s == nil || model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelLocked(model).Wakes++
}

// NoSpeech records a wake word that produced no speech.
func (s *Stats) NoSpeech(model string) {
	if s == nil || model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelLocked(model).NoSpeech++
}

// Observe implements the unroutable half of the false-wake measurement. It is
// meant to be called from the latency observer fan-out in main, after the
// Activity store has seen the completion. A voice request with a transcript
// but no route (Tool or Path) and no reply audio is "wake with nothing
// routable". Every other completion is ignored.
func (s *Stats) Observe(c latency.Completed) {
	if s == nil || c.Source != latency.SourceVoice || c.WakeWord == "" {
		return
	}
	if strings.TrimSpace(c.Transcript) == "" {
		return
	}
	if c.Tool != "" || c.Path != "" {
		return
	}
	if c.HasResponse {
		return // reply audio played, so something was routable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelLocked(c.WakeWord).Unroutable++
}

// Snapshot returns a copy of the counters so callers can read them without
// holding the lock. The returned maps and values are safe to keep.
func (s *Stats) Snapshot() map[string]ModelFalseWakes {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ModelFalseWakes, len(s.models))
	for name, counters := range s.models {
		out[name] = *counters
	}
	return out
}

// modelLocked returns a model's counters, creating them on first use. The
// caller must hold s.mu.
func (s *Stats) modelLocked(model string) *ModelFalseWakes {
	counters := s.models[model]
	if counters == nil {
		counters = &ModelFalseWakes{}
		s.models[model] = counters
	}
	return counters
}
