package alarm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// storedAlarm is one row as read from SQLite. nextFiresAt and snoozedUntil are
// kept separately from Alarm.NextFiresAt because recovery needs to know which
// one is pending.
type storedAlarm struct {
	alarm        Alarm
	days         uint8
	nextFiresAt  time.Time
	snoozedUntil time.Time
	createdAt    time.Time
}

// spec is the row's repeat rule.
func (s storedAlarm) spec() scheduleSpec {
	return scheduleSpec{schedule: s.alarm.Schedule, days: s.days}
}

// AttachStore makes the Manager durable. It first resolves any Alarm history a
// previous process left open as missed, then loads the Alarms a previous run
// left behind, applies the downtime rule and schedules the future ones. It
// must run after the live settings, Cues, Zone and Ring seams are bound (the
// module's Start hook), so the first recovered Ring uses effective settings.
func (m *Manager) AttachStore(st *store.Store, announcements *announce.Queue) error {
	if st == nil || st.DB() == nil {
		return nil // in-memory Manager; existing callers and tests stay valid
	}
	m.mu.Lock()
	if m.db != nil {
		m.mu.Unlock()
		return errNotStored
	}
	m.db = st.DB()
	m.history = alertstore.New(st.DB())
	m.announcements = announcements
	m.mu.Unlock()
	return m.recover()
}

// recover reads the persisted Alarms and schedules them.
func (m *Manager) recover() error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	now := m.now()

	// A previous process may have crashed while an Alarm was fired or ringing.
	// Only Alarm rows are closed here; the Timer manager owns its own kind.
	open, err := m.history.RecoverKind(ctx, "alarm", now)
	if err != nil {
		return err
	}
	for _, event := range open {
		m.logger.Warn("alarm ring interrupted by restart; recorded missed",
			"event_id", event.ID, "label", event.Label, "fires_at", event.FiresAt)
		m.notifyMissedEvent(event, "restart")
	}

	stored, err := m.readAlarms(ctx)
	if err != nil {
		return err
	}
	var missed []Alarm
	for _, saved := range stored {
		alarm := saved.alarm
		if !saved.snoozedUntil.IsZero() {
			target := saved.snoozedUntil
			if target.After(now) {
				m.schedule(alarm, target, true)
				continue
			}
			if now.Sub(target) <= overdueGrace {
				m.schedule(alarm, target, true)
				continue
			}
			// The snooze expired during downtime: record one missed occurrence,
			// then continue with the Alarm's own next occurrence.
			if err := m.retireSnoozeMissed(ctx, saved, now); err != nil {
				return err
			}
			missed = append(missed, alarm)
			if alarm.Schedule == Once {
				continue
			}
			saved.snoozedUntil = time.Time{}
		}

		if alarm.Schedule == Once {
			if saved.nextFiresAt.UnixMilli() <= 0 {
				// It fired but the previous process ended before the outcome;
				// the open history row was just recovered as missed. Remove the
				// finished one-off row so it can never fire again.
				m.deleteDoneRow(alarm.ID)
				continue
			}
			if !saved.nextFiresAt.After(now) {
				if now.Sub(saved.nextFiresAt) <= overdueGrace {
					m.schedule(alarm, saved.nextFiresAt, false)
					continue
				}
				if err := m.retireMissed(ctx, saved, saved.nextFiresAt, now); err != nil {
					return err
				}
				missed = append(missed, alarm)
				continue
			}
			m.schedule(alarm, saved.nextFiresAt, false)
			continue
		}

		// A repeating Alarm always has a next occurrence. A zero row is a
		// defensive repair, not a normal state.
		if saved.nextFiresAt.UnixMilli() <= 0 {
			next := nextOccurrence(saved.spec(), alarm.Hour, alarm.Minute, m.location(), now)
			if err := m.setNext(ctx, alarm.ID, next); err != nil {
				return err
			}
			alarm.NextFiresAt = next
			m.schedule(alarm, next, false)
			continue
		}
		if !saved.nextFiresAt.After(now) {
			if now.Sub(saved.nextFiresAt) <= overdueGrace {
				m.schedule(alarm, saved.nextFiresAt, false)
				continue
			}
			next, err := m.retireRepeatMissed(ctx, saved, now)
			if err != nil {
				return err
			}
			missed = append(missed, alarm)
			alarm.NextFiresAt = next
			m.schedule(alarm, next, false)
			continue
		}
		m.schedule(alarm, saved.nextFiresAt, false)
	}

	if len(missed) > 0 {
		m.announceMissed(missed)
	}
	return nil
}

