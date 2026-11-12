// Package alertstore keeps the durable history of a scheduled alert that has
// fired. The pending schedule (a Timer row, later an Alarm row) is deleted at
// fire time so a restart can never fire it twice; this table is the record of
// what happened afterwards: fired, ringing, and then stopped, missed or
// interrupted. A crash leaves a fired/ringing row behind, and startup resolves
// it as missed instead of silently losing the fact that the alert went off.
package alertstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// State is one step of an alert's lifecycle. Fired and Ringing are open; the
// other three are terminal.
type State string

const (
	// StateFired means the scheduler fired the alert and it is waiting for the
	// one speaker (for example behind another Ring).
	StateFired State = "fired"
	// StateRinging means the alert currently owns the speaker and is ringing.
	StateRinging State = "ringing"
	// StateMissed means the Ring ran to max_ring (or was found still ringing
	// after a crash); nobody stopped or snoozed it.
	StateMissed State = "missed"
	// StateStopped means stop, cancel or snooze ended the Ring.
	StateStopped State = "stopped"
	// StateInterrupted means a clean shutdown ended the Ring before its answer
	// mattered.
	StateInterrupted State = "interrupted"
)

// Open reports whether an event reached the speaker but has no outcome yet.
func (s State) Open() bool { return s == StateFired || s == StateRinging }

// Event is one alert history row. Kind is "timer" today and "alarm" from #53;
// SourceID is the id the scheduler used before it deleted its pending row.
type Event struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	SourceID   string    `json:"source_id"`
	Label      string    `json:"label,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	FiresAt    time.Time `json:"fires_at"`
	FiredAt    time.Time `json:"fired_at"`
	State      State     `json:"state"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	EndedBy    string    `json:"ended_by,omitempty"`
}

// Store reads and writes the alert history table. It is safe for concurrent
// use through the single SQLite connection the store package owns.
type Store struct {
	db *sql.DB
}

// New wraps the assistant's one database handle. A nil handle makes every
// method a no-op, so an in-memory Manager keeps working.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Open reports whether the history is backed by a database.
func (s *Store) Open() bool { return s != nil && s.db != nil }

