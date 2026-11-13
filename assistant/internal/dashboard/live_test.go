package dashboard_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

// openStream connects to the SSE route as the owner and returns the response
// body reader. The caller closes it, which is how the page going away is
// simulated.
func openStream(t *testing.T, server *httptest.Server) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/ui/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Tailscale-User-Login", owner)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("stream status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		res.Body.Close()
		t.Fatalf("Content-Type = %q, want an event stream", ct)
	}
	if res.Header.Get("Content-Encoding") != "" {
		res.Body.Close()
		t.Fatalf("stream was compressed with %q", res.Header.Get("Content-Encoding"))
	}
	return res, bufio.NewReader(res.Body)
}

// readEvent skips comments and reads one event frame.
func readEvent(t *testing.T, reader *bufio.Reader, deadline time.Duration) (name, data string) {
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
		t.Fatalf("no event within %v", deadline)
		return "", ""
	}
}

func TestLiveStreamRefusesRequestsWithoutTheDashboardIdentity(t *testing.T) {
	handler := newHandler(t, dashboard.Options{Activity: activity.New(10)})
	server := httptest.NewServer(handler)
	defer server.Close()

	res, err := http.Get(server.URL + "/api/ui/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
}

func TestLiveStreamSendsACompletedRequestAsOneDelta(t *testing.T) {
	ring := activity.New(10)
	handler := newHandler(t, dashboard.Options{Activity: ring})
	server := httptest.NewServer(handler)
	defer server.Close()

	res, reader := openStream(t, server)
	defer res.Body.Close()

	// The observer runs in the request that completed; the stream is already
	// open, so this delta is pushed rather than polled.
	ring.Observe(latency.Completed{
		ID:         "req-live",
		Source:     latency.SourceVoice,
		Path:       "fast_path",
		Tool:       "calendar.get_events",
		Transcript: "what is on today",
		Outcome:    latency.OutcomeOK,
		Detail:     &latency.Detail{Reply: "nothing until four"},
	})

	name, data := readEvent(t, reader, 5*time.Second)
	if name != "activity" {
		t.Fatalf("event = %q, want activity", name)
	}
	var entry struct {
		ID         string
		Transcript string
		Reply      string
		Source     string
	}
	if err := json.Unmarshal([]byte(data), &entry); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if entry.ID != "req-live" || entry.Transcript != "what is on today" || entry.Reply != "nothing until four" {
		t.Fatalf("event entry = %+v", entry)
	}
}

func TestLiveStreamClosesWhenThePageGoesAway(t *testing.T) {
	ring := activity.New(10)
	handler := newHandler(t, dashboard.Options{Activity: ring})
	server := httptest.NewServer(handler)
	defer server.Close()

	res, reader := openStream(t, server)
	res.Body.Close()

	// The request context is cancelled by the close; the handler must return
	// instead of blocking forever, and a later request still works (which also
	// proves the last unsubscribe released the hub).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ring.Observe(latency.Completed{ID: "after-close", Transcript: "still here", Outcome: latency.OutcomeOK})
		time.Sleep(10 * time.Millisecond)
		if _, err := reader.ReadByte(); err != nil {
			break
		}
	}
	res2, reader2 := openStream(t, server)
	defer res2.Body.Close()
	ring.Observe(latency.Completed{ID: "req-second", Transcript: "second stream", Outcome: latency.OutcomeOK})
	name, _ := readEvent(t, reader2, 5*time.Second)
	if name != "activity" {
		t.Fatalf("second stream event = %q, want activity", name)
	}
}
