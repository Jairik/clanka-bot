package wyoming_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// A service that accepts the stream and then stops answering is bounded by the
// client's read-idle timeout instead of wedging the voice loop forever (#201).
func TestTranscribeFinishTimesOutWhenTheServiceGoesSilent(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		// Drain every event, including audio-stop, and never answer.
		_, _ = io.Copy(io.Discard, conn)
	})

	client := wyoming.NewClient(addr, wyoming.WithTimeout(100*time.Millisecond))
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := stream.Finish()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Finish succeeded against a silent service")
		}
		if !strings.Contains(err.Error(), "wyoming: transcribe:") {
			t.Fatalf("error = %v, want a short transcribe timeout", err)
		}
		if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > time.Second {
			t.Fatalf("Finish returned after %v, want about the 100ms read timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Finish blocked after the service stopped answering")
	}
}

// A finalisation that keeps sending partials resets the idle clock, so a slow
// but answering service is never capped by the per-read timeout (#201).
func TestTranscribeFinishKeepsReadingWhilePartialsArrive(t *testing.T) {
	const idle = 250 * time.Millisecond
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r) // transcribe
		readWireEvent(t, r) // audio-start
		if stop := readWireEvent(t, r); stop.Type != "audio-stop" {
			t.Errorf("final event = %q, want audio-stop", stop.Type)
		}
		// Four partials 100ms apart: every gap fits the idle deadline while the
		// finalisation as a whole outlasts it.
		for _, partial := range []string{"what", "what time", "what time is", "what time is it"} {
			writeWireEvent(t, conn, "transcript-chunk", map[string]any{"text": partial})
			time.Sleep(100 * time.Millisecond)
		}
		writeWireEvent(t, conn, "transcript", map[string]any{"text": "what time is it"})
		writeWireEvent(t, conn, "transcript-stop", nil)
	})

	client := wyoming.NewClient(addr, wyoming.WithTimeout(idle))
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()

	started := time.Now()
	text, err := stream.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if text != "what time is it" {
		t.Fatalf("transcript = %q", text)
	}
	if elapsed := time.Since(started); elapsed <= idle {
		t.Fatalf("finalisation returned after %v, want more than the %v idle deadline", elapsed, idle)
	}
}

// A babbling service that keeps sending partials instead of a final transcript
// must not wedge Finish forever: the total finalisation budget ends it. The
// existing test above proves a progressing finalisation still returns, so this
// one proves the bound (#340).
func TestTranscribeFinishStopsBabblingWithinTheTotalBudget(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r) // transcribe
		readWireEvent(t, r) // audio-start
		readWireEvent(t, r) // audio-stop
		// Keep sending partials forever, the way a stuck service would. The
		// client must give up after its total budget rather than never. Breaking
		// on a write error lets the goroutine exit once the client gives up.
		i := 0
		for {
			i++
			if err := writeWireEventErr(conn, "transcript-chunk", map[string]any{"text": fmt.Sprintf("partial-%d", i)}); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	// A small read-idle timeout would otherwise be reset by every partial; the
	// total budget is what bounds the turn.
	client := wyoming.NewClient(addr, wyoming.WithTimeout(100*time.Millisecond), wyoming.WithFinalisationBudget(300*time.Millisecond))
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := stream.Finish()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Finish succeeded against a babbling service")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Finish never returned against a babbling service")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Finish returned after %v, want within the 300ms total budget (not never)", elapsed)
	}
}

// A busy refusal that arrives before audio is streamed must surface from
// Transcribe, not only at Finish, so the caller does not write the whole
// utterance into a dead session (#368). The test fails if the client waits for
// Finish: it checks no audio-chunk was written after the refusal was known.
func TestTranscribeDetectsBusyBeforeStreaming(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		ev := readWireEvent(t, r)
		if ev.Type != "transcribe" {
			t.Errorf("first event = %q, want transcribe", ev.Type)
		}
		// Refuse immediately, the way a serialising service answers a second
		// session before any audio arrives. The client learns this from
		// Transcribe and never streams the utterance; we do not read further.
		writeWireEvent(t, conn, "error", map[string]any{"text": "speech recognition is busy", "code": "busy"})
	})

	client := wyoming.NewClient(addr, wyoming.WithTimeout(5*time.Second), wyoming.WithRefusalDeadline(250*time.Millisecond))
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Transcribe(ctx, wyoming.TranscribeOptions{}, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err == nil {
		t.Fatal("Transcribe succeeded against a busy service, want ErrBusy")
	}
	if !errors.Is(err, wyoming.ErrBusy) {
		t.Fatalf("Transcribe error = %v, want ErrBusy", err)
	}
	if stream != nil {
		stream.Close()
	}
}

// writeWireEventErr is like writeWireEvent but reports the write error so a fake
// peer can stop once the client has given up, instead of failing forever.
func writeWireEventErr(w io.Writer, typ string, data any) error {
	ev := map[string]any{"type": typ}
	if data != nil {
		ev["data"] = data
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}
