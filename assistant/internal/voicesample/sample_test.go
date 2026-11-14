package voicesample_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// startSampleServer answers one synthesize request with the supplied frames.
func startSampleServer(t *testing.T, format map[string]any, chunks [][]byte, fail bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(line, &request) != nil || request.Type != "synthesize" {
			return
		}
		write := func(value map[string]any, payload []byte) {
			if payload != nil {
				value["payload_length"] = len(payload)
			}
			raw, _ := json.Marshal(value)
			_, _ = conn.Write(append(raw, '\n'))
			if payload != nil {
				_, _ = conn.Write(payload)
			}
		}
		if fail {
			write(map[string]any{"type": "error", "data": map[string]any{"text": "model failed", "code": "synthesis-failed"}}, nil)
			return
		}
		write(map[string]any{"type": "audio-start", "data": format}, nil)
		for _, chunk := range chunks {
			write(map[string]any{"type": "audio-chunk"}, chunk)
		}
		write(map[string]any{"type": "audio-stop"}, nil)
	}()
	return listener.Addr().String()
}

func TestPreviewSynthesizesTheBoundedPhraseToWAV(t *testing.T) {
	addr := startSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}},
		false,
	)
	sampler := voicesample.New(wyoming.NewClient(addr, wyoming.WithTimeout(2*time.Second)), 2*time.Second)
	data, err := sampler.Preview(context.Background(), "/voices/jarvis.safetensors", 1.25)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(data) != 44+8 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("preview = %d bytes, not a 24 kHz PCM16 WAV", len(data))
	}
}

func TestPreviewRejectsAnUnsupportedFormat(t *testing.T) {
	addr := startSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 2},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	sampler := voicesample.New(wyoming.NewClient(addr, wyoming.WithTimeout(2*time.Second)), 2*time.Second)
	if _, err := sampler.Preview(context.Background(), "alba", 1); err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("Preview error = %v, want a format error", err)
	}
}

func TestPreviewReportsAServiceError(t *testing.T) {
	addr := startSampleServer(t, nil, nil, true)
	sampler := voicesample.New(wyoming.NewClient(addr, wyoming.WithTimeout(2*time.Second)), 2*time.Second)
	if _, err := sampler.Preview(context.Background(), "alba", 1); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("Preview error = %v, want the service error", err)
	}
}

// sampleRequest is one synthesize frame the recording server received.
type sampleRequest struct {
	Text  string
	Voice string
	Rate  float64
}

// sampleServer is a multi-connection Wyoming TTS fake that records every
// synthesize request. When block is non-nil, each request waits for one token
// from it before answering, so a test can hold one synthesis open and prove a
// second one waits.
type sampleServer struct {
	addr    string
	format  map[string]any
	chunks  [][]byte
	fail    bool
	block   chan struct{}
	started chan sampleRequest

	mu       sync.Mutex
	requests []sampleRequest
	ln       net.Listener
	wg       sync.WaitGroup
}

func newSampleServer(t *testing.T, format map[string]any, chunks [][]byte, fail bool) *sampleServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &sampleServer{
		addr:    listener.Addr().String(),
		format:  format,
		chunks:  chunks,
		fail:    fail,
		started: make(chan sampleRequest, 8),
		ln:      listener,
	}
	t.Cleanup(func() {
		_ = listener.Close()
		s.wg.Wait()
	})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(conn)
			}()
		}
	}()
	return s
}

func (s *sampleServer) Addr() string { return s.addr }

func (s *sampleServer) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var frame struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(line, &frame) != nil || frame.Type != "synthesize" {
		return
	}
	text, _ := frame.Data["text"].(string)
	voice := ""
	if fields, ok := frame.Data["voice"].(map[string]any); ok {
		voice, _ = fields["name"].(string)
	}
	rate, _ := frame.Data["rate"].(float64)
	request := sampleRequest{Text: text, Voice: voice, Rate: rate}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	fail := s.fail
	s.mu.Unlock()
	s.started <- request
	if s.block != nil {
		<-s.block
	}
	write := func(value map[string]any, payload []byte) {
		if payload != nil {
			value["payload_length"] = len(payload)
		}
		raw, _ := json.Marshal(value)
		_, _ = conn.Write(append(raw, '\n'))
		if payload != nil {
			_, _ = conn.Write(payload)
		}
	}
	if fail {
		write(map[string]any{"type": "error", "data": map[string]any{"text": "model failed", "code": "synthesis-failed"}}, nil)
		return
	}
	write(map[string]any{"type": "audio-start", "data": s.format}, nil)
	for _, chunk := range s.chunks {
		write(map[string]any{"type": "audio-chunk"}, chunk)
	}
	write(map[string]any{"type": "audio-stop"}, nil)
}

func (s *sampleServer) calls() []sampleRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sampleRequest(nil), s.requests...)
}

func newSampler(t *testing.T, s *sampleServer) *voicesample.Sampler {
	t.Helper()
	return voicesample.New(wyoming.NewClient(s.Addr(), wyoming.WithTimeout(5*time.Second)), 5*time.Second)
}

