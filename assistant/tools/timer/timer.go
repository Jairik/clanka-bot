// Package timer implements the timer.set, timer.cancel, timer.remaining and
// timer.list Tools. Timers are persisted in the one SQLite store when the
// module receives one, and survive a restart (ADR-0003).
package timer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	SetName       = "timer.set"
	CancelName    = "timer.cancel"
	RemainingName = "timer.remaining"
	ListName      = "timer.list"
)

// AlertText is spoken when a timer fires. It reuses the reply Speaker.
const AlertText = "Your timer is done."

// StopText and CancelText are spoken by the reply turn that stops a ringing
// alert; the alert itself is cancelled rather than spoken over.
const (
	StopText   = "Timer stopped."
	CancelText = "Timer cancelled."
)

// alertIntents is the fixed set a ringing timer alert handles. A Timer's
// "snooze" is #52 Ring behaviour, so it is deliberately absent here.
func alertIntents() []focus.Intent { return []focus.Intent{focus.Stop, focus.Cancel} }

// alertTimeout bounds one alert so a stuck TTS service cannot leak a goroutine.
const alertTimeout = 30 * time.Second

// Timer is one running or recently set timer.
type Timer struct {
	ID               string    `json:"id"`
	Label            string    `json:"label,omitempty"`
	DurationSeconds  int       `json:"duration_seconds"`
	RemainingSeconds int       `json:"remaining_seconds"`
	FiresAt          time.Time `json:"fires_at"`
}

// entry is one live timer and its callback.
type entry struct {
	timer Timer
	stop  *time.Timer
}

// Manager owns the running timers and fires their alert. A Manager without a
// store is the original in-memory implementation, so existing callers keep
// working; AttachStore adds durability.
type Manager struct {
	mu        sync.Mutex
	now       func() time.Time
	say       func(ctx context.Context, text string) error
	logger    *slog.Logger
	alertText string

	db            *sql.DB
	announcements *announce.Queue
	focus         *focus.Registry
	closed        bool

	seq     int
	entries map[string]*entry
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

// WithFocus makes a ringing alert hold Focus, so a shared "stop" or "cancel"
// reaches the alert instead of the current reply or music. It returns the
// manager so a module can wire it in one line.
func (m *Manager) WithFocus(registry *focus.Registry) *Manager {
	m.mu.Lock()
	m.focus = registry
	m.mu.Unlock()
	return m
}

// alertHolder is the ringing alert's Focus holder. It handles stop and cancel
// by cancelling the alert's speech context; the stop turn says the line.
type alertHolder struct {
	cancel context.CancelFunc
	done   atomic.Bool
}

func (h *alertHolder) Intents() []focus.Intent { return alertIntents() }

func (h *alertHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	switch intent {
	case focus.Stop, focus.Cancel:
	default:
		return "", false
	}
	if !h.done.CompareAndSwap(false, true) {
		return "", false
	}
	h.cancel()
	if intent == focus.Cancel {
		return CancelText, true
	}
	return StopText, true
}

// Set starts an unlabelled timer and returns it. It is the original in-memory
// API; a store write failure is logged because this signature has no error. New
// callers use Start.
func (m *Manager) Set(d time.Duration) Timer {
	timer, err := m.Start("", d)
	if err != nil {
		m.logger.Error("timer set", "error", err.Error())
		return Timer{}
	}
	return timer
}

// Start persists (when a store is attached) and schedules one timer.
func (m *Manager) Start(label string, d time.Duration) (Timer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Timer{}, errors.New("timer: manager is closed")
	}
	now := m.now()
	label = cleanLabel(label)
	timer := Timer{
		Label:            label,
		DurationSeconds:  int(d.Round(time.Second) / time.Second),
		RemainingSeconds: int(d.Round(time.Second) / time.Second),
		FiresAt:          now.Add(d),
	}

	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		id, err := m.insert(ctx, label, d, timer.FiresAt, now)
		if err != nil {
			return Timer{}, err
		}
		timer.ID = id
	} else {
		m.seq++
		timer.ID = strconv.Itoa(m.seq)
	}

	m.entries[timer.ID] = &entry{
		timer: timer,
		stop:  time.AfterFunc(d, func() { m.fire(timer.ID) }),
	}
	m.logger.Info("timer set", "timer_id", timer.ID, "duration_s", timer.DurationSeconds, "label", timer.Label)
	return timer, nil
}

// fire removes the timer, deletes its persisted row, then speaks its alert.
// The row goes first so a restart cannot fire the same timer twice. While the
// alert sounds it holds Focus at Ring priority, so "stop" or "cancel" reaches
// it before the current reply or music.
func (m *Manager) fire(id string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok || m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.entries, id)
	say := m.say
	focusRegistry := m.focus
	db := m.db
	m.mu.Unlock()

	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		if err := m.deleteRows(ctx, []string{id}); err != nil {
			m.logger.Error("timer delete after fire", "timer_id", id, "error", err.Error())
		}
		cancel()
	}

	m.alert(e.timer, say, focusRegistry)
}

