// Package voicesample synthesizes one short preview for the Dashboard: the
// Voice page's sample, which may be the owner's own text, and the wording the
// Settings page previews. It asks the same Wyoming TTS service a reply uses,
// but it never touches the speaker, the active voice setting or the audio sink:
// the PCM is collected in memory and returned either as PCM, for the Pi
// speaker, or as a canonical WAV, for the browser.
package voicesample

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// Phrase is the sample spoken when the owner has not typed one, and what
// Preview synthesizes. It is deliberately short and fixed: a preview is a
// sample, not a reply, and every such call has the same bounded cost.
const Phrase = "This is a short sample of my voice."

// MaxTextRunes bounds owner-typed text on the Voice page: a sample is not a
// reply, so every call keeps a small, bounded cost.
const MaxTextRunes = 200

// MaxPreviewRunes bounds the wording the Settings page previews. It is a rune
// count, so a multi-byte script is not cut short.
const MaxPreviewRunes = 240

// MaxPCMBytes bounds one synthesized preview. At the TTS service's 24 kHz mono
// PCM16 that is over 80 seconds; the fixed phrase is a few seconds and the
// longest accepted text is MaxPreviewRunes characters.
const MaxPCMBytes = 4 << 20

// Errors returned by Text, Preview, PreviewText and Synthesize.
var (
	ErrTooLong     = errors.New("voice sample: synthesized audio is over the preview limit")
	ErrEmptyAudio  = errors.New("voice sample: the speech service returned no audio")
	ErrFormat      = errors.New("voice sample: the speech service returned an unsupported audio format")
	ErrTextEmpty   = errors.New("voice sample: the preview text is empty")
	ErrTextTooLong = errors.New("voice sample: the preview text is over the rune limit")
)

// ErrNotConfigured is returned when the sampler has no TTS client.
var ErrNotConfigured = errors.New("voice sample: speech service is not configured")

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
// Previews are serialised against each other: at most one preview synthesis
// runs at a time. Replies and Filler do not go through the Sampler and are not
// covered by that gate.
type Sampler struct {
	Client  *wyoming.Client
	Timeout time.Duration

	gateOnce sync.Once
	gate     chan struct{}
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
	pcm, format, err := s.Synthesize(ctx, voiceRef, Phrase, rate)
	if err != nil {
		return nil, err
	}
	return audio.EncodeWAV(pcm, format), nil
}

// PreviewText synthesizes text in voiceRef at rate and returns a complete
// PCM16 WAV. Leading and trailing space is trimmed and an empty or over-long
// draft is refused before any TTS call, so a rejected draft never consumes the
// shared speech service.
func (s *Sampler) PreviewText(ctx context.Context, voiceRef string, rate float64, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrTextEmpty
	}
	if utf8.RuneCountInString(text) > MaxPreviewRunes {
		return nil, ErrTextTooLong
	}
	pcm, format, err := s.synthesizePCM(ctx, voiceRef, rate, text)
	if err != nil {
		return nil, err
	}
	return audio.EncodeWAV(pcm, format), nil
}

// Synthesize returns text (Phrase when empty) in voiceRef at rate as mono
// PCM16 and its format. It is the one path that can also feed the Pi speaker,
// so the browser and the Pi play the same audio. voiceRef is a built-in name or
// a path inside the TTS voices mount; resolving it is the caller's job. The
// active voice setting is not read or written here.
func (s *Sampler) Synthesize(ctx context.Context, voiceRef, text string, rate float64) ([]byte, audio.Format, error) {
	text, err := Text(text)
	if err != nil {
		return nil, audio.Format{}, err
	}
	return s.synthesizePCM(ctx, voiceRef, rate, text)
}

// synthesizePCM is the one Wyoming path every preview uses: same call, format
// validation and PCM bound. The size-one gate serialises previews against each
// other, so only one preview request is in flight at a time. The caller has
// already validated text.
func (s *Sampler) synthesizePCM(ctx context.Context, voiceRef string, rate float64, text string) ([]byte, audio.Format, error) {
	if s == nil || s.Client == nil {
		return nil, audio.Format{}, ErrNotConfigured
	}
	gate := s.previewGate()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, audio.Format{}, ctx.Err()
	}
	defer func() { <-gate }()

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

// previewGate lazily builds the size-one synthesis gate, so a Sampler created
// as a struct literal (as tests do) still serialises previews.
func (s *Sampler) previewGate() chan struct{} {
	s.gateOnce.Do(func() {
		s.gate = make(chan struct{}, 1)
	})
	return s.gate
}