func TestPreviewTextSynthesizesTheTrimmedDraft(t *testing.T) {
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}},
		false,
	)
	data, err := newSampler(t, s).PreviewText(context.Background(), "/voices/jarvis.safetensors", 1.25, "  One moment.\n")
	if err != nil {
		t.Fatalf("PreviewText: %v", err)
	}
	if len(data) != 44+8 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("preview = %d bytes, not a 24 kHz PCM16 WAV", len(data))
	}
	calls := s.calls()
	if len(calls) != 1 {
		t.Fatalf("synth calls = %v", calls)
	}
	if calls[0].Text != "One moment." {
		t.Fatalf("text = %q, want the trimmed draft", calls[0].Text)
	}
	if calls[0].Voice != "/voices/jarvis.safetensors" || calls[0].Rate != 1.25 {
		t.Fatalf("voice/rate = %q/%v", calls[0].Voice, calls[0].Rate)
	}
}

func TestPreviewStillSendsTheFixedPhrase(t *testing.T) {
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	if _, err := newSampler(t, s).Preview(context.Background(), "alba", 1); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	calls := s.calls()
	if len(calls) != 1 || calls[0].Text != voicesample.Phrase {
		t.Fatalf("synth calls = %v, want the fixed phrase", calls)
	}
}

func TestPreviewTextRejectsDraftsBeforeTTS(t *testing.T) {
	cases := []struct {
		name string
		text string
		want error
	}{
		{"empty", "", voicesample.ErrTextEmpty},
		{"whitespace", " \n\t ", voicesample.ErrTextEmpty},
		{"241 ascii runes", strings.Repeat("a", voicesample.MaxPreviewRunes+1), voicesample.ErrTextTooLong},
		// 241 runes but 482 bytes: the bound is a rune count, not a byte count.
		{"241 multi-byte runes", strings.Repeat("é", voicesample.MaxPreviewRunes+1), voicesample.ErrTextTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSampleServer(t,
				map[string]any{"rate": 24000, "width": 2, "channels": 1},
				[][]byte{{1, 2, 3, 4}},
				false,
			)
			if _, err := newSampler(t, s).PreviewText(context.Background(), "alba", 1, tc.text); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if calls := s.calls(); len(calls) != 0 {
				t.Fatalf("a rejected draft reached TTS: %v", calls)
			}
		})
	}
}

func TestPreviewTextAcceptsTheRuneBound(t *testing.T) {
	text := strings.Repeat("é", voicesample.MaxPreviewRunes)
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	if _, err := newSampler(t, s).PreviewText(context.Background(), "alba", 1, text); err != nil {
		t.Fatalf("PreviewText at the bound: %v", err)
	}
	calls := s.calls()
	if len(calls) != 1 || calls[0].Text != text {
		t.Fatalf("synth calls = %v, want the full draft", calls)
	}
}

func TestPreviewTextRejectsAnUnsupportedFormat(t *testing.T) {
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 2},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	if _, err := newSampler(t, s).PreviewText(context.Background(), "alba", 1, "hello"); !errors.Is(err, voicesample.ErrFormat) {
		t.Fatalf("error = %v, want a format error", err)
	}
}

func TestPreviewTextBoundsSynthesizedAudio(t *testing.T) {
	// Two chunks that together exceed the PCM bound, so the limit is crossed
	// on the second read rather than by one oversized frame.
	half := voicesample.MaxPCMBytes/2 + 1
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{make([]byte, half), make([]byte, half)},
		false,
	)
	if _, err := newSampler(t, s).PreviewText(context.Background(), "alba", 1, "hello"); !errors.Is(err, voicesample.ErrTooLong) {
		t.Fatalf("error = %v, want ErrTooLong", err)
	}
}

func TestPreviewTextReportsAServiceError(t *testing.T) {
	s := newSampleServer(t, nil, nil, true)
	if _, err := newSampler(t, s).PreviewText(context.Background(), "alba", 1, "hello"); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("error = %v, want the service error", err)
	}
}

func TestPreviewTextSerialisesConcurrentSynthesis(t *testing.T) {
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	s.block = make(chan struct{})
	sampler := newSampler(t, s)

	first := make(chan error, 1)
	go func() {
		_, err := sampler.PreviewText(context.Background(), "alba", 1, "first")
		first <- err
	}()
	if got := <-s.started; got.Text != "first" {
		t.Fatalf("first request = %+v", got)
	}

	second := make(chan error, 1)
	go func() {
		_, err := sampler.PreviewText(context.Background(), "alba", 1, "second")
		second <- err
	}()
	select {
	case got := <-s.started:
		t.Fatalf("second synthesis reached TTS while the first was in flight: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}

	s.block <- struct{}{}
	if got := <-s.started; got.Text != "second" {
		t.Fatalf("second request = %+v", got)
	}
	s.block <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second: %v", err)
	}
}

func TestPreviewTextWorksOnAStructLiteralSampler(t *testing.T) {
	s := newSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	// The gate is initialised lazily, so a struct literal (as tests elsewhere
	// build) still serialises previews instead of panicking on a nil channel.
	sampler := &voicesample.Sampler{
		Client:  wyoming.NewClient(s.Addr(), wyoming.WithTimeout(5*time.Second)),
		Timeout: 5 * time.Second,
	}
	if _, err := sampler.PreviewText(context.Background(), "alba", 1, "hello"); err != nil {
		t.Fatalf("PreviewText: %v", err)
	}
	if calls := s.calls(); len(calls) != 1 || calls[0].Text != "hello" {
		t.Fatalf("synth calls = %v", calls)
	}
}
