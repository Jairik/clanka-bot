// Package conversation holds the in-memory conversation-mode latch and the
// cue-word match that gives one Agent turn the longer spoken budget. Nothing
// here is persisted: a restart ends the mode, like the Follow-up window.
package conversation

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Live setting keys for conversation mode.
const (
	CueWordsKey            = "conversation.cue_words"
	ModeFollowUpSecondsKey = "conversation.mode_follow_up_seconds"
	ModeIdleSecondsKey     = "conversation.mode_idle_seconds"
)

// Code defaults for conversation mode.
var DefaultCueWords = []string{
	"explain",
	"tell me about",
	"tell me more",
	"why",
	"how does",
	"walk me through",
}

const (
	DefaultModeFollowUpSeconds = 15
	DefaultModeIdleSeconds     = 120
)

// Turn values reported by State.Turn and carried into Activity.
const (
	// Mode is a turn taken while conversation mode is on.
	Mode = "mode"
	// Cue is a single turn lifted by a cue word.
	Cue = "cue"
)

// Definitions declares the live conversation settings. The cue list and both
// windows are read per turn, so a Dashboard change applies to the next one.
func Definitions() []settings.Definition {
	return []settings.Definition{
		settings.Strings(CueWordsKey, DefaultCueWords,
			settings.WithDescription("Words or phrases that give one Agent turn the longer conversation budget. Matching is on normalised whole words."),
			settings.WithValidator(func(value any) error {
				list := value.([]string)
				for _, cue := range list {
					if strings.TrimSpace(cue) == "" {
						return fmt.Errorf("cue words must not be blank")
					}
				}
				return nil
			})),
		settings.Int(ModeFollowUpSecondsKey, DefaultModeFollowUpSeconds,
			settings.WithDescription("No-wake Follow-up window while conversation mode is on. 0 disables it."),
			settings.WithValidator(func(value any) error {
				n := value.(int)
				if n < 0 || n > 60 {
					return fmt.Errorf("conversation Follow-up window must be 0–60 seconds")
				}
				return nil
			})),
		settings.Int(ModeIdleSecondsKey, DefaultModeIdleSeconds,
			settings.WithDescription("Quiet time with no turn after which conversation mode ends silently."),
			settings.WithValidator(func(value any) error {
				n := value.(int)
				if n < 10 || n > 3600 {
					return fmt.Errorf("conversation idle timeout must be 10–3600 seconds")
				}
				return nil
			})),
	}
}

// State is the one in-memory conversation-mode latch. It is safe for
// concurrent use by the Tools, the Agent path and the voice loop.
type State struct {
	mu       sync.Mutex
	active   bool
	lastTurn time.Time
	now      func() time.Time
	idle     func() time.Duration
}

// NewState builds the latch. now defaults to time.Now; idle is the live quiet
// window, and a zero or negative result keeps the mode on until it is ended.
func NewState(now func() time.Time, idle func() time.Duration) *State {
	if now == nil {
		now = time.Now
	}
	return &State{now: now, idle: idle}
}

// Start turns conversation mode on and starts the idle clock.
func (s *State) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	s.lastTurn = s.now()
}

// End turns conversation mode off. It is the simple seam an "end session"
// action calls later.
func (s *State) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
}

// Active reports whether conversation mode is on. A mode that has gone the
// configured window with no turn ends silently here.
func (s *State) Active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked()
}

// Touch refreshes the idle clock for a completed request while the mode is on.
// Every turn counts, not only an Agent turn, so a mixed session does not expire
// mid-conversation. It is a no-op when the mode is off.
func (s *State) Touch() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeLocked() {
		s.lastTurn = s.now()
	}
}

// Turn reports how this utterance uses the conversation budget: Mode while the
// mode is on, Cue when the utterance carries a cue word, or an empty string
// for the default budget. A mode turn restarts the idle clock; a cue word
// lifts only that one turn.
func (s *State) Turn(utterance string, cues []string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeLocked() {
		s.lastTurn = s.now()
		return Mode
	}
	if HasCue(utterance, cues) {
		return Cue
	}
	return ""
}

// activeLocked reports whether the mode is on, expiring it after the idle
// window. The caller holds the lock.
func (s *State) activeLocked() bool {
	if !s.active {
		return false
	}
	var idle time.Duration
	if s.idle != nil {
		idle = s.idle()
	}
	if idle > 0 && s.now().Sub(s.lastTurn) >= idle {
		s.active = false
		return false
	}
	return true
}

// HasCue reports whether the utterance contains one of the cue words as
// normalised whole words. A multi-word cue must appear as a contiguous run, so
// "why" matches "why is the sky blue" but not "show me whyever".
func HasCue(utterance string, cues []string) bool {
	words := tokenize(utterance)
	if len(words) == 0 {
		return false
	}
	for _, cue := range cues {
		wanted := tokenize(cue)
		if len(wanted) == 0 {
			continue
		}
		if containsRun(words, wanted) {
			return true
		}
	}
	return false
}

// tokenize splits normalised text into lowercase words. Punctuation is not a
// word, so "explain, please" still carries the cue "explain".
func tokenize(text string) []string {
	return strings.FieldsFunc(phrase.Normalize(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// containsRun reports whether want appears as a contiguous run in words.
func containsRun(words, want []string) bool {
	if len(want) > len(words) {
		return false
	}
	for i := 0; i+len(want) <= len(words); i++ {
		matched := true
		for j, token := range want {
			if words[i+j] != token {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
