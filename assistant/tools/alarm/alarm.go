// Package alarm implements the alarm.set, alarm.list and alarm.cancel Tools.
// An Alarm is a clock-time Scheduled alert that may repeat daily, on weekdays,
// on weekends or on chosen weekdays. It Rings through the shared internal/ring
// engine with the Alarm settings (a 30 s ramp by default) and supports snooze
// (9 min by default). Alarms are persisted in the one SQLite store when the
// module receives one and follow the same two-minute downtime rule as Timers.
package alarm

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
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
)

// Canonical Tool names.
const (
	SetName    = "alarm.set"
	ListName   = "alarm.list"
	CancelName = "alarm.cancel"
)

// SnoozeText is the line spoken by the turn that snoozes a ringing Alarm.
const SnoozeText = "Alarm snoozed."

const (
	// overdueGrace is the downtime rule shared with Timers: an Alarm overdue by
	// this much or less Rings at startup; an older occurrence is only
	// summarised in one Announcement.
	overdueGrace = 2 * time.Minute
	// storeTimeout bounds the small local reads and writes.
	storeTimeout = 5 * time.Second
	// alertTimeout bounds the spoken fallback when no Ring engine is attached.
	alertTimeout = 30 * time.Second
	// defaultFallback is spoken when an unlabelled Alarm has no Ring sound.
	defaultFallback = "Your alarm is going off."
)

// Alarm is one configured clock-time alert. Sound, Volume and RampSeconds are
// stored for #57's per-Alarm overrides but no Tool sets them yet; a nil volume
// or ramp means "use the kind default", so an explicit 0 volume stays distinct.
type Alarm struct {
	ID          string    `json:"id"`
	Label       string    `json:"label,omitempty"`
	Hour        int       `json:"hour"`
	Minute      int       `json:"minute"`
	Schedule    Schedule  `json:"schedule"`
	Weekdays    []string  `json:"weekdays,omitempty"`
	NextFiresAt time.Time `json:"next_fires_at"`
	Sound       string    `json:"sound,omitempty"`
	Volume      *int      `json:"volume,omitempty"`
	RampSeconds *int      `json:"ramp_seconds,omitempty"`
}

// SetIn is one alarm.set request. Schedule is once, daily, weekdays, weekends
// or chosen; a non-empty Weekdays list selects chosen. Tomorrow forces the
// one-off occurrence to the next local day even when the clock time is future.
type SetIn struct {
	Hour     int      `json:"hour"`
	Minute   int      `json:"minute,omitempty"`
	Schedule string   `json:"schedule,omitempty"`
	Weekdays []string `json:"weekdays,omitempty"`
	Label    string   `json:"label,omitempty"`
	Tomorrow bool     `json:"tomorrow,omitempty"`
}

// SetOut is the Alarm that was created.
type SetOut = Alarm

// CancelIn names the Alarm to cancel by id, spoken label or clock time. Empty
// fields cancel every Alarm.
type CancelIn struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	Time  string `json:"time,omitempty"`
}

// CancelOut reports how many Alarms were cancelled.
type CancelOut struct {
	Cancelled int `json:"cancelled"`
}

// ListIn has no arguments.
type ListIn struct{}

// ListOut holds the configured Alarms, soonest first.
type ListOut struct {
	Alarms []Alarm `json:"alarms"`
}

// MissedAlert is the notification hook payload for an Alarm that ran to
// max_ring, expired in downtime or was still ringing after a crash. #62 wires
// ntfy through SetMissedHook; this package only reports the fact.
type MissedAlert struct {
	Kind    string
	Label   string
	FiresAt time.Time
	FiredAt time.Time
	EndedAt time.Time
	Reason  string
}

// Options are the Manager's seams. A nil Now uses time.Now, a nil Zone means
// UTC, and a nil Snooze reads the live ring.alarm.snooze setting.
type Options struct {
	Now           func() time.Time
	Say           func(context.Context, string) error
	Logger        *slog.Logger
	Zone          *tz.Service
	Settings      func() *settings.Service
	Announcements *announce.Queue
	Snooze        func() time.Duration
}