// alert speaks one timer's done line through the shared speaker seam. While it
// sounds, the alert holds Focus at Ring priority so a shared phrase reaches it.
func (m *Manager) alert(timer Timer, say func(ctx context.Context, text string) error, focusRegistry *focus.Registry) {
	m.logger.Info("timer fired", "timer_id", timer.ID, "label", timer.Label)
	if say == nil {
		m.logger.Warn("timer alert dropped: speech is not configured", "timer_id", timer.ID)
		return
	}
	text := m.alertText
	if timer.Label != "" {
		text = capitalize(timer.Label) + " timer is done."
	}
	ctx, cancel := context.WithTimeout(context.Background(), alertTimeout)
	defer cancel()
	if focusRegistry != nil {
		holder := &alertHolder{cancel: cancel}
		claim := focusRegistry.Claim(focus.Ring, holder)
		defer func() {
			// Finish before releasing so a racing intent declines rather than
			// cancelling a finished alert.
			holder.done.Store(true)
			claim.Release()
		}()
	}
	if err := say(ctx, text); err != nil && ctx.Err() == nil {
		m.logger.Error("timer alert", "timer_id", timer.ID, "error", err.Error())
	}
}

// capitalize upper-cases the first rune of a spoken label without cutting a
// multi-byte character.
func capitalize(label string) string {
	if label == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(label)
	return strings.ToUpper(string(first)) + label[size:]
}

// List returns the active timers, soonest first.
func (m *Manager) List() []Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	out := make([]Timer, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, remainingAt(e.timer, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FiresAt.Before(out[j].FiresAt) })
	return out
}

// Remaining returns the soonest active timer, or the soonest one whose label
// matches when a label is given.
func (m *Manager) Remaining(label string) (Timer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	label = cleanLabel(label)
	var best Timer
	found := false
	for _, e := range m.entries {
		if label != "" && e.timer.Label != label {
			continue
		}
		if found && !e.timer.FiresAt.Before(best.FiresAt) {
			continue
		}
		best = remainingAt(e.timer, now)
		found = true
	}
	return best, found
}

// remainingAt copies one timer with its remaining seconds at now.
func remainingAt(timer Timer, now time.Time) Timer {
	remaining := timer.FiresAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	timer.RemainingSeconds = int(remaining.Round(time.Second) / time.Second)
	return timer
}

// Cancel stops one timer by id, or every timer when id is empty. It returns
// how many were cancelled; a persistence failure is logged because this
// signature has no error. New callers use CancelTimers.
func (m *Manager) Cancel(id string) int {
	cancelled, err := m.CancelTimers(id)
	if err != nil {
		m.logger.Error("timer cancel", "timer_id", id, "error", err.Error())
	}
	return cancelled
}

// CancelTimers cancels by id, or every timer when id is empty. The persisted
// rows are deleted before the live map changes, so a database failure leaves
// the timer running rather than silently losing it.
func (m *Manager) CancelTimers(id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for key := range m.entries {
		if id != "" && key != id {
			continue
		}
		ids = append(ids, key)
	}
	return m.cancelLocked(ids, "timer_id", id)
}

// CancelLabel stops every timer with the given label and reports how many.
func (m *Manager) CancelLabel(label string) (int, error) {
	label = cleanLabel(label)
	if label == "" {
		return m.CancelTimers("")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for key, e := range m.entries {
		if e.timer.Label == label {
			ids = append(ids, key)
		}
	}
	return m.cancelLocked(ids, "label", label)
}

// cancelLocked removes the persisted rows then the live entries. The caller
// holds the write lock, so a callback cannot fire between the two steps.
func (m *Manager) cancelLocked(ids []string, attr, value string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		if err := m.deleteRows(ctx, ids); err != nil {
			return 0, err
		}
	}
	for _, key := range ids {
		if e, ok := m.entries[key]; ok {
			e.stop.Stop()
			delete(m.entries, key)
		}
	}
	m.logger.Info("timer cancelled", attr, value, "count", len(ids))
	return len(ids), nil
}

// Close stops every callback without deleting the persisted rows. It models a
// clean process shutdown: a restart recovers the Timers.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for id, e := range m.entries {
		e.stop.Stop()
		delete(m.entries, id)
	}
}

// SetIn asks for a timer length and an optional label.
type SetIn struct {
	DurationSeconds int    `json:"duration_seconds"`
	Label           string `json:"label,omitempty"`
}

// SetOut is the timer that was created.
type SetOut = Timer

