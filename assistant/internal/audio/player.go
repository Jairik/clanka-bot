package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Sink opens one PCM stream on an output device. audio.PulseSink is the
// production implementation; speech and the chime player share the interface.
type Sink interface {
	Open(ctx context.Context, format Format) (io.WriteCloser, error)
}

// Player plays a complete PCM buffer and waits for it to drain. The voice loop
// uses it for the wake chime; replies go through speech.Speaker so they can
// stream.
type Player interface {
	Play(ctx context.Context, pcm []byte, format Format) error
}

// PCMPlayer writes one buffer to a Sink. It works for any Sink implementation,
// including the fake used in tests.
type PCMPlayer struct {
	sink Sink
}

// NewPCMPlayer wraps a sink as a Player.
func NewPCMPlayer(sink Sink) *PCMPlayer {
	return &PCMPlayer{sink: sink}
}

// Play opens the sink, writes pcm and closes it so the audio drains fully.
func (p *PCMPlayer) Play(ctx context.Context, pcm []byte, format Format) error {
	if p.sink == nil {
		return errors.New("audio: player has no sink")
	}
	if len(pcm) == 0 {
		return nil
	}
	writer, err := p.sink.Open(ctx, format)
	if err != nil {
		return err
	}
	if _, err := writer.Write(pcm); err != nil {
		_ = writer.Close()
		return fmt.Errorf("audio: play pcm: %w", err)
	}
	return writer.Close()
}
