// Package voice provides the microphone state machine and speech pipeline.
package voice

import (
	"fmt"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

const (
	// ListeningKey is the live boolean setting that gates wake-word detection.
	// True by default; set to false for an indefinite pause.
	ListeningKey = "wakeword.listening"

	// ListeningUntilKey is the live string setting storing an RFC3339 timestamp
	// for a timed pause. Empty by default; when set to a future timestamp,
	// wake detection is paused until that time passes.
	ListeningUntilKey = "wakeword.listening_until"
)

// ListeningDefinitions returns the settings definitions for wakeword.listening
// and wakeword.listening_until. Both are ordinary live settings.
func ListeningDefinitions() []settings.Definition {
	return []settings.Definition{
		settings.Bool(ListeningKey, true,
			settings.WithDescription("Wake-word listening enabled. Paused from the Dashboard; resumes automatically after a timed pause or on Dashboard resume."),
		),
		settings.String(ListeningUntilKey, "",
			settings.WithValidator(validateListeningUntil),
			settings.WithDescription("End of a timed pause as an RFC3339 timestamp; empty means indefinite pause or active listening."),
		),
	}
}

// validateListeningUntil ensures the timestamp is either empty or a valid RFC3339 string.
func validateListeningUntil(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("wakeword: listening_until must be a string")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, text); err != nil {
		return fmt.Errorf("wakeword: listening_until must be an RFC3339 time: %w", err)
	}
	return nil
}

// IsListening reports whether wake-word listening is currently active.
// The effective state is: listening && !(listening_until is in the future).
// A stale past timestamp simply stops mattering; it is never cleared on read.
func IsListening(listening bool, until string, now time.Time) bool {
	if !listening {
		return false
	}
	if strings.TrimSpace(until) == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return true
	}
	return !t.After(now)
}

// SettingsReader reads boolean and string settings from a settings snapshot.
type SettingsReader interface {
	Bool(key string) bool
	String(key string) string
}

// ListeningState computes the effective listening state and returns whether
// wake detection is active and the current until timestamp.
func ListeningState(r SettingsReader, now time.Time) (bool, string) {
	if r == nil {
		return true, ""
	}
	listening := r.Bool(ListeningKey)
	until := r.String(ListeningUntilKey)
	active := IsListening(listening, until, now)
	return active, until
}
