package pipeline

import (
	"testing"
	"time"
)

// Expiry applies before an event, before dispatch and before listing.
func TestDeferredExpiry(t *testing.T) {
	q := &DeferredQueue{actions: []DeferredAction{{ID: 1, Event: "timer", Expires: time.Now().Add(-time.Second)}}}
	q.Trigger("timer")
	if _, ok := q.NextReady(); ok {
		t.Fatal("expired action dispatched")
	}
	if len(q.List()) != 0 {
		t.Fatal("expired action visible")
	}
}
