package wyoming_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// audioEvent is one decoded event including its inline data.
type audioEvent struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// readAudioEvent reads one event that may carry a binary payload.
func readAudioEvent(t *testing.T, r *bufio.Reader) (audioEvent, []byte) {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Errorf("read event: %v", err)
		return audioEvent{}, nil
	}
	var header struct {
		audioEvent
		PayloadLength int `json:"payload_length"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		t.Errorf("decode event %q: %v", line, err)
		return audioEvent{}, nil
	}
	var payload []byte
	if header.PayloadLength > 0 {
		payload = make([]byte, header.PayloadLength)
		if _, err := io.ReadFull(r, payload); err != nil {
			t.Errorf("read payload: %v", err)
		}
	}
	return header.audioEvent, payload
}

func TestDetectSendsAudioAndReportsDetection(t *testing.T) {
	var gotDetectionName string
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		detect := readWireEvent(t, r)
		if detect.Type != "detect" {
			t.Errorf("first event = %q, want detect", detect.Type)
		}
		names, _ := dataMap(t, detect)["names"].([]any)
		if len(names) != 1 || names[0] != "okay_nabu" {
			t.Errorf("names = %v, want [okay_nabu]", names)
		}
		start := readWireEvent(t, r)
		startData := dataMap(t, start)
		if start.Type != "audio-start" || startData["rate"] != float64(16000) {
			t.Errorf("audio-start = %q %v", start.Type, startData)
		}
		chunk, pcm := readAudioEvent(t, r)
		chunkData := chunk.Data
		if chunk.Type != "audio-chunk" || string(pcm) != "\x01\x02" {
			t.Errorf("chunk = %q %v", chunk.Type, pcm)
		}
		if chunkData["rate"] != float64(16000) || chunkData["width"] != float64(2) || chunkData["channels"] != float64(1) {
			t.Errorf("audio-chunk format = %v, want 16000/2/1", chunkData)
		}
		gotDetectionName = "okay_nabu"
		writeWireEvent(t, conn, "detection", map[string]any{"name": "okay_nabu", "timestamp": 1300})
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Detect(ctx, []string{"okay_nabu"}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	defer stream.Close()
	if err := stream.Write([]byte{1, 2}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	detection, detected, err := stream.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !detected || detection.Name != gotDetectionName {
		t.Fatalf("detection = %+v detected=%v", detection, detected)
	}
	if detection.Timestamp != 1300 {
		t.Fatalf("timestamp = %d, want 1300", detection.Timestamp)
	}
}

func TestDetectSendsPerModelThresholds(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		detect := readWireEvent(t, r)
		data := dataMap(t, detect)
		thresholds, _ := data["thresholds"].(map[string]any)
		if thresholds["clanker"] != 0.6 || thresholds["okay_nabu"] != 0.5 {
			t.Errorf("thresholds = %v, want clanker 0.6 and okay_nabu 0.5", thresholds)
		}
		readWireEvent(t, r) // audio-start
		writeWireEvent(t, conn, "not-detected", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Detect(ctx, []string{"clanker", "okay_nabu"}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1},
		wyoming.WithDetectThresholds(map[string]float64{"clanker": 0.6, "okay_nabu": 0.5}))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	defer stream.Close()
	if _, detected, err := stream.Next(); err != nil || detected {
		t.Fatalf("Next = detected=%v err=%v", detected, err)
	}
}

func TestDetectOmitsThresholdsByDefault(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		detect := readWireEvent(t, r)
		if _, present := dataMap(t, detect)["thresholds"]; present {
			t.Error("detect carried thresholds without the option")
		}
		readWireEvent(t, r) // audio-start
		writeWireEvent(t, conn, "not-detected", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Detect(ctx, nil, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	defer stream.Close()
	if _, _, err := stream.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
}

func TestDetectReportsNoDetection(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r) // detect
		readWireEvent(t, r) // audio-start
		writeWireEvent(t, conn, "not-detected", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Detect(ctx, nil, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	defer stream.Close()

	if _, detected, err := stream.Next(); err != nil || detected {
		t.Fatalf("Next = detected=%v err=%v, want no detection", detected, err)
	}
}

func TestDetectStopsWhenContextCancelled(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r)
		readWireEvent(t, r)
		// Never answer, so Next blocks until the context is cancelled.
		<-time.After(2 * time.Second)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Detect(ctx, nil, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := stream.Next()
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Next succeeded after cancel, want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return after cancel")
	}
}

func TestTranscribeStreamsAudioAndReturnsTheFinalTranscript(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		transcribe := readWireEvent(t, r)
		data := dataMap(t, transcribe)
		if transcribe.Type != "transcribe" || data["language"] != "en" {
			t.Errorf("transcribe = %q %v", transcribe.Type, data)
		}
		start := readWireEvent(t, r)
		if start.Type != "audio-start" {
			t.Errorf("second event = %q, want audio-start", start.Type)
		}
		chunk, pcm := readAudioEvent(t, r)
		chunkData := chunk.Data
		if chunk.Type != "audio-chunk" || string(pcm) != "\x01\x02" {
			t.Errorf("chunk = %q %v", chunk.Type, pcm)
		}
		if chunkData["rate"] != float64(16000) || chunkData["width"] != float64(2) || chunkData["channels"] != float64(1) {
			t.Errorf("audio-chunk format = %v, want 16000/2/1", chunkData)
		}
		stop := readWireEvent(t, r)
		if stop.Type != "audio-stop" {
			t.Errorf("stop = %q, want audio-stop", stop.Type)
		}
		writeWireEvent(t, conn, "transcript", map[string]any{"text": "what time is it", "language": "en"})
		writeWireEvent(t, conn, "transcript-stop", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{Language: "en"}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()
	if err := stream.Write([]byte{1, 2}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	text, err := stream.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if text != "what time is it" {
		t.Fatalf("transcript = %q", text)
	}
}

func TestTranscribeFallsBackToTheLastPartial(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r)
		readWireEvent(t, r)
		readWireEvent(t, r) // audio-stop
		writeWireEvent(t, conn, "transcript-chunk", map[string]any{"text": "what time"})
		writeWireEvent(t, conn, "transcript-chunk", map[string]any{"text": "what time is it"})
		writeWireEvent(t, conn, "transcript-stop", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()
	text, err := stream.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if text != "what time is it" {
		t.Fatalf("transcript = %q", text)
	}
}

func TestTranscribeReportsServerError(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r)
		readWireEvent(t, r)
		readWireEvent(t, r) // audio-stop
		writeWireEvent(t, conn, "error", map[string]any{"text": "model not loaded", "code": "transcription-failed"})
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()
	_, err = stream.Finish()
	if err == nil || !strings.Contains(err.Error(), "model not loaded") {
		t.Fatalf("error = %v, want server text", err)
	}
}