// InsertTx writes one event inside the caller's transaction. The scheduler
// uses it together with the pending-row delete, so a crash can neither fire an
// alert twice nor leave no history of a fire.
func (s *Store) InsertTx(ctx context.Context, tx *sql.Tx, event Event) (int64, error) {
	if s == nil || tx == nil {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO alert_events
			(kind, source_id, label, duration_ms, fires_at, fired_at, state, ended_at, ended_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Kind,
		event.SourceID,
		event.Label,
		event.DurationMS,
		event.FiresAt.UnixMilli(),
		event.FiredAt.UnixMilli(),
		string(event.State),
		nullMillis(event.EndedAt),
		event.EndedBy)
	if err != nil {
		return 0, fmt.Errorf("alertstore: insert event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("alertstore: event id: %w", err)
	}
	return id, nil
}

// MarkRinging moves a fired event to ringing when it takes the speaker.
func (s *Store) MarkRinging(ctx context.Context, id int64, at time.Time) error {
	if s == nil || s.db == nil || id <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_events SET state = ? WHERE id = ? AND state = ?`,
		string(StateRinging), id, string(StateFired)); err != nil {
		return fmt.Errorf("alertstore: mark ringing: %w", err)
	}
	return nil
}

// Finish closes an open event with its terminal state. Calling it twice is a
// no-op after the first terminal write.
func (s *Store) Finish(ctx context.Context, id int64, state State, endedBy string, at time.Time) error {
	if s == nil || s.db == nil || id <= 0 {
		return nil
	}
	if !terminal(state) {
		return fmt.Errorf("alertstore: %q is not a terminal state", state)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_events SET state = ?, ended_at = ?, ended_by = ?
		 WHERE id = ? AND state IN (?, ?)`,
		string(state), at.UnixMilli(), endedBy, id, string(StateFired), string(StateRinging)); err != nil {
		return fmt.Errorf("alertstore: finish event: %w", err)
	}
	return nil
}

// Recover resolves every event a previous process left open. A crash between
// firing and an answer is recorded as missed with ended_by "restart", so the
// history is honest and a later notification hook can act on it. It returns
// the events it closed.
func (s *Store) Recover(ctx context.Context, at time.Time) ([]Event, error) {
	return s.recover(ctx, "", at)
}

// RecoverKind resolves only one kind's open events. The scheduler modules each
// recover their own table, so a Timer startup cannot close a Reminder that is
// waiting for the speaker and report it as a missed Timer.
func (s *Store) RecoverKind(ctx context.Context, kind string, at time.Time) ([]Event, error) {
	return s.recover(ctx, kind, at)
}

// recover closes the open events, optionally filtered to one kind.
func (s *Store) recover(ctx context.Context, kind string, at time.Time) ([]Event, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("alertstore: begin recovery: %w", err)
	}
	query := `SELECT id, kind, source_id, label, duration_ms, fires_at, fired_at, state, ended_by
		 FROM alert_events WHERE state IN (?, ?)`
	args := []any{string(StateFired), string(StateRinging)}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("alertstore: read open events: %w", err)
	}
	var open []Event
	for rows.Next() {
		var (
			event     Event
			duration  int64
			firesAt   int64
			firedAt   int64
			stateText string
		)
		if err := rows.Scan(&event.ID, &event.Kind, &event.SourceID, &event.Label,
			&duration, &firesAt, &firedAt, &stateText, &event.EndedBy); err != nil {
			rows.Close()
			tx.Rollback()
			return nil, fmt.Errorf("alertstore: read open event: %w", err)
		}
		event.DurationMS = duration
		event.FiresAt = time.UnixMilli(firesAt)
		event.FiredAt = time.UnixMilli(firedAt)
		event.State = State(stateText)
		open = append(open, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		tx.Rollback()
		return nil, fmt.Errorf("alertstore: read open events: %w", err)
	}
	rows.Close()

	for i := range open {
		if _, err := tx.ExecContext(ctx,
			`UPDATE alert_events SET state = ?, ended_at = ?, ended_by = ?
			 WHERE id = ? AND state IN (?, ?)`,
			string(StateMissed), at.UnixMilli(), "restart", open[i].ID,
			string(StateFired), string(StateRinging)); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("alertstore: recover event %d: %w", open[i].ID, err)
		}
		open[i].State = StateMissed
		open[i].EndedAt = at
		open[i].EndedBy = "restart"
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("alertstore: commit recovery: %w", err)
	}
	return open, nil
}

// List returns the newest events first, capped at limit. A limit of zero or
// less returns 50.
func (s *Store) List(ctx context.Context, limit int) ([]Event, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, source_id, label, duration_ms, fires_at, fired_at,
		        state, ended_at, ended_by
		 FROM alert_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("alertstore: list events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var (
			event     Event
			duration  int64
			firesAt   int64
			firedAt   int64
			endedAt   sql.NullInt64
			stateText string
		)
		if err := rows.Scan(&event.ID, &event.Kind, &event.SourceID, &event.Label,
			&duration, &firesAt, &firedAt, &stateText, &endedAt, &event.EndedBy); err != nil {
			return nil, fmt.Errorf("alertstore: list event: %w", err)
		}
		event.DurationMS = duration
		event.FiresAt = time.UnixMilli(firesAt)
		event.FiredAt = time.UnixMilli(firedAt)
		event.State = State(stateText)
		if endedAt.Valid {
			event.EndedAt = time.UnixMilli(endedAt.Int64)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alertstore: list events: %w", err)
	}
	return events, nil
}

// terminal reports whether state closes an event.
func terminal(state State) bool {
	switch state {
	case StateMissed, StateStopped, StateInterrupted:
		return true
	default:
		return false
	}
}

// nullMillis stores an absent time as SQL NULL rather than the Unix epoch.
func nullMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
