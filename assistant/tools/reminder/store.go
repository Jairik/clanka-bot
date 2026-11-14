package reminder

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// errAlreadyGone means the pending row was removed before the fire transaction
// reached it, so the Reminder must not be spoken.
var errAlreadyGone = errors.New("reminder: pending row is already gone")

// AttachStore makes the Manager durable. It first resolves any Reminder a
// previous process left open as missed, then loads the rows a previous run left
// behind: overdue by at most two minutes speaks now, older ones become one
// summary Announcement, and future ones are rescheduled. AttachStore may be
// called once; a nil store keeps the in-memory Manager.
func (m *Manager) AttachStore(st *store.Store) error {
	if st == nil || st.DB() == nil {
		return nil // in-memory Manager; existing callers and tests stay valid
	}
	m.mu.Lock()
	if m.db != nil {
		m.mu.Unlock()
		return errors.New("reminder: store is already attached")
	}
	m.db = st.DB()
	m.history = alertstore.New(st.DB())
	m.mu.Unlock()
	return m.recover()
}

// recover reads the persisted Reminders and schedules them.
func (m *Manager) recover() error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	// A previous process may have crashed after the fire transaction opened a
	// history row but before the alert finished. It is missed and is never
	// spoken again: at-most-once is the promise across a restart.
	open, err := m.history.RecoverKind(ctx, "reminder", m.now())
	if err != nil {
		return err
	}
	for _, event := range open {
		// No message in the log; the history row keeps it for the #58 page.
		m.logger.Warn("reminder interrupted by restart; recorded missed",
			"event_id", event.ID, "fires_at", event.FiresAt)
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, message, kind, duration_ms, fires_at FROM reminders ORDER BY fires_at, id`)
	if err != nil {
		return fmt.Errorf("reminder: read saved reminders: %w", err)
	}
	var reminders []Reminder
	for rows.Next() {
		var (
			id         int64
			message    string
			kind       string
			durationMS int64
			firesMS    int64
		)
		if err := rows.Scan(&id, &message, &kind, &durationMS, &firesMS); err != nil {
			rows.Close()
			return fmt.Errorf("reminder: read saved reminder: %w", err)
		}
		reminders = append(reminders, Reminder{
			ID:              strconv.FormatInt(id, 10),
			Message:         message,
			Kind:            Kind(kind),
			DurationSeconds: int(time.Duration(durationMS) * time.Millisecond / time.Second),
			FiresAt:         time.UnixMilli(firesMS),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reminder: read saved reminders: %w", err)
	}
	rows.Close()

	now := m.now()
	var missed []Reminder
	for _, saved := range reminders {
		if !saved.FiresAt.After(now) {
			if overdue := now.Sub(saved.FiresAt); overdue > overdueGrace {
				if err := m.retireMissed(ctx, saved, now, "downtime"); err != nil {
					return err
				}
				missed = append(missed, saved)
				continue
			}
			// Within the grace window the fire transaction owns the delete, so a
			// crash before the callback cannot lose it without a record.
			m.schedule(saved, 0)
			continue
		}
		m.schedule(saved, saved.FiresAt.Sub(now))
	}
	if len(missed) > 0 {
		m.announceMissed(missed)
	}
	return nil
}

// schedule puts one Reminder in the live map and starts its callback.
func (m *Manager) schedule(saved Reminder, delay time.Duration) {
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
		reminder: saved,
		stop:     time.AfterFunc(delay, func() { m.runFire(id) }),
	}
	m.mu.Unlock()
}

// retireMissed deletes an overdue pending row and writes its missed history row
// in one transaction, so downtime is durable and never re-spoken.
func (m *Manager) retireMissed(ctx context.Context, r Reminder, endedAt time.Time, reason string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reminder: begin downtime retire: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE id = ?`, r.ID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("reminder: delete overdue reminder %s: %w", r.ID, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		tx.Rollback()
		return nil
	}
	event := historyEvent(r, alertstore.StateMissed)
	event.FiredAt = r.FiresAt
	event.EndedAt = endedAt
	event.EndedBy = reason
	if _, err := m.history.InsertTx(ctx, tx, event); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reminder: commit downtime retire: %w", err)
	}
	return nil
}