// CancelIn optionally names one timer; empty cancels them all.
type CancelIn struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
}

// CancelOut reports how many timers were cancelled.
type CancelOut struct {
	Cancelled int `json:"cancelled"`
}

// RemainingIn optionally names the timer to look up.
type RemainingIn struct {
	Label string `json:"label,omitempty"`
}

// RemainingOut is the timer whose remaining time was asked for.
type RemainingOut struct {
	Timer Timer `json:"timer"`
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
		Description: "Start a countdown timer, optionally with a label.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in SetIn) (SetOut, error) {
			if in.DurationSeconds <= 0 {
				return SetOut{}, &tool.ToolError{Say: "How long should the timer be?"}
			}
			return m.Start(in.Label, time.Duration(in.DurationSeconds)*time.Second)
		},
		Speak: func(out SetOut) string {
			duration := parse.HumanDuration(time.Duration(out.DurationSeconds) * time.Second)
			if out.Label != "" {
				return capitalize(out.Label) + " timer set for " + duration + "."
			}
			return "Timer set for " + duration + "."
		},
		Examples: []string{
			"set a timer for five minutes",
			"set a timer for 10 minutes",
			"start a timer for an hour",
			"set a timer for an hour and a half",
			"timer for 30 seconds",
			// A labelled phrasing proves the name is carried through Extract
			// without making the label itself a routing signal.
			"set a laundry timer for 10 minutes",
		},
		Extract: func(text string) (SetIn, bool) {
			d, ok := parse.Duration(text, time.Minute)
			if !ok || d <= 0 {
				return SetIn{}, false
			}
			return SetIn{
				DurationSeconds: int(d.Round(time.Second) / time.Second),
				Label:           labelForTimer(text),
			}, true
		},
	}
}

// NewCancel builds timer.cancel.
func NewCancel(m *Manager) tool.Tool[CancelIn, CancelOut] {
	return tool.Tool[CancelIn, CancelOut]{
		Name:        CancelName,
		Description: "Cancel the running timer, or one timer by id or label.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in CancelIn) (CancelOut, error) {
			if in.ID == "" && in.Label != "" {
				cancelled, err := m.CancelLabel(in.Label)
				return CancelOut{Cancelled: cancelled}, err
			}
			cancelled, err := m.CancelTimers(in.ID)
			return CancelOut{Cancelled: cancelled}, err
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
			"cancel the kettle timer",
			"cancel it",
			"cancel",
		},
		Extract: func(text string) (CancelIn, bool) {
			return CancelIn{Label: labelForTimer(text)}, true
		},
	}
}

// NewRemaining builds timer.remaining.
func NewRemaining(m *Manager) tool.Tool[RemainingIn, RemainingOut] {
	return tool.Tool[RemainingIn, RemainingOut]{
		Name:        RemainingName,
		Description: "Say how long is left on a timer, optionally named by label.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in RemainingIn) (RemainingOut, error) {
			timer, ok := m.Remaining(in.Label)
			if !ok {
				if in.Label != "" {
					return RemainingOut{}, &tool.ToolError{Say: "There is no " + in.Label + " timer."}
				}
				return RemainingOut{}, &tool.ToolError{Say: "No timers are running."}
			}
			return RemainingOut{Timer: timer}, nil
		},
		Speak: func(out RemainingOut) string {
			left := parse.HumanDuration(time.Duration(out.Timer.RemainingSeconds) * time.Second)
			if out.Timer.Label != "" {
				return "The " + out.Timer.Label + " timer has " + left + " left."
			}
			return "Your timer has " + left + " left."
		},
		Examples: []string{
			"how long is left on the timer",
			"how much time is left on the timer",
			"how long does the timer have left",
			"what's left on the timer",
			"how long until the timer goes off",
			"when does the timer finish",
			"how much longer on the timer",
			"how much longer does the timer have",
		},
		Extract: func(text string) (RemainingIn, bool) {
			return RemainingIn{Label: labelForTimer(text)}, true
		},
	}
}

// NewList builds timer.list.
func NewList(m *Manager) tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the running timers and how long is left.",
		Permission:  tool.PermissionRead,
		// The list names the user's timers, which can include private labels, so
		// it never leaves the Pi: the Agent path cannot see or call it.
		LocalOnly: tool.LocalOnlyYes,
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
			"what timers do I have",
			"list my timers",
			"show me my timers",
			"what timers are running",
			"how many timers are running",
			"do I have any timers",
			"what timers are active",
			"show all my timers",
		},
	}
}

// Register adds every timer Tool.
func Register(r *tool.Registry, m *Manager) error {
	if err := tool.Add(r, NewSet(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewCancel(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewRemaining(m)); err != nil {
		return err
	}
	return tool.Add(r, NewList(m))
}
