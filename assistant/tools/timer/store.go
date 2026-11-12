package timer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// overdueGrace is the downtime rule: a Timer overdue by this much or less goes
// off at startup; an older one is only summarised in one Announcement.
const overdueGrace = 2 * time.Minute

// storeTimeout bounds the startup read and the small writes. The database is
// local, so a slow query means it is wedged, not busy.
const storeTimeout = 5 * time.Second

// AttachStore makes the Manager durable. It first resolves any alert a
// previous process left open as missed, then loads the Timers a previous run
// left behind, applies the downtime rule, and schedules the future ones.
// Announcements receives the single summary for Timers that expired long
// before startup; it may be nil, in which case that summary is only logged.
//
// The pending Timer row is deleted in the same transaction that opens its
// history row, so a restart can never fire it twice and still knows it fired.
func (m *Manager) AttachStore(st *store.Store, announcements *announce.Queue) error {
	if st == nil || st.DB() == nil {
		return nil // in-memory Manager; existing callers and tests stay valid
	}
	m.mu.Lock()
	if m.db != nil {
		m.mu.Unlock()
		return errors.New("timer: store is already attached")
	}
	m.db = st.DB()
	m.history = alertstore.New(st.DB())
	m.announcements = announcements
	m.mu.Unlock()
	return m.recover()
}

// recover reads the persisted Timers and schedules them. It must run once,
// after AttachStore has installed the handle and before serving.
func (m *Manager) recover() error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	// A previous process may have crashed while an alert was fired or ringing.
	// Resolve those open history rows first: they are missed, and the hook can
	// tell #62. They are never rung again, preserving at-most-once delivery.
	open, err := m.history.Recover(ctx, m.now())
	if err != nil {
		return err
	}
	for _, event := range open {
		m.logger.Warn("timer ring interrupted by restart; recorded missed",
			"event_id", event.ID, "label", event.Label, "fires_at", event.FiresAt)
		m.notifyMissedEvent(event, "restart")
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, label, duration_ms, fires_at FROM timers ORDER BY fires_at, id`)
	if err != nil {
		return fmt.Errorf("timer: read saved timers: %w", err)
	}
	type saved struct {
		id       string
		label    string
		duration time.Duration
		firesAt  time.Time
	}
	var timers []saved
	for rows.Next() {
		var (
			id         int64
			label      string
			durationMS int64
			firesMS    int64
		)
		if err := rows.Scan(&id, &label, &durationMS, &firesMS); err != nil {
			rows.Close()
			return fmt.Errorf("timer: read saved timer: %w", err)
		}
		timers = append(timers, saved{
			id:       strconv.FormatInt(id, 10),
			label:    label,
			duration: time.Duration(durationMS) * time.Millisecond,
			firesAt:  time.UnixMilli(firesMS),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("timer: read saved timers: %w", err)
	}
	rows.Close()

	now := m.now()
	var missed []Timer
	for _, saved := range timers {
		savedTimer := Timer{
			ID:              saved.id,
			Label:           saved.label,
			DurationSeconds: int(saved.duration.Round(time.Second) / time.Second),
			FiresAt:         saved.firesAt,
		}
		if !saved.firesAt.After(now) {
			if overdue := now.Sub(saved.firesAt); overdue > overdueGrace {
				// Too old to ring: remove the pending row and record one
				// durable missed history row in the same transaction.
				if err := m.retireMissed(ctx, savedTimer, now, "downtime"); err != nil {
					return err
				}
				missed = append(missed, savedTimer)
				continue
			}
			// Within the grace window: ring now. The row stays until the fire
			// transaction replaces it with the history row, so a crash before
			// the callback cannot lose the Timer without a record.
			m.schedule(savedTimer, 0)
			continue
		}
		m.schedule(savedTimer, saved.firesAt.Sub(now))
	}

	if len(missed) > 0 {
		m.announceMissed(missed)
	}
	return nil
}

// schedule puts one Timer in the live map and starts its one-shot callback.
func (m *Manager) schedule(saved Timer, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	id := saved.ID
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.entries[id] = &entry{
		timer: saved,
		stop:  time.AfterFunc(delay, func() { m.runFire(id) }),
	}
	m.mu.Unlock()
}

// retireMissed deletes an overdue pending row and writes its missed history row
// in one transaction, then reports it to the notification hook. An in-memory
// Manager cannot reach here because recover only runs with a store.
func (m *Manager) retireMissed(ctx context.Context, timer Timer, endedAt time.Time, reason string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timer: begin downtime retire: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM timers WHERE id = ?`, timer.ID); err != nil {
		tx.Rollback()
		return fmt.Errorf("timer: delete overdue timer %s: %w", timer.ID, err)
	}
	event := alertstore.Event{
		Kind:       "timer",
		SourceID:   timer.ID,
		Label:      timer.Label,
		DurationMS: int64(timer.DurationSeconds) * int64(time.Second/time.Millisecond),
		FiresAt:    timer.FiresAt,
		FiredAt:    timer.FiresAt,
		State:      alertstore.StateMissed,
		EndedAt:    endedAt,
		EndedBy:    reason,
	}
	if _, err := m.history.InsertTx(ctx, tx, event); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timer: commit downtime retire: %w", err)
	}
	m.notifyMissedEvent(event, reason)
	return nil
}

