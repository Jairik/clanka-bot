package announce_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// confirmationClock is a manually advanced clock for the Confirmation TTL.
type confirmationClock struct {
	mu sync.Mutex
	at time.Time
}

func newConfirmationClock() *confirmationClock {
	return &confirmationClock{at: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *confirmationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *confirmationClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type queueSendIn struct {
	To string `json:"to"`
}

type queueSendOut struct {
	Status string `json:"status"`
}

// queueSendTool is a test-only confirm Tool; it counts runs so the test can
// prove the expired question never fires it.
func queueSendTool(runs *int) tool.Tool[queueSendIn, queueSendOut] {
	return tool.Tool[queueSendIn, queueSendOut]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, queueSendIn) (queueSendOut, error) {
			*runs++
			return queueSendOut{Status: "sent"}, nil
		},
	}
}

// TestQueuedAnnouncementPlaysAfterAConfirmationExpires covers issue #174: an
// unanswered Confirmation must stop blocking the Announcement queue on its own,
// without a further request or another turn.
func TestQueuedAnnouncementPlaysAfterAConfirmationExpires(t *testing.T) {
	clock := newConfirmationClock()
	registry := tool.NewRegistry(time.Second, tool.WithClock(clock.Now))
	runs := 0
	if err := tool.Add(registry, queueSendTool(&runs)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := registry.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`)); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger(), announce.WithBlocked(func() bool {
		_, pending := registry.Pending()
		return pending
	}))
	t.Cleanup(queue.Close)

	queue.Enqueue("Your reminder: check the oven.")
	expectSilence(t, speaker)

	clock.Advance(tool.PendingTTL)
	// The blocked predicate only rechecks on the queue's timer; no request,
	// turn or explicit Release happens here.
	expectPhrasesWithTimeout(t, speaker, 2*time.Second, "Your reminder: check the oven.")
	if runs != 0 {
		t.Fatalf("the expired Confirmation ran %d times", runs)
	}
}
