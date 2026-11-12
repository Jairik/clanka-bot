// Package timer implements the timer.set, timer.cancel, timer.remaining and
// timer.list Tools. Timers are persisted in the one SQLite store when the
// module receives one, and survive a restart (ADR-0003). When a Timer fires it
// Rings through internal/ring: a looping Cue with a durable fired/ringing/
// stopped/missed history row.
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

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
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

// alertIntents is the fixed set a ringing Timer handles. "snooze" on a Timer
// acts as stop, so the same holder consumes it; #53 gives an Alarm the real
// snooze behaviour.
func alertIntents() []focus.Intent { return []focus.Intent{focus.Stop, focus.Cancel, focus.Snooze} }

// alertTimeout bounds one spoken fallback alert so a stuck TTS service cannot
// leak a goroutine. A Ring bounds itself with its max_ring setting.
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
	history       *alertstore.Store
	announcements *announce.Queue
	focus         *focus.Registry
	ring          func(context.Context, Timer) bool
	ringEngine    *ring.Engine
	missed        func(MissedAlert)
	closed        bool

	// ctx is cancelled by Close so an active Ring and queued alert callbacks
	// stop promptly. It is not a deadline: the Ring owns max_ring.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks an in-flight fire so Close can wait for its history row and
	// let the caller close the database afterwards.
	wg      sync.WaitGroup
	seq     int
	entries map[string]*entry

	// active holds one cancellable Ring context per fired Timer, keyed by the
	// id its history row uses, so a cancel can end a Ring after the pending row
	// and live entry are already gone. ringCancelled distinguishes a dashboard
	// cancel from a shutdown for the history outcome.
	active        map[string]context.CancelFunc
	ringCancelled map[string]bool
}

// MissedAlert is the notification hook payload for a Timer that ran to
// max_ring, expired in downtime or was still ringing after a crash. #62 wires
// ntfy through SetMissedHook; this package only reports the fact.
type MissedAlert struct {
	Kind            string
	Label           string
	DurationSeconds int
	FiresAt         time.Time
	FiredAt         time.Time
	EndedAt         time.Time
	Reason          string
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
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		now:           now,
		say:           say,
		logger:        logger,
		alertText:     alertText,
		ctx:           ctx,
		cancel:        cancel,
		entries:       make(map[string]*entry),
		active:        make(map[string]context.CancelFunc),
		ringCancelled: make(map[string]bool),
	}
}

// SetRing installs the raw #55 Ring callback that runs when a Timer fires. It
// reports whether it handled the alert; returning false keeps the configured
// spoken line. The context is cancelled by a Focus stop, cancel or Timer
// snooze, so a looping Ring can stop its sound.
//
// This callback is not serialised with another fire; the production path is
// SetRingEngine, which owns the one-speaker queue and Focus. A custom callback
// that plays audio must serialise itself.
func (m *Manager) SetRing(ring func(context.Context, Timer) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ring = ring
}

// SetRingEngine attaches the #52 Ring engine. It takes precedence over a raw
// SetRing callback: the engine owns serialisation, Focus, the reply boundary,
// music ducking and max_ring, and Manager records the Ring lifecycle around
// it. SetRing stays available for a lightweight custom alert and the #55
// contract tests.
func (m *Manager) SetRingEngine(engine *ring.Engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ringEngine = engine
}

// SetMissedHook installs the later Notification seam (#62). It is called after
// the missed state is durable, with the alert's identity and reason.
func (m *Manager) SetMissedHook(hook func(MissedAlert)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.missed = hook
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

// alertHolder is the fallback spoken alert's Focus holder. It handles stop,
// cancel and snooze by cancelling the speech; the stop turn says the line. The
// Ring engine has its own equivalent holder so it can report the exact reason.
type alertHolder struct {
	cancel context.CancelFunc
	done   atomic.Bool

	mu     sync.Mutex
	reason string
}

func (h *alertHolder) Intents() []focus.Intent { return alertIntents() }

func (h *alertHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	switch intent {
	case focus.Stop, focus.Cancel, focus.Snooze:
	default:
		return "", false
	}
	if !h.done.CompareAndSwap(false, true) {
		return "", false
	}
	h.mu.Lock()
	switch intent {
	case focus.Cancel:
		h.reason = "cancel"
	case focus.Snooze:
		h.reason = "snooze"
	default:
		h.reason = "stop"
	}
	h.mu.Unlock()
	h.cancel()
	if intent == focus.Cancel {
		return CancelText, true
	}
	return StopText, true
}

// endedBy reports the intent that stopped the fallback alert, if any.
func (h *alertHolder) endedBy() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
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
		stop:  time.AfterFunc(d, func() { m.runFire(timer.ID) }),
	}
	m.logger.Info("timer set", "timer_id", timer.ID, "duration_s", timer.DurationSeconds, "label", timer.Label)
	return timer, nil
}