// retireSnoozeMissed clears an expired snooze and records one missed history
// row. A one-off row is deleted; a repeating row keeps its own next occurrence.
func (m *Manager) retireSnoozeMissed(ctx context.Context, saved storedAlarm, endedAt time.Time) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("alarm: begin snooze retire: %w", err)
	}
	if saved.alarm.Schedule == Once {
		if _, err := tx.ExecContext(ctx, `DELETE FROM alarms WHERE id = ?`, saved.alarm.ID); err != nil {
			tx.Rollback()
			return fmt.Errorf("alarm: delete overdue alarm %s: %w", saved.alarm.ID, err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE alarms SET snoozed_until = NULL WHERE id = ?`, saved.alarm.ID); err != nil {
		tx.Rollback()
		return fmt.Errorf("alarm: clear overdue snooze %s: %w", saved.alarm.ID, err)
	}
	event := alertstore.Event{
		Kind:     "alarm",
		SourceID: saved.alarm.ID,
		Label:    saved.alarm.Label,
		FiresAt:  saved.snoozedUntil,
		FiredAt:  saved.snoozedUntil,
		State:    alertstore.StateMissed,
		EndedAt:  endedAt,
		EndedBy:  "downtime",
	}
	if _, err := m.history.InsertTx(ctx, tx, event); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("alarm: commit snooze retire: %w", err)
	}
	m.notifyMissedEvent(event, "downtime")
	return nil
}

// retireMissed removes a one-off occurrence or advances a repeating one, and
// records the missed history row in the same transaction.
func (m *Manager) retireMissed(ctx context.Context, saved storedAlarm, occurrence, endedAt time.Time) error {
	_, err := m.retireOccurrence(ctx, saved, occurrence, endedAt)
	return err
}

// retireRepeatMissed advances a repeating Alarm and returns its next
// occurrence after the downtime gap.
func (m *Manager) retireRepeatMissed(ctx context.Context, saved storedAlarm, now time.Time) (time.Time, error) {
	return m.retireOccurrence(ctx, saved, saved.nextFiresAt, now)
}

// retireOccurrence writes one missed occurrence and the row's new state.
func (m *Manager) retireOccurrence(ctx context.Context, saved storedAlarm, occurrence, endedAt time.Time) (time.Time, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("alarm: begin downtime retire: %w", err)
	}
	var next time.Time
	if saved.alarm.Schedule == Once {
		if _, err := tx.ExecContext(ctx, `DELETE FROM alarms WHERE id = ?`, saved.alarm.ID); err != nil {
			tx.Rollback()
			return time.Time{}, fmt.Errorf("alarm: delete overdue alarm %s: %w", saved.alarm.ID, err)
		}
	} else {
		next = nextOccurrence(saved.spec(), saved.alarm.Hour, saved.alarm.Minute, m.location(), endedAt)
		if _, err := tx.ExecContext(ctx,
			`UPDATE alarms SET next_fires_at = ?, snoozed_until = NULL WHERE id = ?`,
			next.UnixMilli(), saved.alarm.ID); err != nil {
			tx.Rollback()
			return time.Time{}, fmt.Errorf("alarm: advance overdue alarm %s: %w", saved.alarm.ID, err)
		}
	}
	event := alertstore.Event{
		Kind:     "alarm",
		SourceID: saved.alarm.ID,
		Label:    saved.alarm.Label,
		FiresAt:  occurrence,
		FiredAt:  occurrence,
		State:    alertstore.StateMissed,
		EndedAt:  endedAt,
		EndedBy:  "downtime",
	}
	if _, err := m.history.InsertTx(ctx, tx, event); err != nil {
		tx.Rollback()
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("alarm: commit downtime retire: %w", err)
	}
	m.notifyMissedEvent(event, "downtime")
	return next, nil
}

// announceMissed queues the one summary Announcement for Alarms that expired
// beyond the downtime grace.
func (m *Manager) announceMissed(missed []Alarm) {
	text := missedSummary(missed)
	if m.announcements != nil && m.announcements.Enqueue(text) {
		m.logger.Info("alarm downtime summary queued", "alarms", len(missed))
		return
	}
	m.logger.Warn("alarm downtime summary dropped: speech is not configured",
		"alarms", len(missed), "summary", text)
}

// missedSummary renders one line naming the missed Alarms.
func missedSummary(missed []Alarm) string {
	names := make([]string, 0, len(missed))
	for _, alarm := range missed {
		name := formatClock(alarm.Hour, alarm.Minute)
		if alarm.Label != "" {
			name = alarm.Label + " at " + name
		}
		names = append(names, name)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "You missed an alarm: " + names[0] + "."
	default:
		return fmt.Sprintf("You missed %d alarms: %s.", len(names), strings.Join(names, ", "))
	}
}

// readAlarms reads every row, soonest first.
func (m *Manager) readAlarms(ctx context.Context) ([]storedAlarm, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, label, hour, minute, schedule, weekdays, next_fires_at,
		        created_at, snoozed_until, sound, volume, ramp_ms
		 FROM alarms ORDER BY next_fires_at, id`)
	if err != nil {
		return nil, fmt.Errorf("alarm: read saved alarms: %w", err)
	}
	defer rows.Close()
	var stored []storedAlarm
	for rows.Next() {
		var (
			id        int64
			label     string
			hour      int
			minute    int
			schedule  string
			days      int64
			nextMS    int64
			createdMS int64
			snoozeMS  sql.NullInt64
			sound     string
			volume    sql.NullInt64
			rampMS    sql.NullInt64
		)
		if err := rows.Scan(&id, &label, &hour, &minute, &schedule, &days,
			&nextMS, &createdMS, &snoozeMS, &sound, &volume, &rampMS); err != nil {
			return nil, fmt.Errorf("alarm: read saved alarm: %w", err)
		}
		saved := storedAlarm{
			alarm: Alarm{
				ID:       fmt.Sprintf("%d", id),
				Label:    label,
				Hour:     hour,
				Minute:   minute,
				Schedule: storedSchedule(schedule),
				Sound:    sound,
			},
			days:        uint8(days),
			nextFiresAt: time.UnixMilli(nextMS),
			createdAt:   time.UnixMilli(createdMS),
		}
		if saved.alarm.Schedule == Chosen {
			saved.alarm.Weekdays = weekdayNames(uint8(days))
		}
		if snoozeMS.Valid {
			saved.snoozedUntil = time.UnixMilli(snoozeMS.Int64)
		}
		if volume.Valid {
			value := int(volume.Int64)
			saved.alarm.Volume = &value
		}
		if rampMS.Valid {
			value := int(time.Duration(rampMS.Int64) * time.Millisecond / time.Second)
			saved.alarm.RampSeconds = &value
		}
		if !saved.snoozedUntil.IsZero() {
			saved.alarm.NextFiresAt = saved.snoozedUntil
		} else if saved.nextFiresAt.UnixMilli() > 0 {
			saved.alarm.NextFiresAt = saved.nextFiresAt
		}
		stored = append(stored, saved)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alarm: read saved alarms: %w", err)
	}
	return stored, nil
}

// beginFire reads the authoritative row, advances or removes the pending
// occurrence and opens the history row in one transaction. A missing row means
// a concurrent cancel won; the fire then stays silent. For an in-memory
// Manager the caller builds the state from its live entry.
func (m *Manager) beginFire(id string, snoozed bool) (fireState, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fireState{}, false, fmt.Errorf("alarm: begin fire: %w", err)
	}
	defer tx.Rollback()

	var (
		label    string
		hour     int
		minute   int
		schedule string
		days     int64
		nextMS   int64
		snoozeMS sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT label, hour, minute, schedule, weekdays, next_fires_at, snoozed_until
		 FROM alarms WHERE id = ?`, id).
		Scan(&label, &hour, &minute, &schedule, &days, &nextMS, &snoozeMS)
	if errors.Is(err, sql.ErrNoRows) {
		return fireState{}, false, nil
	}
	if err != nil {
		return fireState{}, false, fmt.Errorf("alarm: read firing alarm %s: %w", id, err)
	}

	occurrence := time.UnixMilli(nextMS)
	var next *time.Time
	if snoozed {
		if !snoozeMS.Valid {
			return fireState{}, false, nil // the snooze was cleared
		}
		occurrence = time.UnixMilli(snoozeMS.Int64)
		if _, err := tx.ExecContext(ctx,
			`UPDATE alarms SET snoozed_until = NULL WHERE id = ?`, id); err != nil {
			return fireState{}, false, fmt.Errorf("alarm: clear snooze %s: %w", id, err)
		}
		if nextMS > 0 {
			value := time.UnixMilli(nextMS)
			next = &value
		}
	} else if storedSchedule(schedule) == Once {
		if nextMS <= 0 {
			return fireState{}, false, nil // already fired
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE alarms SET next_fires_at = 0, snoozed_until = NULL WHERE id = ?`, id); err != nil {
			return fireState{}, false, fmt.Errorf("alarm: mark one-off fired %s: %w", id, err)
		}
	} else {
		if nextMS <= 0 {
			return fireState{}, false, nil // already fired
		}
		value := nextOccurrence(scheduleSpec{schedule: storedSchedule(schedule), days: uint8(days)},
			hour, minute, m.location(), occurrence)
		if _, err := tx.ExecContext(ctx,
			`UPDATE alarms SET next_fires_at = ?, snoozed_until = NULL WHERE id = ?`,
			value.UnixMilli(), id); err != nil {
			return fireState{}, false, fmt.Errorf("alarm: advance fired alarm %s: %w", id, err)
		}
		next = &value
	}

	alarm := Alarm{
		ID:          id,
		Label:       label,
		Hour:        hour,
		Minute:      minute,
		Schedule:    storedSchedule(schedule),
		NextFiresAt: occurrence,
	}
	if alarm.Schedule == Chosen {
		alarm.Weekdays = weekdayNames(uint8(days))
	}
	event := alertstore.Event{
		Kind:     "alarm",
		SourceID: id,
		Label:    label,
		FiresAt:  occurrence,
		FiredAt:  m.now(),
		State:    alertstore.StateFired,
	}
	eventID, err := m.history.InsertTx(ctx, tx, event)
	if err != nil {
		return fireState{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return fireState{}, false, fmt.Errorf("alarm: commit fire: %w", err)
	}
	return fireState{eventID: eventID, alarm: alarm, occurrence: occurrence, next: next}, true, nil
}

// setNext repairs a repeating row that lost its next occurrence.
func (m *Manager) setNext(ctx context.Context, id string, next time.Time) error {
	if _, err := m.db.ExecContext(ctx,
		`UPDATE alarms SET next_fires_at = ? WHERE id = ?`, next.UnixMilli(), id); err != nil {
		return fmt.Errorf("alarm: repair next occurrence %s: %w", id, err)
	}
	return nil
}

// storedSchedule normalises a stored schedule value; an unknown value falls
// back to a one-off so a corrupt row cannot stop startup.
func storedSchedule(text string) Schedule {
	switch Schedule(text) {
	case Daily, Weekdays, Weekends, Chosen:
		return Schedule(text)
	default:
		return Once
	}
}
