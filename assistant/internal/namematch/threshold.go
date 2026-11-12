package namematch

import (
	"fmt"
	"sync"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// KeyThreshold is the settings key of the live similarity floor.
const KeyThreshold = "name_match.threshold"

// Reader is the settings read seam. *settings.Service satisfies it.
type Reader interface {
	Float(key string) float64
}

// Threshold is the live floor shared by every Matcher. It is created before
// the settings service exists because Tool modules capture Deps during
// registration, then bound after settings.New. Get is safe before Bind and
// returns the fallback. It deliberately does not install a settings observer:
// the wording manager owns the single observer, so the threshold is read per
// match instead.
type Threshold struct {
	mu       sync.RWMutex
	reader   Reader
	fallback float64
}

// NewThreshold captures the fallback used before Bind or when the setting is
// unreadable. A value outside (0, 1] uses DefaultThreshold.
func NewThreshold(fallback float64) *Threshold {
	if fallback <= 0 || fallback > 1 {
		fallback = DefaultThreshold
	}
	return &Threshold{fallback: fallback}
}

// Bind attaches the shared settings service. The value is read on every Get,
// so a Dashboard or voice write applies on the next lookup without a restart.
func (t *Threshold) Bind(reader Reader) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.reader = reader
	t.mu.Unlock()
}

// Get returns the effective threshold, or the fallback when the setting is
// missing, mistyped or out of range.
func (t *Threshold) Get() float64 {
	if t == nil {
		return DefaultThreshold
	}
	t.mu.RLock()
	reader, fallback := t.reader, t.fallback
	t.mu.RUnlock()
	if reader != nil {
		if value := reader.Float(KeyThreshold); value > 0 && value <= 1 {
			return value
		}
	}
	return fallback
}

// Definition registers the threshold with the same settings service the
// Dashboard and voice Tools write through. A zero config value means "unset"
// and leaves the code default; any other invalid value fails settings.New.
func Definition(configValue float64) settings.Definition {
	opts := []settings.Option{
		settings.WithKind(settings.Live),
		settings.WithValidator(validateThreshold),
		settings.WithDescription("Minimum cosine similarity for a spoken name to resolve to a library entry; in (0, 1]."),
	}
	if configValue != 0 {
		opts = append(opts, settings.WithConfig(configValue))
	}
	return settings.Float(KeyThreshold, DefaultThreshold, opts...)
}

// validateThreshold keeps a floor that would match everything or nothing out
// of the store and out of config.yaml.
func validateThreshold(value any) error {
	threshold, ok := value.(float64)
	if !ok {
		return fmt.Errorf("want a number")
	}
	if threshold <= 0 || threshold > 1 {
		return fmt.Errorf("must be in (0, 1]")
	}
	return nil
}
