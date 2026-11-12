package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// Filler is a short phrase synthesized once at startup and replayed from memory,
// so the Agent path can cue the user without waiting on TTS again.
type Filler struct {
	Text   string
	Format audio.Format
	PCM    []byte
}

// SynthesizeFiller renders the whole phrase through the TTS service once and
// caches the PCM. Startup calls it before the request loop begins.
func SynthesizeFiller(ctx context.Context, client *wyoming.Client, voice wyoming.Voice, text string, opts ...wyoming.SynthesisOption) (*Filler, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("speech: filler text is required")
	}
	if client == nil {
		return nil, errors.New("speech: tts client is required")
	}
	stream, err := client.Synthesize(ctx, text, voice, opts...)
	if err != nil {
		return nil, fmt.Errorf("speech: synthesize filler: %w", err)
	}
	defer stream.Close()

	var pcm bytes.Buffer
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("speech: synthesize filler: %w", err)
		}
		pcm.Write(chunk)
	}
	if pcm.Len() == 0 {
		return nil, errors.New("speech: filler audio is empty")
	}
	return &Filler{
		Text:   text,
		Format: audio.Format{Rate: stream.Format.Rate, Width: stream.Format.Width, Channels: stream.Format.Channels},
		PCM:    pcm.Bytes(),
	}, nil
}

// Play writes the cached PCM to sink. It makes no TTS call, so the phrase is
// heard immediately; cancelling ctx stops the audio.
func (f *Filler) Play(ctx context.Context, sink Sink) error {
	if f == nil || len(f.PCM) == 0 {
		return nil
	}
	if sink == nil {
		return errors.New("speech: sink is required")
	}
	writer, err := sink.Open(ctx, f.Format)
	if err != nil {
		return err
	}
	// A Filler plays on the same turn context as the reply, so the same quiet
	// gain applies to its cached PCM.
	writer = newGainWriter(writer, f.Format, GainFrom(ctx))
	if _, err := writer.Write(f.PCM); err != nil {
		_ = writer.Close()
		return fmt.Errorf("speech: play filler: %w", err)
	}
	return writer.Close()
}
