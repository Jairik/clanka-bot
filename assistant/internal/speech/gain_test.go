package speech_test

import (
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

// captureSink records every byte accepted by one opened writer and acts as the
// fake player seam; no audio device is touched.
type captureSink struct {
	format audio.Format
	writes [][]byte
}

func (s *captureSink) Open(_ context.Context, format audio.Format) (io.WriteCloser, error) {
	s.format = format
	return &captureWriter{sink: s}, nil
}

type captureWriter struct{ sink *captureSink }

func (w *captureWriter) Write(p []byte) (int, error) {
	w.sink.writes = append(w.sink.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (w *captureWriter) Close() error { return nil }

func (s *captureSink) pcm() []byte {
	var out []byte
	for _, chunk := range s.writes {
		out = append(out, chunk...)
	}
	return out
}

func TestGainScalesPCM16AndBuffersSplitSamples(t *testing.T) {
	sink := &captureSink{}
	ctx := speech.WithGain(context.Background(), 50)
	// Use the public gainWriter through Filler.Play, which exercises the same
	// wrapper the Speaker uses.
	filler := &speech.Filler{
		Text:   "test",
		Format: audio.Format{Rate: 16000, Width: 2, Channels: 1},
		PCM:    samples(1000, -1000, 2000),
	}
	if err := filler.Play(ctx, sink); err != nil {
		t.Fatalf("Filler.Play: %v", err)
	}
	got := sink.pcm()
	if len(got) != 6 {
		t.Fatalf("played %d bytes, want 6", len(got))
	}
	for i, want := range []int16{500, -500, 1000} {
		if value := int16(binary.LittleEndian.Uint16(got[i*2:])); value != want {
			t.Fatalf("sample %d = %d, want %d", i, value, want)
		}
	}
}

func TestGainFromDefaultsToNormalAndClamps(t *testing.T) {
	if got := speech.GainFrom(context.Background()); got != 100 {
		t.Fatalf("GainFrom = %d, want 100", got)
	}
	if got := speech.GainFrom(speech.WithGain(context.Background(), 35)); got != 35 {
		t.Fatalf("GainFrom = %d, want 35", got)
	}
	if got := speech.GainFrom(speech.WithGain(context.Background(), 500)); got != 100 {
		t.Fatalf("GainFrom clamped = %d, want 100", got)
	}
	if got := speech.GainFrom(speech.WithGain(context.Background(), -5)); got != 0 {
		t.Fatalf("GainFrom clamped = %d, want 0", got)
	}
}

// samples builds one mono PCM16 buffer.
func samples(values ...int16) []byte {
	out := make([]byte, len(values)*2)
	for i, value := range values {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(value))
	}
	return out
}
