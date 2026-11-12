package speech_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// TestSpeakerReadsTheLiveVoiceAndRateEachSay proves a Dashboard or voice write
// reaches the next synthesis without recreating the Speaker.
func TestSpeakerReadsTheLiveVoiceAndRateEachSay(t *testing.T) {
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
				Type string         `json:"type"`
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

	var mu sync.RWMutex
	choice := speech.VoiceSettings{Voice: wyoming.Voice{Name: "alba"}, Rate: 1.0}
	speaker := speech.New(wyoming.NewClient(ln.Addr().String()), newRecorder(), wyoming.Voice{Name: "fallback"}, nil)
	speaker.BindVoice(func() speech.VoiceSettings {
		mu.RLock()
		defer mu.RUnlock()
		return choice
	})

	if _, err := speaker.Say(context.Background(), "One."); err != nil {
		t.Fatalf("first Say: %v", err)
	}
	first := <-requests
	voice := first["voice"].(map[string]any)
	if voice["name"] != "alba" || first["rate"] != 1.0 {
		t.Fatalf("first request = %+v", first)
	}

	mu.Lock()
	choice = speech.VoiceSettings{Voice: wyoming.Voice{Name: "/voices/jarvis.safetensors"}, Rate: 1.3}
	mu.Unlock()
	if _, err := speaker.Say(context.Background(), "Two."); err != nil {
		t.Fatalf("second Say: %v", err)
	}
	second := <-requests
	voice = second["voice"].(map[string]any)
	if voice["name"] != "/voices/jarvis.safetensors" || second["rate"] != 1.3 {
		t.Fatalf("second request = %+v", second)
	}
}
