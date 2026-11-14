package dashboard

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

// readFrame skips blank lines and comments and returns the next named event.
func readFrame(t *testing.T, reader *bufio.Reader, deadline time.Duration) (name, data string) {
	t.Helper()
	type frame struct{ name, data string }
	frames := make(chan frame, 1)
	go func() {
		var current frame
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if current.name != "" {
					frames <- current
					return
				}
			case strings.HasPrefix(line, "event: "):
				current.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case got := <-frames:
		return got.name, got.data
	case <-time.After(deadline):
		t.Fatalf("no frame within %v", deadline)
		return "", ""
	}
}

// TestLiveStreamPingsByName pins the idle frame the page's staleness watchdog
// listens for (#149). A comment would keep a proxy alive but fires no
// EventSource event, so the frame has to be a named event carrying data.
func TestLiveStreamPingsByName(t *testing.T) {
	handler, err := New(Options{
		Owner:  "owner@example.com",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Production pings every 15 s; only the frame is under test.
	handler.live.keepalive = 10 * time.Millisecond
	server := httptest.NewServer(handler)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/ui/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(identityHeader, "owner@example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", res.StatusCode)
	}

	name, data := readFrame(t, bufio.NewReader(res.Body), 5*time.Second)
	if name != "ping" {
		t.Fatalf("idle event = %q, want ping", name)
	}
	if data != "{}" {
		t.Fatalf("ping data = %q, want {}", data)
	}
}
