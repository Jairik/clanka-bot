package reminder_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// fakeReminderNotifier records the messages the Manager would push, without
// touching ntfy.
type fakeReminderNotifier struct {
	mu       sync.Mutex
	messages []string
	ch       chan string
}

func newFakeReminderNotifier() *fakeReminderNotifier {
	return &fakeReminderNotifier{ch: make(chan string, 8)}
}

func (f *fakeReminderNotifier) NotifyReminder(_ context.Context, message string) {
	f.mu.Lock()
	f.messages = append(f.messages, message)
	f.mu.Unlock()
	f.ch <- message
}

func (f *fakeReminderNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func waitReminderNotification(t *testing.T, notifier *fakeReminderNotifier) string {
	t.Helper()
	select {
	case message := <-notifier.ch:
		return message
	case <-time.After(3 * time.Second):
		t.Fatal("the Reminder never reached the Notifier")
		return ""
	}
}

// TestDueReminderNotifiesThePhoneOnce proves a fired Reminder reaches the phone
// alongside the spoken message, and only once.
func TestDueReminderNotifiesThePhoneOnce(t *testing.T) {
	speaker := newTraceRecorder()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)
	notifier := newFakeReminderNotifier()
	manager := reminder.NewManager(reminder.Options{
		Speaker: speaker, Queue: queue, Notify: notifier, Logger: testLogger(),
	})
	t.Cleanup(manager.Close)

	if _, err := manager.StartDuration("check the oven", 10*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if got := waitReminderNotification(t, notifier); got != "check the oven" {
		t.Fatalf("notification = %q", got)
	}
	if got := speaker.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q", got)
	}
	select {
	case extra := <-notifier.ch:
		t.Fatalf("one Reminder sent two notifications: %q", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestCancelledReminderDoesNotNotify keeps a cancel before the due time from
// producing a phone Notification.
func TestCancelledReminderDoesNotNotify(t *testing.T) {
	notifier := newFakeReminderNotifier()
	manager := reminder.NewManager(reminder.Options{Notify: notifier, Logger: testLogger()})
	t.Cleanup(manager.Close)

	item, err := manager.StartDuration("check the oven", time.Hour)
	if err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if cancelled, err := manager.Cancel(item.ID); err != nil || cancelled != 1 {
		t.Fatalf("Cancel = %d, %v", cancelled, err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := notifier.count(); got != 0 {
		t.Fatalf("cancelled Reminder sent %d notifications, want 0", got)
	}
}
