package audio_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

func TestChimeIsAudibleMonoPCM16(t *testing.T) {
	const rate = 16000
	pcm := audio.Chime(rate)
	if len(pcm) == 0 {
		t.Fatal("Chime returned no audio")
	}
	if len(pcm)%2 != 0 {
		t.Fatalf("chime length %d is not a whole number of 16-bit samples", len(pcm))
	}

	// The two notes are 0.25 s plus a 20 ms gap: 0.27 s at 16 kHz.
	wantSamples := int(0.27 * rate)
	if got := len(pcm) / 2; got < wantSamples-2 || got > wantSamples+2 {
		t.Fatalf("chime has %d samples, want about %d", got, wantSamples)
	}

	peak := 0
	for i := 0; i < len(pcm); i += 2 {
		sample := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	if peak < 2000 {
		t.Fatalf("chime peak %d is too quiet", peak)
	}
	if peak > 12000 {
		t.Fatalf("chime peak %d is too loud for a wake cue", peak)
	}
}

func TestChimeScalesWithSampleRate(t *testing.T) {
	if got, want := len(audio.Chime(8000))*2, len(audio.Chime(16000)); got < want-64 || got > want+64 {
		t.Fatalf("8 kHz chime is %d bytes, want about half of %d", got, want)
	}
	if audio.Chime(0) != nil {
		t.Fatal("Chime(0) should return nil")
	}
}

// fakeSink records every buffer it is asked to play.
type fakeSink struct {
	format audio.Format
	writes [][]byte
	err    error
}

func (s *fakeSink) Open(ctx context.Context, format audio.Format) (io.WriteCloser, error) {
	s.format = format
	if s.err != nil {
		return nil, s.err
	}
	return &fakeWriter{sink: s}, nil
}

type fakeWriter struct{ sink *fakeSink }

func (w *fakeWriter) Write(p []byte) (int, error) {
	w.sink.writes = append(w.sink.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (w *fakeWriter) Close() error { return nil }

func TestPCMPlayerWritesTheWholeBufferToTheSink(t *testing.T) {
	sink := &fakeSink{}
	player := audio.NewPCMPlayer(sink)
	format := audio.Format{Rate: 16000, Width: 2, Channels: 1}

	if err := player.Play(context.Background(), []byte{1, 2, 3, 4}, format); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if sink.format != format {
		t.Fatalf("sink format = %+v, want %+v", sink.format, format)
	}
	if len(sink.writes) != 1 || len(sink.writes[0]) != 4 {
		t.Fatalf("sink writes = %v, want one 4-byte write", sink.writes)
	}
}

func TestPCMPlayerReportsSinkFailure(t *testing.T) {
	player := audio.NewPCMPlayer(&fakeSink{err: errors.New("no sink")})
	err := player.Play(context.Background(), []byte{1, 2}, audio.Format{Rate: 16000, Width: 2, Channels: 1})
	if err == nil {
		t.Fatal("Play succeeded, want sink error")
	}
}
