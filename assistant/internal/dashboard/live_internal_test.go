package dashboard

import "testing"

// TestLiveHubBoundsOneClient pins the bounded buffer: a reader that cannot keep
// up is disconnected so EventSource can reconnect and resync.
func TestLiveHubBoundsOneClient(t *testing.T) {
	hub := newLiveHub(nil)
	client := hub.subscribe()
	defer hub.unsubscribe(client)

	for i := 0; i < liveBuffer+1; i++ {
		hub.publish("activity", map[string]int{"n": i})
	}
	select {
	case <-client.lagged:
	default:
		t.Fatal("slow client was not dropped at the buffer bound")
	}
}

func TestLiveHubOnlySendsAChangedSample(t *testing.T) {
	hub := newLiveHub(nil)
	client := hub.subscribe()
	defer hub.unsubscribe(client)
	// The fake handler-less hub cannot sample; it is only used for fan-out.
	hub.diffAndPublish("volume", map[string]any{"Percent": 40})
	hub.diffAndPublish("volume", map[string]any{"Percent": 40})
	if got := len(client.events); got != 1 {
		t.Fatalf("events after an identical sample = %d, want 1", got)
	}
	hub.diffAndPublish("volume", map[string]any{"Percent": 50})
	if got := len(client.events); got != 2 {
		t.Fatalf("events after a change = %d, want 2", got)
	}
	// The health key drops the changed-at string, so a repeated check is silent
	// even though the payload carries a fresh timestamp.
	hub.diffAndPublishKey("health",
		map[string]any{"CheckedAt": "", "Status": "ok"},
		map[string]any{"CheckedAt": "12:00:00", "Status": "ok"})
	hub.diffAndPublishKey("health",
		map[string]any{"CheckedAt": "", "Status": "ok"},
		map[string]any{"CheckedAt": "12:00:30", "Status": "ok"})
	if got := len(client.events); got != 3 {
		t.Fatalf("events after an identical health check = %d, want 3", got)
	}
	hub.diffAndPublishKey("health",
		map[string]any{"CheckedAt": "", "Status": "error"},
		map[string]any{"CheckedAt": "12:01:00", "Status": "error"})
	if got := len(client.events); got != 4 {
		t.Fatalf("events after a health change = %d, want 4", got)
	}
}

func TestLiveHubStopsSamplingWithTheLastStream(t *testing.T) {
	hub := newLiveHub(nil)
	client := hub.subscribe()
	hub.mu.Lock()
	started := hub.cancel != nil
	hub.mu.Unlock()
	if !started {
		t.Fatal("the first stream did not start the samplers")
	}
	hub.unsubscribe(client)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.cancel != nil {
		t.Fatal("the samplers kept running after the last stream closed")
	}
}