// entry is one live Alarm and its pending callback.
type entry struct {
	alarm      Alarm
	occurrence time.Time
	snoozed    bool
	timer      *time.Timer
}

// Manager owns the configured Alarms and fires their Ring. A Manager without a
// store is in-memory; AttachStore adds durability and the downtime rule.
type Manager struct {
	mu     sync.Mutex
	now    func() time.Time
	say    func(context.Context, string) error
	logger *slog.Logger
	zone   *tz.Service
	// settings is late-bound because Tool modules register before main creates
	// the live settings service. The snooze delay is read per snooze action.
	settings      func() *settings.Service
	announcements *announce.Queue
	snooze        func() time.Duration

	db      *sql.DB
	history *alertstore.Store
	engine  *ring.Engine
	missed  func(MissedAlert)

	closed bool
	// ctx is cancelled by Close so an active Ring and queued callbacks stop.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks an in-flight fire so Close can wait for its final history write.
	wg      sync.WaitGroup
	seq     int
	entries map[string]*entry
	// cancelled remembers ids removed while a fire was in flight, so the fire
	// cannot schedule a ghost occurrence after its Ring ends.
	cancelled map[string]bool
	// active holds the cancel function of each Ring that owns the speaker, so
	// alarm.cancel can stop it and record stopped/cancel.
	active map[string]context.CancelFunc
}

// NewManager builds an empty manager.
func NewManager(opts Options) *Manager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		now:           now,
		say:           opts.Say,
		logger:        logger,
		zone:          opts.Zone,
		settings:      opts.Settings,
		announcements: opts.Announcements,
		snooze:        opts.Snooze,
		ctx:           ctx,
		cancel:        cancel,
		entries:       make(map[string]*entry),
		cancelled:     make(map[string]bool),
		active:        make(map[string]context.CancelFunc),
	}
}

// SetRingEngine attaches the shared #52 Ring engine. It must be called before
// Start's recovery so an overdue Alarm that fires immediately already Rings.
func (m *Manager) SetRingEngine(engine *ring.Engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.engine = engine
}

// SetMissedHook installs the later Notification seam (#62). It is called after
// the missed state is durable, with the alert's identity and reason.
func (m *Manager) SetMissedHook(hook func(MissedAlert)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.missed = hook
}

// location reads the live timezone for this scheduling call. It is never
// snapshotted at registration, so a Dashboard or voice change applies to the
// next occurrence computed after it.
func (m *Manager) location() *time.Location {
	if m.zone != nil {
		return m.zone.Location()
	}
	return time.UTC
}

// Set persists (when a store is attached) and schedules one Alarm. The next
// occurrence is computed with the live timezone at this call.
func (m *Manager) Set(ctx context.Context, in SetIn) (Alarm, error) {
	if in.Hour < 0 || in.Hour > 23 {
		return Alarm{}, &tool.ToolError{Say: "An alarm needs an hour from 0 to 23."}
	}
	if in.Minute < 0 || in.Minute > 59 {
		return Alarm{}, &tool.ToolError{Say: "An alarm needs a minute from 0 to 59."}
	}
	spec, weekdays, err := normalizeSchedule(in.Schedule, in.Weekdays)
	if err != nil {
		return Alarm{}, &tool.ToolError{Say: capitalize(err.Error()) + "."}
	}
	now := m.now()
	loc := m.location()
	next := nextOnce(now, in.Hour, in.Minute, loc, in.Tomorrow)
	if spec.schedule != Once {
		next = nextOccurrence(spec, in.Hour, in.Minute, loc, now)
	}
	alarm := Alarm{
		Label:       cleanAlarmLabel(in.Label),
		Hour:        in.Hour,
		Minute:      in.Minute,
		Schedule:    spec.schedule,
		Weekdays:    weekdays,
		NextFiresAt: next,
	}
	if m.db != nil {
		id, err := m.insert(ctx, alarm, spec.days, now)
		if err != nil {
			return Alarm{}, err
		}
		alarm.ID = id
	} else {
		m.mu.Lock()
		m.seq++
		alarm.ID = strconv.Itoa(m.seq)
		m.mu.Unlock()
	}
	m.schedule(alarm, next, false)
	m.logger.Info("alarm set",
		"alarm_id", alarm.ID,
		"label", alarm.Label,
		"time", formatClock(alarm.Hour, alarm.Minute),
		"schedule", alarm.Schedule,
		"next_fires_at", next)
	return alarm, nil
}

