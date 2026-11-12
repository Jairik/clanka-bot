package alertstore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openHistory opens the real database with its real migrations.
func openHistory(t *testing.T) (*alertstore.Store, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return alertstore.New(st.DB()), st
}

// TestEventLifecycleIsDurableAcrossReopen proves the fired -> ringing ->
// stopped/missed row survives a real database close and reopen.
func TestEventLifecycleIsDurableAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	history := alertstore.New(st.DB())
	firedAt := time.UnixMilli(1_700_000_000_000)
	id := insertEvent(t, st.DB(), history, alertstore.Event{
		Kind:       "timer",
		SourceID:   "7",
		Label:      "pasta",
		DurationMS: 600_000,
		FiresAt:    firedAt,
		FiredAt:    firedAt,
		State:      alertstore.StateFired,
	})
	if err := history.MarkRinging(ctx, id, firedAt.Add(time.Second)); err != nil {
		t.Fatalf("MarkRinging: %v", err)
	}
	if err := history.Finish(ctx, id, alertstore.StateMissed, "timeout", firedAt.Add(5*time.Minute)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	events, err := alertstore.New(reopened.DB()).List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	event := events[0]
	if event.ID != id || event.Label != "pasta" || event.State != alertstore.StateMissed {
		t.Fatalf("event = %+v, want the missed pasta row", event)
	}
	if event.EndedBy != "timeout" || !event.EndedAt.Equal(firedAt.Add(5*time.Minute)) {
		t.Fatalf("ended = %q at %s, want timeout at +5m", event.EndedBy, event.EndedAt)
	}
}

// TestRecoverClosesOnlyOpenEvents models a crash: fired/ringing rows become
// missed on the next start, while a terminal row is left untouched.
func TestRecoverClosesOnlyOpenEvents(t *testing.T) {
	ctx := context.Background()
	history, st := openHistory(t)
	at := time.UnixMilli(1_700_000_100_000)
	fired := insertEvent(t, st.DB(), history, alertstore.Event{
		Kind: "timer", SourceID: "fired", Label: "fired", FiresAt: at, FiredAt: at, State: alertstore.StateFired,
	})
	ringing := insertEvent(t, st.DB(), history, alertstore.Event{
		Kind: "timer", SourceID: "ringing", Label: "ringing", FiresAt: at, FiredAt: at, State: alertstore.StateRinging,
	})
	stopped := insertEvent(t, st.DB(), history, alertstore.Event{
		Kind: "timer", SourceID: "stopped", Label: "stopped", FiresAt: at, FiredAt: at, State: alertstore.StateRinging,
	})
	if err := history.Finish(ctx, stopped, alertstore.StateStopped, "stop", at); err != nil {
		t.Fatalf("finish stopped: %v", err)
	}

	recovered, err := history.Recover(ctx, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered = %+v, want the two open rows", recovered)
	}
	for _, event := range recovered {
		if event.State != alertstore.StateMissed || event.EndedBy != "restart" {
			t.Fatalf("recovered event = %+v, want missed/restart", event)
		}
	}
	events, err := history.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	states := map[int64]alertstore.State{}
	for _, event := range events {
		states[event.ID] = event.State
	}
	if states[fired] != alertstore.StateMissed || states[ringing] != alertstore.StateMissed {
		t.Fatalf("states = %+v, want both open rows missed", states)
	}
	if states[stopped] != alertstore.StateStopped {
		t.Fatalf("terminal row changed to %q", states[stopped])
	}
}

// TestRecoverKindLeavesAnotherSchedulerAlone proves the per-kind recovery seam:
// a Timer startup must not close a Reminder that is waiting for the speaker.
func TestRecoverKindLeavesAnotherSchedulerAlone(t *testing.T) {
	ctx := context.Background()
	history, st := openHistory(t)
	at := time.UnixMilli(1_700_000_100_000)
	timer := insertEvent(t, st.DB(), history, alertstore.Event{
		Kind: "timer", SourceID: "timer", FiresAt: at, FiredAt: at, State: alertstore.StateFired,
	})
	reminder := insertEvent(t, st.DB(), history, alertstore.Event{
		Kind: "reminder", SourceID: "reminder", FiresAt: at, FiredAt: at, State: alertstore.StateFired,
	})

	recovered, err := history.RecoverKind(ctx, "timer", at.Add(time.Minute))
	if err != nil {
		t.Fatalf("RecoverKind: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Kind != "timer" || recovered[0].ID != timer {
		t.Fatalf("recovered = %+v, want only the timer row", recovered)
	}
	state := eventState(t, history, reminder)
	if state != alertstore.StateFired {
		t.Fatalf("reminder state = %q, want it still open", state)
	}
	if _, err := history.RecoverKind(ctx, "reminder", at.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecoverKind reminder: %v", err)
	}
	state = eventState(t, history, reminder)
	if state != alertstore.StateMissed {
		t.Fatalf("reminder after its own recovery = %q, want missed", state)
	}
}

// eventState reads one row's state through List.
func eventState(t *testing.T, history *alertstore.Store, id int64) alertstore.State {
	t.Helper()
	events, err := history.List(context.Background(), 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, event := range events {
		if event.ID == id {
			return event.State
		}
	}
	t.Fatalf("event %d not found", id)
	return ""
}

// insertEvent writes one event in its own transaction, as the scheduler does.
func insertEvent(t *testing.T, db *sql.DB, history *alertstore.Store, event alertstore.Event) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := history.InsertTx(ctx, tx, event)
	if err != nil {
		tx.Rollback()
		t.Fatalf("InsertTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}
