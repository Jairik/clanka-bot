package wyoming_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// busyPeer is a fake speech-to-text service that keeps the real server's
// contract (#196): one session holds the model at a time, and a second
// connection that asks to transcribe is refused with the busy error while the
// first one keeps its answer.
type busyPeer struct {
	addr string
	// holding closes when the first session has the model.
	holding chan struct{}
	// release closes when the test lets the first session answer.
	release chan struct{}
}

func newBusyPeer(t *testing.T) *busyPeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &busyPeer{
		addr:    ln.Addr().String(),
		holding: make(chan struct{}),
		release: make(chan struct{}),
	}
	go func() {
		first, err := ln.Accept()
		if err != nil {
			return
		}
		go p.serveHolder(first)
		second, err := ln.Accept()
		if err != nil {
			return
		}
		go p.serveRefused(second)
	}()
	return p
}

// serveHolder takes the model, reads the whole turn, and answers only when the
// test releases it.
func (p *busyPeer) serveHolder(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		ev, err := peerNext(r)
		if err != nil {
			return
		}
		switch ev.Type {
		case "transcribe":
			close(p.holding)
		case "audio-stop":
			<-p.release
			peerWrite(conn, "transcript", map[string]any{"text": "what time is it", "language": "en"})
			peerWrite(conn, "transcript-stop", nil)
			return
		}
	}
}

// serveRefused answers the busy error as soon as the second session asks to
// transcribe, then drains the events the client had already queued.
func (p *busyPeer) serveRefused(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		ev, err := peerNext(r)
		if err != nil {
			return
		}
		if ev.Type == "transcribe" {
			peerWrite(conn, "error", map[string]any{
				"text": "speech recognition is busy",
				"code": "busy",
			})
			_, _ = io.Copy(io.Discard, conn)
			return
		}
	}
}

// peerNext reads one wire event for a fake peer. It returns errors instead of
// failing a test from a goroutine, and discards binary payloads the way
// readAudioEvent does for the client-side fakes.
func peerNext(r *bufio.Reader) (wireEvent, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return wireEvent{}, err
	}
	var header struct {
		wireEvent
		PayloadLength int `json:"payload_length"`
		DataLength    int `json:"data_length"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return wireEvent{}, err
	}
	skip := header.PayloadLength + header.DataLength
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(skip)); err != nil {
			return wireEvent{}, err
		}
	}
	return header.wireEvent, nil
}

func peerWrite(w io.Writer, typ string, data any) {
	ev := map[string]any{"type": typ}
	if data != nil {
		ev["data"] = data
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}

// A second transcription while one holds the model must come back as a busy
// refusal, not a read timeout, and the session that holds the model must still
// finish. Both finalisations run at once, so this also exercises the client
// under -race.
func TestSecondTranscriptionIsRefusedAsBusyWhileTheFirstHoldsTheModel(t *testing.T) {
	peer := newBusyPeer(t)
	client := wyoming.NewClient(peer.addr, wyoming.WithTimeout(5*time.Second))
	ctx := context.Background()
	format := wyoming.Format{Rate: 16000, Width: 2, Channels: 1}

	first, err := client.Transcribe(ctx, wyoming.TranscribeOptions{Language: "en"}, format)
	if err != nil {
		t.Fatalf("Transcribe (first): %v", err)
	}
	defer first.Close()
	if err := first.Write([]byte{1, 2}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-peer.holding:
	case <-time.After(2 * time.Second):
		t.Fatal("the first session never reached the fake service")
	}

	second, err := client.Transcribe(ctx, wyoming.TranscribeOptions{Language: "en"}, format)
	if err != nil {
		t.Fatalf("Transcribe (second): %v", err)
	}
	defer second.Close()

	type result struct {
		text string
		err  error
	}
	firstDone := make(chan result, 1)
	go func() {
		text, err := first.Finish()
		firstDone <- result{text, err}
	}()

	if _, err := second.Finish(); !errors.Is(err, wyoming.ErrBusy) {
		t.Fatalf("second Finish error = %v, want ErrBusy", err)
	} else {
		var serverErr *wyoming.ServerError
		if !errors.As(err, &serverErr) || serverErr.Code != "busy" {
			t.Fatalf("second Finish error = %#v, want a ServerError with code busy", err)
		}
		if !strings.Contains(err.Error(), "busy") {
			t.Fatalf("second Finish error = %q, want the service's wording", err)
		}
	}

	close(peer.release)
	select {
	case got := <-firstDone:
		if got.err != nil {
			t.Fatalf("first Finish: %v", got.err)
		}
		if got.text != "what time is it" {
			t.Fatalf("first transcript = %q", got.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Finish did not return after the service answered")
	}
}

// An idle session the service ends is a different failure from a busy
// refusal, and a caller that retries a busy turn must not treat the two alike.
func TestAnIdleTimeoutIsNotReportedAsBusy(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		readWireEvent(t, r) // transcribe
		readWireEvent(t, r) // audio-start
		readWireEvent(t, r) // audio-stop
		writeWireEvent(t, conn, "error", map[string]any{
			"text": "speech recognition timed out waiting for audio",
			"code": "idle-timeout",
		})
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
	if err == nil {
		t.Fatal("Finish succeeded against an ended session")
	}
	if errors.Is(err, wyoming.ErrBusy) {
		t.Fatalf("idle timeout reported as busy: %v", err)
	}
	var serverErr *wyoming.ServerError
	if !errors.As(err, &serverErr) || serverErr.Code != "idle-timeout" {
		t.Fatalf("Finish error = %#v, want a ServerError with code idle-timeout", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Finish error = %q, want the service's wording", err)
	}
}