// runFire registers one callback with Close's wait group. It is the only path
// from an AfterFunc into fire, so a shutdown can wait for the final history
// write before the database closes.
func (m *Manager) runFire(id string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	m.fire(id)
}

// fire removes the timer, atomically replaces its pending row with an open
// alert-history row, then Rings it. The delete happens in the same transaction
// as the history insert, so a crash can never fire the same Timer twice and
// still leaves a record that it fired.
func (m *Manager) fire(id string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok || m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.entries, id)
	ringFn := m.ring
	engine := m.ringEngine
	say := m.say
	focusRegistry := m.focus
	db := m.db
	rootCtx := m.ctx
	fallback := m.alertText
	m.mu.Unlock()

	timer := e.timer
	eventID := int64(0)
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		var err error
		eventID, err = m.beginFire(ctx, timer)
		cancel()
		if err != nil {
			// A history failure must not drop the alert. The pending row is
			// left behind only when the transaction rolled back; the manager
			// still sounds the Timer once now.
			m.logger.Error("timer fire history", "timer_id", timer.ID, "error", err.Error())
		}
	}

	// One cancellable context per fired Timer lets a page or voice cancel end
	// a Ring whose pending row and live entry are already gone.
	ringCtx, ringCancel := context.WithCancel(rootCtx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		ringCancel()
		m.finishEvent(eventID, alertstore.StateInterrupted, "shutdown")
		return
	}
	m.active[timer.ID] = ringCancel
	m.mu.Unlock()
	defer func() {
		ringCancel()
		m.mu.Lock()
		delete(m.active, timer.ID)
		delete(m.ringCancelled, timer.ID)
		m.mu.Unlock()
	}()

	if engine != nil {
		result := engine.Ring(ringCtx, ring.Request{
			Kind:         ring.Timer,
			ID:           timer.ID,
			Label:        timer.Label,
			Duration:     time.Duration(timer.DurationSeconds) * time.Second,
			FiresAt:      timer.FiresAt,
			FallbackText: fallbackText(fallback, timer),
			OnStart:      func() { m.markRinging(eventID) },
		})
		if m.releaseRing(timer.ID) {
			result = ring.Result{Played: result.Played, Outcome: ring.OutcomeStopped, EndedBy: "cancel"}
		}
		m.finishRing(eventID, timer, result)
		return
	}

	m.markRinging(eventID)
	endedBy, _ := m.alert(ringCtx, timer, ringFn, say, focusRegistry)
	if m.releaseRing(timer.ID) {
		endedBy = "cancel"
	}
	m.finishEvent(eventID, alertstore.StateStopped, endedBy)
}

// releaseRing removes the active Ring for id and reports whether a cancel
// asked for it, so a cancelled Ring records stopped/cancel instead of a
// shutdown.
func (m *Manager) releaseRing(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, id)
	cancelled := m.ringCancelled[id]
	delete(m.ringCancelled, id)
	return cancelled
}

// alert hands one firing Timer to the raw #55 Ring seam, then falls back to the
// single spoken line. While either sounds it holds Focus at Ring priority.
func (m *Manager) alert(parent context.Context, timer Timer, ringFn func(context.Context, Timer) bool, say func(ctx context.Context, text string) error, focusRegistry *focus.Registry) (endedBy string, rang bool) {
	m.logger.Info("timer fired", "timer_id", timer.ID, "label", timer.Label)
	if ringFn == nil && say == nil {
		m.logger.Warn("timer alert dropped: speech is not configured", "timer_id", timer.ID)
		return "dropped", false
	}
	ctx, cancel := context.WithTimeout(parent, alertTimeout)
	defer cancel()
	var holder *alertHolder
	if focusRegistry != nil {
		holder = &alertHolder{cancel: cancel}
		claim := focusRegistry.Claim(focus.Ring, holder)
		defer func() {
			// Finish before releasing so a racing intent declines rather than
			// cancelling a finished alert.
			holder.done.Store(true)
			claim.Release()
		}()
	}
	reason := func(fallback string) string {
		if holder != nil {
			if ended := holder.endedBy(); ended != "" {
				return ended
			}
		}
		return fallback
	}
	if ringFn != nil && ringFn(ctx, timer) {
		m.logger.Info("timer rung", "timer_id", timer.ID, "label", timer.Label)
		return reason("rung"), true
	}
	if say == nil {
		m.logger.Warn("timer alert dropped: speech is not configured", "timer_id", timer.ID)
		return reason("dropped"), false
	}
	text := fallbackText(m.alertText, timer)
	if err := say(ctx, text); err != nil && ctx.Err() == nil {
		m.logger.Error("timer alert", "timer_id", timer.ID, "error", err.Error())
	}
	return reason("spoken"), true
}