// List returns the configured Alarms, soonest first. It reads the store when
// one is attached, so a row that is firing or waiting on a snooze is still
// described accurately; an in-memory Manager lists its live entries.
func (m *Manager) List() []Alarm {
	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		stored, err := m.readAlarms(ctx)
		if err != nil {
			m.logger.Error("alarm list", "error", err.Error())
			return nil
		}
		out := make([]Alarm, 0, len(stored))
		for _, saved := range stored {
			if saved.nextFiresAt.UnixMilli() <= 0 && saved.snoozedUntil.IsZero() {
				continue // a one-off that already fired and is finishing its Ring
			}
			out = append(out, saved.alarm)
		}
		sortAlarms(out)
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Alarm, 0, len(m.entries))
	for _, e := range m.entries {
		alarm := e.alarm
		alarm.NextFiresAt = e.occurrence
		out = append(out, alarm)
	}
	sortAlarms(out)
	return out
}

// sortAlarms orders the soonest next fire first.
func sortAlarms(alarms []Alarm) {
	sort.Slice(alarms, func(i, j int) bool {
		return alarms[i].NextFiresAt.Before(alarms[j].NextFiresAt)
	})
}

// Cancel removes the Alarms selected by in and reports how many were removed.
// An Alarm that is currently Ringing is stopped and recorded stopped/cancel.
func (m *Manager) Cancel(ctx context.Context, in CancelIn) (int, error) {
	var match func(Alarm) bool
	switch {
	case strings.TrimSpace(in.ID) != "":
		id := strings.TrimSpace(in.ID)
		match = func(alarm Alarm) bool { return alarm.ID == id }
	case strings.TrimSpace(in.Time) != "":
		hour, minute, ok := parse.Clock(in.Time)
		if !ok {
			return 0, &tool.ToolError{Say: "Which alarm time should I cancel?"}
		}
		match = func(alarm Alarm) bool { return alarm.Hour == hour && alarm.Minute == minute }
	case strings.TrimSpace(in.Label) != "":
		label := cleanAlarmLabel(in.Label)
		if label == "" {
			return 0, &tool.ToolError{Say: "Which alarm should I cancel?"}
		}
		match = func(alarm Alarm) bool { return alarm.Label == label }
	default:
		match = func(Alarm) bool { return true }
	}
	return m.cancelWhere(ctx, match)
}

// CancelAll removes every Alarm. It is the in-memory and test-friendly form of
// an empty alarm.cancel request.
func (m *Manager) CancelAll() int {
	cancelled, err := m.cancelWhere(context.Background(), func(Alarm) bool { return true })
	if err != nil {
		m.logger.Error("alarm cancel all", "error", err.Error())
	}
	return cancelled
}

// cancelWhere finds matching rows, deletes them, then removes their live
// entries and stops an in-flight Ring. The cancelled set closes the window
// where a fire that already removed its entry tries to schedule its next
// occurrence after the row was deleted.
func (m *Manager) cancelWhere(ctx context.Context, match func(Alarm) bool) (int, error) {
	var ids []string
	if m.db != nil {
		readCtx, cancel := context.WithTimeout(ctx, storeTimeout)
		stored, err := m.readAlarms(readCtx)
		cancel()
		if err != nil {
			return 0, err
		}
		for _, saved := range stored {
			if match(saved.alarm) {
				ids = append(ids, saved.alarm.ID)
			}
		}
		if len(ids) == 0 {
			return 0, nil
		}
		if err := m.deleteRows(ctx, ids); err != nil {
			return 0, err
		}
	} else {
		m.mu.Lock()
		for id, e := range m.entries {
			if match(e.alarm) {
				ids = append(ids, id)
			}
		}
		m.mu.Unlock()
		if len(ids) == 0 {
			return 0, nil
		}
	}

	m.mu.Lock()
	for _, id := range ids {
		m.cancelled[id] = true
		if e := m.entries[id]; e != nil {
			e.timer.Stop()
			delete(m.entries, id)
		}
		if cancel := m.active[id]; cancel != nil {
			cancel()
		}
	}
	m.mu.Unlock()
	m.logger.Info("alarm cancelled", "count", len(ids))
	return len(ids), nil
}