// notifyMissedEvent reports one already-persisted missed history row.
func (m *Manager) notifyMissedEvent(event alertstore.Event, reason string) {
	m.mu.Lock()
	hook := m.missed
	m.mu.Unlock()
	if hook == nil {
		return
	}
	hook(MissedAlert{
		Kind:            event.Kind,
		Label:           event.Label,
		DurationSeconds: int(time.Duration(event.DurationMS) * time.Millisecond / time.Second),
		FiresAt:         event.FiresAt,
		FiredAt:         event.FiredAt,
		EndedAt:         event.EndedAt,
		Reason:          reason,
	})
}

// announceMissed queues the one summary Announcement for Timers that expired
// beyond the downtime grace. Labels make the summary useful; the duration is
// the fallback for an unlabelled Timer.
func (m *Manager) announceMissed(missed []Timer) {
	text := missedSummary(missed)
	if m.announcements != nil && m.announcements.Enqueue(text) {
		m.logger.Info("timer downtime summary queued", "timers", len(missed))
		return
	}
	// No speech seam is not a reason to keep retrying at every startup; the
	// log line is the headless record that these Timers expired.
	m.logger.Warn("timer downtime summary dropped: speech is not configured",
		"timers", len(missed), "summary", text)
}

// missedSummary renders one line naming the Timers that expired during
// downtime. It never lists more than the labels or durations it has.
func missedSummary(missed []Timer) string {
	names := make([]string, 0, len(missed))
	for _, timer := range missed {
		names = append(names, describeTimer(timer))
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "You missed a timer: " + names[0] + "."
	default:
		return fmt.Sprintf("You missed %d timers: %s.", len(names), strings.Join(names, ", "))
	}
}

// describeTimer is the short spoken name for one Timer.
func describeTimer(timer Timer) string {
	if timer.Label != "" {
		return timer.Label
	}
	return parse.HumanDuration(time.Duration(timer.DurationSeconds) * time.Second)
}

// insert writes one new Timer row and returns its database id.
func (m *Manager) insert(ctx context.Context, label string, duration time.Duration, firesAt, createdAt time.Time) (string, error) {
	result, err := m.db.ExecContext(ctx,
		`INSERT INTO timers (label, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?)`,
		label,
		duration.Milliseconds(),
		firesAt.UnixMilli(),
		createdAt.UnixMilli())
	if err != nil {
		return "", fmt.Errorf("timer: save timer: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("timer: saved timer id: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// deleteRows removes persisted Timers. A missing row is not an error: a fired
// and a cancelled path may both reach the same id.
func (m *Manager) deleteRows(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timer: begin delete: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM timers WHERE id = ?`, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("timer: delete timer %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timer: commit delete: %w", err)
	}
	return nil
}
