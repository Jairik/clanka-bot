package reminder_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// TestCancelFireRace lets a cancel and the due callback race on the same
// Reminder. Exactly one path owns it: a cancelled Reminder never speaks, and a
// fired one speaks at most once and leaves no pending row.
func TestCancelFireRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st := openReminderStore(t, path)
	defer st.Close()
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	defer queue.Close()
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	if err := manager.AttachStore(st); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	defer manager.Close()

	const total = 20
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		r, err := manager.StartDuration(fmt.Sprintf("item %d", i), 50*time.Millisecond)
		if err != nil {
			t.Fatalf("StartDuration %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}

	var (
		wg        sync.WaitGroup
		cancelled int
		mu        sync.Mutex
	)
	for i, id := range ids {
		if i%2 == 1 {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if n, err := manager.Cancel(id); err == nil && n == 1 {
				mu.Lock()
				cancelled++
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	time.Sleep(250 * time.Millisecond)

	if got := countReminderRows(t, st); got != 0 {
		t.Fatalf("pending rows after the race = %d, want 0", got)
	}
	if cancelled != total/2 {
		t.Fatalf("cancelled %d reminders, want %d", cancelled, total/2)
	}
	spoken := rec.snapshot()
	if len(spoken) != total/2 {
		t.Fatalf("spoken %d reminders, want %d", len(spoken), total/2)
	}
	seen := make(map[string]bool, len(spoken))
	for _, event := range spoken {
		if seen[event] {
			t.Fatalf("reminder repeated: %q", event)
		}
		seen[event] = true
	}
}

// TestCancelAndDueCallbackRaceOnTheSameInstant exercises the true race: a
// due callback and a cancel arrive together. Either way the database ends
// empty and no message is spoken twice.
func TestCancelAndDueCallbackRaceOnTheSameInstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st := openReminderStore(t, path)
	defer st.Close()
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	defer queue.Close()
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	if err := manager.AttachStore(st); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	defer manager.Close()

	for i := 0; i < 30; i++ {
		r, err := manager.StartDuration(fmt.Sprintf("race %d", i), time.Millisecond)
		if err != nil {
			t.Fatalf("StartDuration %d: %v", i, err)
		}
		go manager.Cancel(r.ID)
	}
	time.Sleep(300 * time.Millisecond)
	if got := countReminderRows(t, st); got != 0 {
		t.Fatalf("pending rows after the simultaneous race = %d, want 0", got)
	}
	seen := make(map[string]bool)
	for _, event := range rec.snapshot() {
		if seen[event] {
			t.Fatalf("reminder repeated: %q", event)
		}
		seen[event] = true
	}
}

// TestConcurrentListAndSetAreRaceFree exercises the Manager lock while readers
// and writers overlap, which is what the Dashboard (#58) will do.
func TestConcurrentListAndSetAreRaceFree(t *testing.T) {
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	defer queue.Close()
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	defer manager.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = manager.StartDuration(fmt.Sprintf("item %d", i), time.Hour)
			_ = manager.List()
		}(i)
	}
	wg.Wait()
	if got := len(manager.List()); got != 8 {
		t.Fatalf("list after concurrent sets = %d, want 8", got)
	}
	if n, err := manager.Cancel(""); err != nil || n != 8 {
		t.Fatalf("Cancel all = %d, %v; want 8, nil", n, err)
	}
}

// TestFireWithoutStoreStillRecordsThroughTheQueue is the in-memory seam: no
// database means no history, but the alert still plays once.
func TestFireWithoutStoreStillRecordsThroughTheQueue(t *testing.T) {
	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	defer queue.Close()
	manager := reminder.NewManager(reminder.Options{Speaker: rec, Queue: queue, Logger: testLogger()})
	defer manager.Close()

	if _, err := manager.StartDuration("check the oven", 10*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q", got)
	}
	events, err := manager.History(10)
	if err != nil || len(events) != 0 {
		t.Fatalf("in-memory history = %+v, %v; want empty", events, err)
	}
}