// announceMissed queues the one summary Announcement for Reminders that
// expired beyond the downtime grace. It is an ordinary Announcement, so Quiet
// hours holds and summarises it like any other.
func (m *Manager) announceMissed(missed []Reminder) {
	text := missedSummary(missed)
	if m.queue != nil && m.queue.Enqueue(text) {
		m.logger.Info("reminder downtime summary queued", "reminders", len(missed))
		return
	}
	// Do not log the summary: a Reminder message is content. The count is the
	// headless record that these Reminders expired.
	m.logger.Warn("reminder downtime summary dropped: speech is not configured",
		"reminders", len(missed))
}

// missedSummary renders one line naming what was missed. Duplicate messages are
// said once, which keeps a repeated reminder from reading as a stutter.
func missedSummary(missed []Reminder) string {
	seen := make(map[string]bool, len(missed))
	names := make([]string, 0, len(missed))
	for _, r := range missed {
		name := strings.TrimSpace(r.Message)
		if name == "" {
			name = "a reminder"
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	switch len(missed) {
	case 0:
		return ""
	case 1:
		return "You missed a reminder: " + names[0] + "."
	default:
		if len(names) == 0 {
			return fmt.Sprintf("You missed %d reminders.", len(missed))
		}
		return fmt.Sprintf("You missed %d reminders: %s.", len(missed), strings.Join(names, ", "))
	}
}

// historyEvent builds the alert-history row for one Reminder. The message is
// kept in label so the local Scheduled alerts page (#58) can show what was
// missed after the pending row is gone; it is never written to a log.
func historyEvent(r Reminder, state alertstore.State) alertstore.Event {
	return alertstore.Event{
		Kind:       "reminder",
		SourceID:   r.ID,
		Label:      r.Message,
		DurationMS: int64(r.DurationSeconds) * int64(time.Second/time.Millisecond),
		FiresAt:    r.FiresAt,
		FiredAt:    r.FiresAt,
		State:      state,
	}
}

// insert writes one new Reminder row and returns its database id.
func (m *Manager) insert(ctx context.Context, r Reminder, createdAt time.Time) (string, error) {
	result, err := m.db.ExecContext(ctx,
		`INSERT INTO reminders (message, kind, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.Message,
		string(r.Kind),
		int64(r.DurationSeconds)*int64(time.Second/time.Millisecond),
		r.FiresAt.UnixMilli(),
		createdAt.UnixMilli())
	if err != nil {
		return "", fmt.Errorf("reminder: save reminder: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("reminder: saved reminder id: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// deleteRows removes persisted Reminders in one transaction. A missing row is
// not an error: a fired and a cancelled path may both reach the same id.
func (m *Manager) deleteRows(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reminder: begin delete: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE id = ?`, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("reminder: delete reminder %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reminder: commit delete: %w", err)
	}
	return nil
}

// beginFire replaces the pending row with an open history row in one
// transaction. The delete is committed before the speaker starts, which is what
// makes a Reminder at-most-once across a restart.
func (m *Manager) beginFire(r Reminder) (int64, error) {
	m.mu.Lock()
	db := m.db
	m.mu.Unlock()
	if db == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reminder: begin fire: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE id = ?`, r.ID)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("reminder: delete fired reminder %s: %w", r.ID, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		tx.Rollback()
		return 0, errAlreadyGone
	}
	event := historyEvent(r, alertstore.StateFired)
	event.FiredAt = m.now()
	id, err := m.history.InsertTx(ctx, tx, event)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reminder: commit fire: %w", err)
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
		m.logger.Error("reminder history ringing", "event_id", eventID, "error", err.Error())
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
		m.logger.Error("reminder history finish", "event_id", eventID, "state", string(state), "error", err.Error())
	}
}
