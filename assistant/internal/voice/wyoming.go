package voice

import (
	"context"
	"sync"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// WyomingWake adapts the Wyoming detect client to the voice loop's WakeService.
// It owns the goroutine that turns the blocking Next call into a channel.
type WyomingWake struct {
	client *wyoming.Client
	names  []string
	format wyoming.Format
}

// NewWyomingWake builds a wake word service adapter. names limits the models
// that may fire; empty accepts any model the service has loaded.
func NewWyomingWake(client *wyoming.Client, names []string, format wyoming.Format) *WyomingWake {
	return &WyomingWake{client: client, names: names, format: format}
}

// Open starts a detection stream.
func (w *WyomingWake) Open(ctx context.Context) (WakeStream, error) {
	stream, err := w.client.Detect(ctx, w.names, w.format)
	if err != nil {
		return nil, err
	}
	s := &wakeStream{stream: stream, detections: make(chan Detection, 1)}
	go s.pump()
	return s, nil
}

type wakeStream struct {
	stream     *wyoming.DetectStream
	detections chan Detection

	mu  sync.Mutex
	err error
}

// pump reads the single detection or end-of-stream result from the client.
func (s *wakeStream) pump() {
	defer close(s.detections)
	detection, detected, err := s.stream.Next()
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	if err == nil && detected {
		s.detections <- Detection{Name: detection.Name}
	}
}

// Write feeds one frame of mic audio to the wake word model.
func (s *wakeStream) Write(pcm []byte) error { return s.stream.Write(pcm) }

// Detections delivers the firing, then closes.
func (s *wakeStream) Detections() <-chan Detection { return s.detections }

// Err reports why the stream ended, once Detections is closed.
func (s *wakeStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close releases the Wyoming connection.
func (s *wakeStream) Close() error { return s.stream.Close() }

// WyomingSTT adapts the Wyoming transcription client to STTService.
type WyomingSTT struct {
	client *wyoming.Client
	opts   wyoming.TranscribeOptions
	format wyoming.Format
}

// NewWyomingSTT builds a transcription adapter.
func NewWyomingSTT(client *wyoming.Client, opts wyoming.TranscribeOptions, format wyoming.Format) *WyomingSTT {
	return &WyomingSTT{client: client, opts: opts, format: format}
}

// Open starts one transcription stream.
func (s *WyomingSTT) Open(ctx context.Context) (STTStream, error) {
	return s.client.Transcribe(ctx, s.opts, s.format)
}
