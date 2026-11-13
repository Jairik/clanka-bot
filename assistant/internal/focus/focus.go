// Package focus routes shared Control-phrase intents to whatever currently
// holds the user's attention: a ringing Scheduled alert first, then the
// current reply, then playing music. The order is fixed in code.
package focus

import (
	"context"
	"strings"
	"sync"
)

// Intent is one shared Control-phrase action from the fixed vocabulary.
type Intent string

const (
	Stop   Intent = "stop"
	Pause  Intent = "pause"
	Resume Intent = "resume"
	Next   Intent = "next"
	Cancel Intent = "cancel"
	Snooze Intent = "snooze"
)

// Intents returns the fixed vocabulary in the order the spec lists it.
func Intents() []Intent {
	return []Intent{Stop, Pause, Resume, Next, Cancel, Snooze}
}

// ParseIntent normalises text and reports whether it is a Focus intent. The
// second result is false for a typo, so config validation can reject it.
func ParseIntent(text string) (Intent, bool) {
	normalized := Intent(strings.ToLower(strings.TrimSpace(text)))
	for _, intent := range Intents() {
		if normalized == intent {
			return intent, true
		}
	}
	return "", false
}

// IsIntentTarget reports whether a Control phrase value names a shared Focus
// intent rather than a Tool. Canonical Tool names always contain one dot
// ("module.tool"), so a value without a dot can only be an intent.
func IsIntentTarget(value string) bool {
	return !strings.Contains(strings.TrimSpace(value), ".")
}

// Priority is the fixed Focus order. Lower values win.
type Priority int

const (
	// Ring is a ringing Scheduled alert: a Timer or Alarm going off.
	Ring Priority = iota
	// Reply is the Assistant's current spoken reply.
	Reply
	// Music is music playing on the Pi.
	Music
)

// String names the priority for logs and tests.
func (p Priority) String() string {
	switch p {
	case Ring:
		return "ring"
	case Reply:
		return "reply"
	case Music:
		return "music"
	default:
		return "unknown"
	}
}

// Holder is one activity that can take Focus and handle some intents. A
// holder declares only what it can really do: the reply cannot pause, while
// music can. HandleIntent reports false when the holder finished between the
// claim and the intent, so a lower holder still gets a chance.
type Holder interface {
	Intents() []Intent
	HandleIntent(ctx context.Context, intent Intent) (string, bool)
}

// holderEntry is one priority slot. token makes release ownership explicit: a
// late release from an old turn must not clear the holder that replaced it.
type holderEntry struct {
	holder Holder
	token  uint64
}

// Registry tracks the current holder at each priority. The zero value is ready
// and every method is safe for concurrent use.
type Registry struct {
	// OnReleased is installed before use and observes completed music/reply lifetimes.
	OnReleased func(Priority)
	mu         sync.Mutex
	seq        uint64
	holders    [3]holderEntry
}

// New returns an empty Focus registry. The zero value works too; this exists
// so wiring reads as one thing.
func New() *Registry { return &Registry{} }

// Claim makes holder the Focus holder at priority p and returns the token that
// releases it. A newer claim at the same priority wins; releasing an older
// token is a no-op. A nil holder claims nothing.
func (r *Registry) Claim(p Priority, holder Holder) *Claim {
	if r == nil || holder == nil || p < Ring || p > Music {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.holders[p] = holderEntry{holder: holder, token: r.seq}
	return &Claim{registry: r, priority: p, token: r.seq}
}

// Held reports whether anything currently holds Focus at priority p. The
// Announcement queue uses it to wait while a Ring is audible; it reads the
// slot only and never calls a holder.
func (r *Registry) Held(p Priority) bool {
	if r == nil || p < Ring || p > Music {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holders[p].holder != nil
}

// HandleIntent offers intent to the holders in fixed priority order and
// returns the first handled result plus its optional spoken text. It reports
// false when nothing holds Focus or no holder declares the intent.
func (r *Registry) HandleIntent(ctx context.Context, intent Intent) (string, bool) {
	if r == nil {
		return "", false
	}
	// Snapshot first: a holder may cancel a turn that releases its own claim,
	// so the registry lock must not be held while a holder runs.
	order := make([]Holder, 0, len(r.holders))
	r.mu.Lock()
	for _, entry := range r.holders {
		if entry.holder != nil {
			order = append(order, entry.holder)
		}
	}
	r.mu.Unlock()

	for _, holder := range order {
		if !declares(holder, intent) {
			continue
		}
		if text, ok := holder.HandleIntent(ctx, intent); ok {
			return text, true
		}
	}
	return "", false
}

// declares reports whether holder handles intent.
func declares(holder Holder, intent Intent) bool {
	for _, declared := range holder.Intents() {
		if declared == intent {
			return true
		}
	}
	return false
}

// Claim is one holder's ownership token. Release is nil-safe and idempotent.
type Claim struct {
	registry *Registry
	priority Priority
	token    uint64
	once     sync.Once
}

// Release clears this claim if it is still the current holder at its priority.
func (c *Claim) Release() {
	if c == nil || c.registry == nil {
		return
	}
	c.once.Do(func() {
		c.registry.mu.Lock()
		entry := c.registry.holders[c.priority]
		if entry.token == c.token {
			c.registry.holders[c.priority] = holderEntry{}
			callback := c.registry.OnReleased
			c.registry.mu.Unlock()
			if callback != nil {
				callback(c.priority)
			}
			return
		}
		c.registry.mu.Unlock()
	})
}
