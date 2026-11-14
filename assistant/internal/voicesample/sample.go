// Package voicesample synthesizes one short phrase for the Dashboard's voice
// preview. It asks the same Wyoming TTS service a reply uses, but it never
// touches the speaker, the active voice setting or the audio sink: the PCM is
// collected in memory and returned to the caller, which either serves it as a
// WAV to the browser or queues it for the Pi speaker.
package voicesample

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// Phrase is the sample spoken when the owner has not typed one.
const Phrase = "This is a short sample of my voice."

// MaxTextRunes bounds owner-typed sample text: a preview is a sample, not a
// reply, so every call keeps a small, bounded cost.
const MaxTextRunes = 200

// MaxPCMBytes bounds one synthesized preview. At the TTS service's 24 kHz mono
// PCM16 that is over 80 seconds; the fixed phrase is a few seconds.
const MaxPCMBytes = 4 << 20

// Errors returned by Preview.
var (
	ErrTooLong     = errors.New("voice sample: synthesized audio is over the preview limit")
	ErrEmptyAudio  = errors.New("voice sample: the speech service returned no audio")
	ErrFormat      = errors.New("voice sample: the speech service returned an unsupported audio format")
	ErrTextTooLong = fmt.Errorf("voice sample: the text is longer than %d characters", MaxTextRunes)
)

// Text normalizes owner-typed sample text: surrounding space is trimmed, an
// empty value falls back to Phrase, and anything over MaxTextRunes is refused
// rather than cut mid-word.
func Text(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Phrase, nil
	}
	if utf8.RuneCountInString(text) > MaxTextRunes {
		return "", ErrTextTooLong
	}
	return text, nil
}

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

// Preview synthesizes text (Phrase when empty) in voiceRef at rate and
// returns a complete PCM16 WAV for the browser.
func (s *Sampler) Preview(ctx context.Context, voiceRef, text string, rate float64) ([]byte, error) {
	pcm, format, err := s.Synthesize(ctx, voiceRef, text, rate)
	if err != nil {
		return nil, err
	}
	return audio.EncodeWAV(pcm, format), nil
}

// Synthesize returns text (Phrase when empty) in voiceRef at rate as mono
// PCM16 and its format. voiceRef is a built-in name or a path inside the TTS
// voices mount; resolving it is the caller's job. The active voice setting is
// not read or written here.
func (s *Sampler) Synthesize(ctx context.Context, voiceRef, text string, rate float64) ([]byte, audio.Format, error) {
	if s == nil || s.Client == nil {
		return nil, audio.Format{}, errors.New("voice sample: speech service is not configured")
	}
	text, err := Text(text)
	if err != nil {
		return nil, audio.Format{}, err
	}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	stream, err := s.Client.Synthesize(ctx, text, wyoming.Voice{Name: voiceRef}, wyoming.WithRate(rate))
	if err != nil {
		return nil, audio.Format{}, fmt.Errorf("voice sample: %w", err)
	}
	defer stream.Close()

	format := audio.Format{Rate: stream.Format.Rate, Width: stream.Format.Width, Channels: stream.Format.Channels}
	if format.Rate < 8000 || format.Rate > 96000 || format.Width != 2 || format.Channels != 1 {
		return nil, audio.Format{}, fmt.Errorf("%w: %d Hz, width %d, channels %d", ErrFormat, format.Rate, format.Width, format.Channels)
	}

	pcm := make([]byte, 0, 64<<10)
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, audio.Format{}, fmt.Errorf("voice sample: %w", err)
		}
		if len(chunk) == 0 {
			continue
		}
		if len(pcm)+len(chunk) > MaxPCMBytes {
			return nil, audio.Format{}, ErrTooLong
		}
		pcm = append(pcm, chunk...)
	}
	if len(pcm) == 0 {
		return nil, audio.Format{}, ErrEmptyAudio
	}
	return pcm, format, nil
}
