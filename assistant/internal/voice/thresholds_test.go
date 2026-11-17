package voice_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
	"github.com/Jairik/clanka-bot/assistant/internal/wakethreshold"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// staticThresholds is a fixed map for tests that only need the seam.
type staticThresholds map[string]float64

// Map returns a copy of the fixed thresholds map.
func (s staticThresholds) Map() map[string]float64 {
	copied := make(map[string]float64, len(s))
	for name, threshold := range s {
		copied[name] = threshold
	}
	return copied
}

// Signature builds a stable string representing the static thresholds.
func (s staticThresholds) Signature() string {
	signature := ""
	for name, threshold := range s {
		signature += name + "=" + fmt.Sprint(threshold) + ";"
	}
	return signature
}

// liveThresholds stands in for the settings-backed source: a test moves the
// signature the way a stored write would.
type liveThresholds struct {
	mu        sync.Mutex
	signature string
	values    map[string]float64
}

// Map returns a copy of the dynamic thresholds map.
func (l *liveThresholds) Map() map[string]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := make(map[string]float64, len(l.values))
	for name, threshold := range l.values {
		copied[name] = threshold
	}
	return copied
}

// Signature returns the current signature under lock.
func (l *liveThresholds) Signature() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.signature
}

// set updates the signature under lock.
func (l *liveThresholds) set(signature string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.signature = signature
}

// thresholdServer records the thresholds map on every detect request. It then
// ends the stream, which is all the Open behaviour under test needs.
func thresholdServer(t *testing.T) (string, <-chan map[string]float64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	records := make(chan map[string]float64, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				line, err := reader.ReadBytes('\n')
				if err != nil {
					return
				}
				var event struct {
					Type string         `json:"type"`
					Data map[string]any `json:"data"`
				}
				if err := json.Unmarshal(line, &event); err != nil || event.Type != "detect" {
					return
				}
				thresholds := make(map[string]float64)
				raw, _ := event.Data["thresholds"].(map[string]any)
				for name, value := range raw {
					if threshold, ok := value.(float64); ok {
						thresholds[name] = threshold
					}
				}
				records <- thresholds
				_, _ = fmt.Fprintf(conn, `{"type":"not-detected","data":{}}`+"\n")
			}(conn)
		}
	}()
	return listener.Addr().String(), records
}

// nextThresholds receives the next detected threshold map from the record channel.
func nextThresholds(t *testing.T, records <-chan map[string]float64) map[string]float64 {
	t.Helper()
	select {
	case thresholds := <-records:
		return thresholds
	case <-time.After(3 * time.Second):
		t.Fatal("the wake service did not receive a detect request")
		return nil
	}
}

// TestOpenSendsTheLiveThresholdsAfterAStoredChange is the end-to-end seam:
// the settings write changes the map the next detect request carries, with no
// restart and no new constructor.
func TestOpenSendsTheLiveThresholdsAfterAStoredChange(t *testing.T) {
	addr, records := thresholdServer(t)
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	cfg := config.Wakeword{
		Models:     []string{"clanker"},
		Thresholds: map[string]float64{"clanker": 0.6},
	}
	service, err := settings.New(ctx, st, wakethreshold.Definitions(cfg)...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	wake := voice.NewWyomingWake(wyoming.NewClient(addr), cfg.DetectModels(),
		wyoming.Format{Rate: 16000, Width: 2, Channels: 1},
		wakethreshold.NewSource(service, cfg))

	stream, err := wake.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	if got := nextThresholds(t, records); got["clanker"] != 0.6 {
		t.Fatalf("first detect thresholds = %v, want the configured 0.6", got)
	}

	if _, err := service.Set(ctx, wakethreshold.Key("clanker"), 0.72); err != nil {
		t.Fatalf("Set: %v", err)
	}
	second, err := wake.Open(ctx)
	if err != nil {
		t.Fatalf("Open after Set: %v", err)
	}
	defer second.Close()
	if got := nextThresholds(t, records); got["clanker"] != 0.72 {
		t.Fatalf("detect after the stored change = %v, want 0.72", got)
	}
}

// TestLoopReopensTheStreamWhenTheThresholdSignatureChanges proves the idle
// detect stream does not wait for the next turn: a changed signature ends the
// open stream without firing, and the loop reopens it with the new map.
func TestLoopReopensTheStreamWhenTheThresholdSignatureChanges(t *testing.T) {
	source := &scriptSource{frames: repeat(silentFrame(), 4000)}
	// A very late firing keeps the first stream healthy while the test edits.
	wake := &fakeWake{afterWords: 1 << 30, name: "clanker"}
	stt := &fakeSTT{text: "should not happen"}
	responder := &fakeResponder{calls: make(chan string, 1)}
	thresholds := &liveThresholds{
		signature: "clanker=0.6",
		values:    map[string]float64{"clanker": 0.6},
	}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: stt, Responder: responder,
			WakeWords: []string{"clanker"}, Thresholds: thresholds,
		})
	runLoop(t, loop)

	waitFor(t, 2*time.Second, func() bool {
		streams, open, _ := wake.snapshot()
		return streams == 1 && open == 1
	})
	thresholds.set("clanker=0.72")
	waitFor(t, 2*time.Second, func() bool {
		streams, open, _ := wake.snapshot()
		// The old stream closed and a new one opened with the new map.
		return streams >= 2 && open == 1
	})
	if opens := stt.openCount(); opens != 0 {
		t.Fatalf("STT opened %d streams for a threshold reopen", opens)
	}
	if wakes := responder.wakeWords(); len(wakes) != 0 {
		t.Fatalf("responder saw %v for a threshold reopen", wakes)
	}
}

// TestLoopKeepsAHealthyStreamWhenTheSignatureIsUnchanged guards the other
// half: an unrelated write must not churn the detect stream.
func TestLoopKeepsAHealthyStreamWhenTheSignatureIsUnchanged(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 4000)}
	wake := &fakeWake{afterWords: 1 << 30, name: "clanker"}
	thresholds := &liveThresholds{
		signature: "clanker=0.6",
		values:    map[string]float64{"clanker": 0.6},
	}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: &fakeSTT{}, Responder: &fakeResponder{calls: make(chan string, 1)},
			WakeWords: []string{"clanker"}, Thresholds: thresholds,
		})
	runLoop(t, loop)

	// Wait until plenty of frames reached the wake stream, then check that the
	// loop never reopened a healthy stream.
	waitFor(t, 2*time.Second, func() bool { return wake.loudFrames() > 20 })
	streams, open, _ := wake.snapshot()
	if streams != 1 || open != 1 {
		t.Fatalf("streams/opens = %d/%d, want the one healthy stream", streams, open)
	}
}
