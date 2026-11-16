package speech_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// TestContextRateOverridesSynthesisWithoutChangingSetting proves a per-utterance
// WithRate reaches the TTS request while the live voice.rate setting is left
// intact for the next, ordinary synthesis.
func TestContextRateOverridesSynthesisWithoutChangingSetting(t *testing.T) {
	requests := make(chan map[string]any, 2)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(conn)
			line, err := r.ReadString('\n')
			if err != nil {
				conn.Close()
				return
			}
			var request struct {
				Data map[string]any `json:"data"`
			}
			_ = json.Unmarshal([]byte(line), &request)
			requests <- request.Data
			writeEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
			writeAudio(t, conn, []byte{1, 2})
			writeEvent(t, conn, "audio-stop", nil)
			conn.Close()
		}
	}()

	// The persistent setting is 1.0 and is never written by this test.
	current := speech.VoiceSettings{Voice: wyoming.Voice{Name: "alba"}, Rate: 1.0}
	speaker := speech.New(wyoming.NewClient(ln.Addr().String()), newRecorder(), wyoming.Voice{Name: "fallback"}, nil)
	speaker.BindVoice(func() speech.VoiceSettings { return current })

	// A replay at a different speed overrides the setting for one Say.
	ctx := speech.WithRate(context.Background(), 0.6)
	if _, err := speaker.Say(ctx, "One."); err != nil {
		t.Fatalf("override Say: %v", err)
	}
	first := <-requests
	if first["rate"] != 0.6 {
		t.Fatalf("first request rate = %v, want the 0.6 override", first["rate"])
	}

	// The next, ordinary reply inherits the unchanged setting.
	if _, err := speaker.Say(context.Background(), "Two."); err != nil {
		t.Fatalf("ordinary Say: %v", err)
	}
	second := <-requests
	if second["rate"] != 1.0 {
		t.Fatalf("second request rate = %v, want 1.0 from the setting", second["rate"])
	}
	if current.Rate != 1.0 {
		t.Fatalf("voice.rate source changed to %v", current.Rate)
	}

	// A non-positive override is a no-op: the context is left unchanged.
	plain := context.Background()
	if got := speech.WithRate(plain, 0); got != plain {
		t.Fatal("WithRate(ctx, 0) changed the context")
	}
	if got := speech.RateFrom(plain); got != 0 {
		t.Fatalf("RateFrom(plain) = %v, want 0", got)
	}
	if got := speech.RateFrom(speech.WithRate(plain, 1.4)); got != 1.4 {
		t.Fatalf("RateFrom(WithRate(ctx, 1.4)) = %v, want 1.4", got)
	}
}