// Close stops every callback and cancels an active Ring without deleting the
// persisted rows. It waits for an in-flight fire to record its outcome, so the
// caller may close the database afterwards. Pending Alarms are recovered on the
// next start, while an interrupted Ring is recorded as such.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	for id, e := range m.entries {
		e.timer.Stop()
		delete(m.entries, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// schedule puts one Alarm occurrence in the live map and starts its callback.
// It replaces any older callback for the same id, so a snooze cannot leave the
// next repeat occurrence running at the same time.
func (m *Manager) schedule(alarm Alarm, occurrence time.Time, snoozed bool) {
	delay := occurrence.Sub(m.now())
	if delay < 0 {
		delay = 0
	}
	m.mu.Lock()
	if m.closed || m.cancelled[alarm.ID] {
		m.mu.Unlock()
		return
	}
	if old := m.entries[alarm.ID]; old != nil {
		old.timer.Stop()
	}
	id := alarm.ID
	m.entries[id] = &entry{
		alarm:      alarm,
		occurrence: occurrence,
		snoozed:    snoozed,
		timer:      time.AfterFunc(delay, func() { m.runFire(id, snoozed) }),
	}
	m.mu.Unlock()
}

// runFire registers one callback with Close's wait group, so a shutdown waits
// for the final history write before the database closes.
func (m *Manager) runFire(id string, snoozed bool) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	m.fire(id, snoozed)
}

// fire removes the entry, atomically advances or deletes its pending row and
// opens the history row, then Rings it. The transaction makes a duplicate fire
// impossible across a crash and keeps the next repeat occurrence durable.
func (m *Manager) fire(id string, snoozed bool) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok || m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.entries, id)
	captured := e.alarm
	capturedOccurrence := e.occurrence
	m.mu.Unlock()

	var state fireState
	if m.db == nil {
		// In-memory Manager: the removed entry is the authoritative schedule.
		state = fireState{alarm: captured, occurrence: capturedOccurrence}
		if !snoozed && captured.Schedule != Once {
			next := nextOccurrence(scheduleSpec{schedule: captured.Schedule, days: weekdayMaskOf(captured.Weekdays)},
				captured.Hour, captured.Minute, m.location(), capturedOccurrence)
			state.next = &next
		}
	} else {
		var err error
		state, ok, err = m.beginFire(id, snoozed)
		if err != nil {
			m.logger.Error("alarm fire history", "alarm_id", id, "error", err.Error())
			return
		}
		if !ok {
			// Cancelled or already fired; the row is authoritative.
			return
		}
	}
	alarm := state.alarm
	if alarm.ID == "" {
		alarm = captured
	}

	ringCtx, ringCancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		ringCancel()
		m.finishEvent(state.eventID, alertstore.StateInterrupted, "shutdown")
		return
	}
	if m.cancelled[id] {
		m.mu.Unlock()
		ringCancel()
		m.finishEvent(state.eventID, alertstore.StateStopped, "cancel")
		// The row was deleted by alarm.cancel; scheduling the next occurrence
		// here would resurrect a cancelled Alarm.
		return
	}
	m.active[id] = ringCancel
	m.mu.Unlock()
	defer func() {
		ringCancel()
		m.mu.Lock()
		delete(m.active, id)
		m.mu.Unlock()
	}()

	if m.engine != nil {
		result := m.engine.Ring(ringCtx, ring.Request{
			Kind:         ring.Alarm,
			ID:           id,
			Label:        alarm.Label,
			FiresAt:      state.occurrence,
			FallbackText: fallbackText(alarm),
			AllowSnooze:  true,
			SnoozeText:   SnoozeText,
			OnStart:      func() { m.markRinging(state.eventID) },
		})
		m.finishRing(id, state, result)
		return
	}

	// No Ring engine: speak the single fallback line, still bounded by Close.
	m.markRinging(state.eventID)
	m.speakFallback(ringCtx, alarm)
	result := ring.Result{Played: true, Outcome: ring.OutcomeStopped, EndedBy: "spoken"}
	if ringCtx.Err() != nil {
		// Close cancelled the line; record a clean shutdown, not a completed
		// alert.
		result = ring.Result{Outcome: ring.OutcomeInterrupted, EndedBy: "shutdown"}
	}
	m.finishRing(id, state, result)
}

