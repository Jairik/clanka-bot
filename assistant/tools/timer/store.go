package timer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// AttachStore makes the Manager durable. It loads the Timers a previous run
// left behind, applies the downtime rule, then schedules the future ones.
// Announcements receives the single summary for Timers that expired long
// before startup; it may be nil, in which case that summary is only logged.
//
// A Timer row is deleted before its alert is spoken. A crash during the alert
// can therefore lose it, but a restart can never fire it twice.
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
	m.announcements = announcements
	m.mu.Unlock()
	return m.recover()
}

// recover reads the persisted Timers and schedules them. It must run once,
// after AttachStore has installed the handle and before serving.
func (m *Manager) recover() error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

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
			// Remove the row first: whichever path fires next cannot repeat it.
			if err := m.deleteRows(ctx, []string{saved.id}); err != nil {
				return err
			}
			if overdue := now.Sub(saved.firesAt); overdue > overdueGrace {
				missed = append(missed, savedTimer)
				continue
			}
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
		stop:  time.AfterFunc(delay, func() { m.fire(id) }),
	}
	m.mu.Unlock()
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
