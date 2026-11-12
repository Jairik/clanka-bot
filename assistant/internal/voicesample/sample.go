// Package voicesample synthesizes one short phrase for the Dashboard's browser
// preview. It asks the same Wyoming TTS service a reply uses, but it never
// touches the speaker, the active voice setting or the audio sink: the PCM is
// collected in memory and returned as a canonical WAV.
package voicesample

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// Phrase is deliberately short and fixed: a preview is a sample, not a reply,
// and every call has the same bounded cost.
const Phrase = "This is a short sample of my voice."

// MaxPCMBytes bounds one synthesized preview. At the TTS service's 24 kHz mono
// PCM16 that is over 80 seconds; the fixed phrase is a few seconds.
const MaxPCMBytes = 4 << 20

// Errors returned by Preview.
var (
	ErrTooLong    = errors.New("voice sample: synthesized audio is over the preview limit")
	ErrEmptyAudio = errors.New("voice sample: the speech service returned no audio")
	ErrFormat     = errors.New("voice sample: the speech service returned an unsupported audio format")
)

// Sampler is the production preview boundary. A nil client or an unwired
// Dashboard option disables previews rather than falling back to the speaker.
type Sampler struct {
	Client  *wyoming.Client
	Timeout time.Duration
}

// New builds a preview sampler over the shared TTS client.
func New(client *wyoming.Client, timeout time.Duration) *Sampler {
	if client == nil {
		return nil
	}
	return &Sampler{Client: client, Timeout: timeout}
}

// Preview synthesizes the fixed phrase in voiceRef at rate and returns a
// complete PCM16 WAV. voiceRef is a built-in name or a path inside the TTS
// voices mount; resolving it is the caller's job. The active voice setting is
// not read or written here.
func (s *Sampler) Preview(ctx context.Context, voiceRef string, rate float64) ([]byte, error) {
	if s == nil || s.Client == nil {
		return nil, errors.New("voice sample: speech service is not configured")
	}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	stream, err := s.Client.Synthesize(ctx, Phrase, wyoming.Voice{Name: voiceRef}, wyoming.WithRate(rate))
	if err != nil {
		return nil, fmt.Errorf("voice sample: %w", err)
	}
	defer stream.Close()

	format := audio.Format{Rate: stream.Format.Rate, Width: stream.Format.Width, Channels: stream.Format.Channels}
	if format.Rate < 8000 || format.Rate > 96000 || format.Width != 2 || format.Channels != 1 {
		return nil, fmt.Errorf("%w: %d Hz, width %d, channels %d", ErrFormat, format.Rate, format.Width, format.Channels)
	}

	pcm := make([]byte, 0, 64<<10)
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("voice sample: %w", err)
		}
		if len(chunk) == 0 {
			continue
		}
		if len(pcm)+len(chunk) > MaxPCMBytes {
			return nil, ErrTooLong
		}
		pcm = append(pcm, chunk...)
	}
	if len(pcm) == 0 {
		return nil, ErrEmptyAudio
	}
	return audio.EncodeWAV(pcm, format), nil
}