// speakFallback is the no-engine path: one spoken line, cancelled by Close.
func (m *Manager) speakFallback(ctx context.Context, alarm Alarm) {
	if m.say == nil {
		m.logger.Warn("alarm alert dropped: speech is not configured", "alarm_id", alarm.ID)
		return
	}
	sayCtx, cancel := context.WithTimeout(ctx, alertTimeout)
	defer cancel()
	if err := m.say(sayCtx, fallbackText(alarm)); err != nil && sayCtx.Err() == nil {
		m.logger.Error("alarm alert", "alarm_id", alarm.ID, "error", err.Error())
	}
}

// finishRing records the Ring outcome and decides whether to snooze or to
// continue the repeat schedule. A cancelled Alarm schedules nothing.
func (m *Manager) finishRing(id string, state fireState, result ring.Result) {
	cancelled := m.isCancelled(id)
	outcome := result.Outcome
	endedBy := result.EndedBy
	if cancelled && outcome == ring.OutcomeInterrupted {
		// alarm.cancel cancelled the Ring context; record the user's intent
		// rather than a shutdown.
		outcome = ring.OutcomeStopped
		endedBy = "cancel"
	}
	recorded := alertstore.StateStopped
	switch outcome {
	case ring.OutcomeMissed:
		recorded = alertstore.StateMissed
	case ring.OutcomeInterrupted:
		recorded = alertstore.StateInterrupted
	}
	if endedBy == "" {
		endedBy = "stopped"
	}
	if !result.Played && recorded == alertstore.StateStopped {
		endedBy = "unavailable"
	}
	m.finishEvent(state.eventID, recorded, endedBy)
	if recorded == alertstore.StateMissed {
		m.notifyMissed(state.alarm, state.occurrence, "max_ring")
	}
	if cancelled || outcome == ring.OutcomeInterrupted {
		return
	}
	if endedBy == "snooze" {
		m.scheduleSnooze(state.alarm)
		return
	}
	m.afterFire(state)
}

// afterFire continues a repeating Alarm, or removes a finished one-off row.
func (m *Manager) afterFire(state fireState) {
	if state.alarm.Schedule == Once {
		m.deleteDoneRow(state.alarm.ID)
		return
	}
	if state.next != nil {
		m.schedule(state.alarm, *state.next, false)
	}
}

// scheduleSnooze persists and schedules one re-ring after the live snooze
// delay. It keeps the row, so a restart during the snooze still re-rings.
func (m *Manager) scheduleSnooze(alarm Alarm) {
	target := m.now().Add(m.snoozeDuration())
	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		result, err := m.db.ExecContext(ctx,
			`UPDATE alarms SET snoozed_until = ? WHERE id = ?`, target.UnixMilli(), alarm.ID)
		cancel()
		if err != nil {
			m.logger.Error("alarm snooze", "alarm_id", alarm.ID, "error", err.Error())
			return
		}
		if rows, err := result.RowsAffected(); err == nil && rows == 0 {
			return // cancelled while the Ring was ending
		}
	}
	if m.isCancelled(alarm.ID) {
		return
	}
	m.schedule(alarm, target, true)
	m.logger.Info("alarm snoozed", "alarm_id", alarm.ID, "until", target)
}

// snoozeDuration reads the live setting, falling back to the code default.
func (m *Manager) snoozeDuration() time.Duration {
	if m.snooze != nil {
		if d := m.snooze(); d > 0 {
			return d
		}
	}
	if m.settings != nil {
		if svc := m.settings(); svc != nil {
			if d := ring.ReadSettings(svc, ring.Alarm).Snooze; d > 0 {
				return d
			}
		}
	}
	return ring.DefaultAlarmSnooze
}

