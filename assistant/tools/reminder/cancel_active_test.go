package reminder_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// TestCancelQueryStopsASpeakingReminder covers the active map: the fired
// Reminder has no pending row, so only retained metadata can match "oven".
func TestCancelQueryStopsASpeakingReminder(t *testing.T) {
	speaker := &blockingSpeaker{started: make(chan struct{}, 1)}
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: speaker, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 10*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	select {
	case <-speaker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Reminder never started speaking")
	}
	if cancelled, err := manager.CancelQuery("oven"); err != nil || cancelled != 1 {
		t.Fatalf("CancelQuery(oven) = %d, %v; want the speaking Reminder", cancelled, err)
	}
	if cancelled, err := manager.CancelQuery("dentist"); err != nil || cancelled != 0 {
		t.Fatalf("CancelQuery(dentist) = %d, %v; want 0", cancelled, err)
	}
}

// TestCancelQueryEmptyStopsAQueuedReminder covers the Announcement queue case:
// "cancel every reminder" must remove an item that is waiting its turn.
func TestCancelQueryEmptyStopsAQueuedReminder(t *testing.T) {
	speaker := newTraceRecorder()
	var blocked atomic.Bool
	blocked.Store(true)
	queue := announce.New(speaker, testLogger(), announce.WithBlocked(func() bool { return blocked.Load() }))
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: speaker, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 10*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	// Wait until the fire has removed the pending row, then cancel the queued
	// item. Before that point CancelQuery would be cancelling the pending row,
	// which is not the case under test.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the Reminder never left the pending list")
		}
		if len(manager.List()) == 0 {
			if cancelled, err := manager.CancelQuery(""); err == nil && cancelled == 1 {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	blocked.Store(false)
	speaker.expectSilence(t, 100*time.Millisecond)
}

// TestCancelQueryRecordsAStoppedHistoryRow keeps the Scheduled alerts history
// honest: a user cancel is stopped/cancel, not a barge-in.
func TestCancelQueryRecordsAStoppedHistoryRow(t *testing.T) {
	st := openReminderStore(t, t.TempDir()+"/data/assistant.db")
	defer st.Close()
	speaker := &blockingSpeaker{started: make(chan struct{}, 1)}
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: speaker, Queue: queue, Logger: testLogger()})
	if err := manager.AttachStore(st); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 10*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	select {
	case <-speaker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Reminder never started speaking")
	}
	if cancelled, err := manager.CancelQuery("oven"); err != nil || cancelled != 1 {
		t.Fatalf("CancelQuery = %d, %v", cancelled, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := manager.History(1)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(events) == 1 && events[0].State == alertstore.StateStopped {
			if events[0].EndedBy != "cancel" {
				t.Fatalf("ended_by = %q, want cancel", events[0].EndedBy)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the cancelled Reminder never recorded a stopped/cancel history row")
}
