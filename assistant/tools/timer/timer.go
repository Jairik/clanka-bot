// Package timer implements the timer.set, timer.cancel and timer.list Tools.
package timer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	SetName    = "timer.set"
	CancelName = "timer.cancel"
	ListName   = "timer.list"
)

// AlertText is spoken when a timer fires. It reuses the reply Speaker.
const AlertText = "Your timer is done."

// alertTimeout bounds one alert so a stuck TTS service cannot leak a goroutine.
const alertTimeout = 30 * time.Second

// Timer is one running or recently set timer.
type Timer struct {
	ID               string    `json:"id"`
	DurationSeconds  int       `json:"duration_seconds"`
	RemainingSeconds int       `json:"remaining_seconds"`
	FiresAt          time.Time `json:"fires_at"`
}

type entry struct {
	timer Timer
	stop  *time.Timer
}

// Manager owns the running timers and fires their alert.
type Manager struct {
	mu        sync.Mutex
	now       func() time.Time
	say       func(ctx context.Context, text string) error
	logger    *slog.Logger
	alertText string
	seq       int
	entries   map[string]*entry
}

// NewManager builds an empty manager with the default alert phrase. A nil now
// uses time.Now; a nil sayer still fires but logs that the alert was dropped.
func NewManager(now func() time.Time, say func(ctx context.Context, text string) error, logger *slog.Logger) *Manager {
	return NewManagerText(now, say, logger, AlertText)
}

// NewManagerText builds an empty manager with a configured alert phrase.
func NewManagerText(now func() time.Time, say func(ctx context.Context, text string) error, logger *slog.Logger, alertText string) *Manager {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(alertText) == "" {
		alertText = AlertText
	}
	return &Manager{now: now, say: say, logger: logger, alertText: alertText, entries: make(map[string]*entry)}
}

// Set starts a timer and returns it.
func (m *Manager) Set(d time.Duration) Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := strconv.Itoa(m.seq)
	now := m.now()
	t := Timer{
		ID:               id,
		DurationSeconds:  int(d.Round(time.Second) / time.Second),
		RemainingSeconds: int(d.Round(time.Second) / time.Second),
		FiresAt:          now.Add(d),
	}
	m.entries[id] = &entry{
		timer: t,
		stop:  time.AfterFunc(d, func() { m.fire(id) }),
	}
	m.logger.Info("timer set", "timer_id", id, "duration_s", t.DurationSeconds)
	return t
}

// fire removes the timer and speaks its alert.
func (m *Manager) fire(id string) {
	m.mu.Lock()
	if _, ok := m.entries[id]; !ok {
		m.mu.Unlock()
		return
	}
	delete(m.entries, id)
	say := m.say
	m.mu.Unlock()

	m.logger.Info("timer fired", "timer_id", id)
	if say == nil {
		m.logger.Warn("timer alert dropped: speech is not configured", "timer_id", id)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), alertTimeout)
	defer cancel()
	if err := say(ctx, m.alertText); err != nil {
		m.logger.Error("timer alert", "timer_id", id, "error", err.Error())
	}
}

// List returns the active timers, soonest first.
func (m *Manager) List() []Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	out := make([]Timer, 0, len(m.entries))
	for _, e := range m.entries {
		t := e.timer
		remaining := t.FiresAt.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		t.RemainingSeconds = int(remaining.Round(time.Second) / time.Second)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FiresAt.Before(out[j].FiresAt) })
	return out
}

// Cancel stops one timer by id, or every timer when id is empty. It returns
// how many were cancelled.
func (m *Manager) Cancel(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cancelled := 0
	for key, e := range m.entries {
		if id != "" && key != id {
			continue
		}
		e.stop.Stop()
		delete(m.entries, key)
		cancelled++
	}
	if cancelled > 0 {
		m.logger.Info("timer cancelled", "timer_id", id, "count", cancelled)
	}
	return cancelled
}

// SetIn asks for a timer length.
type SetIn struct {
	DurationSeconds int `json:"duration_seconds"`
}

// SetOut is the timer that was created.
type SetOut = Timer

// CancelIn optionally names one timer; empty cancels them all.
type CancelIn struct {
	ID string `json:"id,omitempty"`
}

// CancelOut reports how many timers were cancelled.
type CancelOut struct {
	Cancelled int `json:"cancelled"`
}

// ListIn has no arguments.
type ListIn struct{}

// ListOut holds the active timers.
type ListOut struct {
	Timers []Timer `json:"timers"`
}

// NewSet builds timer.set.
func NewSet(m *Manager) tool.Tool[SetIn, SetOut] {
	return tool.Tool[SetIn, SetOut]{
		Name:        SetName,
		Description: "Start a countdown timer.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in SetIn) (SetOut, error) {
			if in.DurationSeconds <= 0 {
				return SetOut{}, &tool.ToolError{Say: "How long should the timer be?"}
			}
			return m.Set(time.Duration(in.DurationSeconds) * time.Second), nil
		},
		Speak: func(out SetOut) string {
			return "Timer set for " + parse.HumanDuration(time.Duration(out.DurationSeconds)*time.Second) + "."
		},
		Examples: []string{
			"set a timer for five minutes",
			"set a timer for 10 minutes",
			"start a timer for an hour",
			"set a timer for an hour and a half",
			"timer for 30 seconds",
		},
		Extract: func(text string) (SetIn, bool) {
			d, ok := parse.Duration(text, time.Minute)
			if !ok || d <= 0 {
				return SetIn{}, false
			}
			return SetIn{DurationSeconds: int(d.Round(time.Second) / time.Second)}, true
		},
	}
}

// NewCancel builds timer.cancel.
func NewCancel(m *Manager) tool.Tool[CancelIn, CancelOut] {
	return tool.Tool[CancelIn, CancelOut]{
		Name:        CancelName,
		Description: "Cancel the running timer, or one timer by id.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in CancelIn) (CancelOut, error) {
			return CancelOut{Cancelled: m.Cancel(in.ID)}, nil
		},
		Speak: func(out CancelOut) string {
			if out.Cancelled == 0 {
				return "No timer to cancel."
			}
			return "Timer cancelled."
		},
		Examples: []string{
			"cancel the timer",
			"cancel my timer",
			"stop the timer",
			"cancel it",
		},
		Extract: func(string) (CancelIn, bool) { return CancelIn{}, true },
	}
}

// NewList builds timer.list.
func NewList(m *Manager) tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the running timers and how long is left.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, ListIn) (ListOut, error) {
			return ListOut{Timers: m.List()}, nil
		},
		Speak: func(out ListOut) string {
			switch len(out.Timers) {
			case 0:
				return "No timers are running."
			case 1:
				return "Your timer has " + parse.HumanDuration(time.Duration(out.Timers[0].RemainingSeconds)*time.Second) + " left."
			default:
				next := parse.HumanDuration(time.Duration(out.Timers[0].RemainingSeconds) * time.Second)
				return fmt.Sprintf("You have %d timers. The next one has %s left.", len(out.Timers), next)
			}
		},
		Examples: []string{
			"how long is left on my timer",
			"how much time is left",
			"what timers do I have",
			"list my timers",
		},
	}
}

// Register adds all three timer Tools.
func Register(r *tool.Registry, m *Manager) error {
	if err := tool.Add(r, NewSet(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewCancel(m)); err != nil {
		return err
	}
	return tool.Add(r, NewList(m))
}