// markRinging records that the Alarm now owns the speaker.
func (m *Manager) markRinging(eventID int64) {
	if eventID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := m.history.MarkRinging(ctx, eventID, m.now()); err != nil {
		m.logger.Error("alarm history ringing", "event_id", eventID, "error", err.Error())
	}
}

// finishEvent closes one open history row.
func (m *Manager) finishEvent(eventID int64, state alertstore.State, endedBy string) {
	if eventID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := m.history.Finish(ctx, eventID, state, endedBy, m.now()); err != nil {
		m.logger.Error("alarm history finish", "event_id", eventID, "state", state, "error", err.Error())
	}
}

// notifyMissed reports one max_ring Alarm after its history row is durable.
func (m *Manager) notifyMissed(alarm Alarm, occurrence time.Time, reason string) {
	m.mu.Lock()
	hook := m.missed
	m.mu.Unlock()
	if hook == nil {
		return
	}
	hook(MissedAlert{
		Kind:    "alarm",
		Label:   alarm.Label,
		FiresAt: occurrence,
		FiredAt: occurrence,
		EndedAt: m.now(),
		Reason:  reason,
	})
}

// notifyMissedEvent reports one already-persisted recovered missed row.
func (m *Manager) notifyMissedEvent(event alertstore.Event, reason string) {
	m.mu.Lock()
	hook := m.missed
	m.mu.Unlock()
	if hook == nil {
		return
	}
	hook(MissedAlert{
		Kind:    event.Kind,
		Label:   event.Label,
		FiresAt: event.FiresAt,
		FiredAt: event.FiredAt,
		EndedAt: event.EndedAt,
		Reason:  reason,
	})
}

// History returns the newest alert-history rows. It is the read seam for tests
// and the later Dashboard/Notification work.
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

// isCancelled reports whether id was removed while its fire was in flight.
func (m *Manager) isCancelled(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled[id]
}

// fallbackText is the spoken line used when there is no Ring sound.
func fallbackText(alarm Alarm) string {
	if alarm.Label != "" {
		return capitalize(alarm.Label) + " alarm is going off."
	}
	return defaultFallback
}

// capitalize upper-cases the first rune without cutting a multi-byte one.
func capitalize(text string) string {
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	return strings.ToUpper(string(first)) + text[size:]
}

// insert persists one Alarm and returns its database id.
func (m *Manager) insert(ctx context.Context, alarm Alarm, days uint8, createdAt time.Time) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`INSERT INTO alarms
		 (label, hour, minute, schedule, weekdays, next_fires_at, created_at, sound, volume, ramp_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		alarm.Label,
		alarm.Hour,
		alarm.Minute,
		string(alarm.Schedule),
		int64(days),
		alarm.NextFiresAt.UnixMilli(),
		createdAt.UnixMilli(),
		alarm.Sound,
		nullableInt(alarm.Volume),
		nullableSeconds(alarm.RampSeconds))
	if err != nil {
		return "", fmt.Errorf("alarm: save alarm: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("alarm: saved alarm id: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// nullableInt stores an absent override as SQL NULL.
func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// nullableSeconds stores a ramp override as milliseconds, or NULL when unset.
func nullableSeconds(value *int) any {
	if value == nil {
		return nil
	}
	return int64(*value) * int64(time.Second/time.Millisecond)
}

// deleteRows removes persisted Alarms in one transaction.
func (m *Manager) deleteRows(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("alarm: begin delete: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM alarms WHERE id = ?`, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("alarm: delete alarm %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("alarm: commit delete: %w", err)
	}
	return nil
}

// deleteDoneRow removes a one-off row only when it has no pending snooze, so a
// concurrent snooze cannot be deleted by the fire that just ended.
func (m *Manager) deleteDoneRow(id string) {
	if m.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM alarms WHERE id = ? AND next_fires_at = 0 AND snoozed_until IS NULL`, id); err != nil {
		m.logger.Error("alarm delete finished", "alarm_id", id, "error", err.Error())
	}
}

// fireState is one fire's durable identity and next repeat occurrence.
type fireState struct {
	eventID    int64
	alarm      Alarm
	occurrence time.Time
	next       *time.Time
}

// errNotStored is returned when AttachStore is called twice.
var errNotStored = errors.New("alarm: store is already attached")