// fallbackText is the short spoken line used when there is no Ring sound.
func fallbackText(alertText string, timer Timer) string {
	if timer.Label != "" {
		return capitalize(timer.Label) + " timer is done."
	}
	return alertText
}

// beginFire deletes the pending Timer row and inserts its open history row in
// one transaction. The row id is zero for an in-memory Manager.
func (m *Manager) beginFire(ctx context.Context, timer Timer) (int64, error) {
	if m.db == nil {
		return 0, nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("timer: begin fire: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM timers WHERE id = ?`, timer.ID); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("timer: delete fired timer %s: %w", timer.ID, err)
	}
	event := alertstore.Event{
		Kind:       "timer",
		SourceID:   timer.ID,
		Label:      timer.Label,
		DurationMS: int64(timer.DurationSeconds) * int64(time.Second/time.Millisecond),
		FiresAt:    timer.FiresAt,
		FiredAt:    m.now(),
		State:      alertstore.StateFired,
	}
	id, err := m.history.InsertTx(ctx, tx, event)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("timer: commit fire: %w", err)
	}
	return id, nil
}

// markRinging records that the alert now owns the speaker.
func (m *Manager) markRinging(eventID int64) {
	if eventID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := m.history.MarkRinging(ctx, eventID, m.now()); err != nil {
		m.logger.Error("timer history ringing", "event_id", eventID, "error", err.Error())
	}
}

// finishRing records the Ring engine's outcome and fires the missed hook.
func (m *Manager) finishRing(eventID int64, timer Timer, result ring.Result) {
	state := alertstore.StateStopped
	switch result.Outcome {
	case ring.OutcomeMissed:
		state = alertstore.StateMissed
	case ring.OutcomeInterrupted:
		state = alertstore.StateInterrupted
	}
	endedBy := result.EndedBy
	if endedBy == "" {
		endedBy = "stopped"
	}
	if !result.Played && state == alertstore.StateStopped {
		endedBy = "unavailable"
	}
	m.finishEvent(eventID, state, endedBy)
	if state == alertstore.StateMissed {
		m.notifyMissed(timer, "max_ring")
	}
}

// finishEvent closes one open history row. A zero id is the in-memory case.
func (m *Manager) finishEvent(eventID int64, state alertstore.State, endedBy string) {
	if eventID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := m.history.Finish(ctx, eventID, state, endedBy, m.now()); err != nil {
		m.logger.Error("timer history finish", "event_id", eventID, "state", state, "error", err.Error())
	}
}

// notifyMissed reports one max_ring alert. The history row is already durable
// when this runs; #62 can send its Notification from this hook.
func (m *Manager) notifyMissed(timer Timer, reason string) {
	m.notifyMissedEvent(alertstore.Event{
		Kind:       "timer",
		SourceID:   timer.ID,
		Label:      timer.Label,
		DurationMS: int64(timer.DurationSeconds) * int64(time.Second/time.Millisecond),
		FiresAt:    timer.FiresAt,
		FiredAt:    timer.FiresAt,
		EndedAt:    m.now(),
	}, reason)
}

// History returns the newest alert-history rows, including missed and stopped
// alerts from previous runs. It is the read seam for tests and the later
// Dashboard/Notification work.
func (m *Manager) History(limit int) ([]alertstore.Event, error) {
	m.mu.Lock()
	history := m.history
	m.mu.Unlock()
	if history == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	return history.List(ctx, limit)
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
	cancelled, err := m.cancelLocked(ids, "timer_id", id)
	if err != nil {
		return cancelled, err
	}
	if id != "" {
		if cancel := m.active[id]; cancel != nil {
			// A fired Timer has no pending row or live entry; mark the Ring so
			// its history row records stopped/cancel, then end it now.
			m.ringCancelled[id] = true
			cancel()
			cancelled++
			m.logger.Info("timer ring cancelled", "timer_id", id)
		}
	}
	return cancelled, nil
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

// Close stops every callback and cancels an active Ring without deleting the
// persisted rows. It waits for an in-flight fire to record its outcome, so the
// caller may close the database afterwards. It models a clean process
// shutdown: pending Timers are recovered on restart, while an interrupted Ring
// is recorded as such.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	for id, e := range m.entries {
		e.stop.Stop()
		delete(m.entries, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
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
