// Package voicesample synthesizes one short preview for the Dashboard's
// browser player: normally the fixed phrase, or the wording the user is
// editing when the Settings page previews a text setting. It asks the same
// Wyoming TTS service a reply uses, but it never touches the speaker, the
// active voice setting or the audio sink: the PCM is collected in memory and
// returned as a canonical WAV.
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

// Phrase is deliberately short and fixed: a preview is a sample, not a reply,
// and every call has the same bounded cost. It is what Preview synthesizes;
// the Settings wording preview passes the user's own text instead, bounded by
// MaxPreviewRunes.
const Phrase = "This is a short sample of my voice."

// MaxPreviewRunes bounds the user wording a wording preview may synthesize.
// It is a rune count, so a multi-byte script is not cut short.
const MaxPreviewRunes = 240

// MaxPCMBytes bounds one synthesized preview. At the TTS service's 24 kHz mono
// PCM16 that is over 80 seconds; the fixed phrase is a few seconds and a
// wording preview is at most MaxPreviewRunes characters.
const MaxPCMBytes = 4 << 20

// Errors returned by Preview and PreviewText.
var (
	ErrTooLong     = errors.New("voice sample: synthesized audio is over the preview limit")
	ErrEmptyAudio  = errors.New("voice sample: the speech service returned no audio")
	ErrFormat      = errors.New("voice sample: the speech service returned an unsupported audio format")
	ErrTextEmpty   = errors.New("voice sample: the preview text is empty")
	ErrTextTooLong = errors.New("voice sample: the preview text is over the rune limit")
)

// ErrNotConfigured is returned when the sampler has no TTS client.
var ErrNotConfigured = errors.New("voice sample: speech service is not configured")

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
	return s.synthesize(ctx, voiceRef, rate, Phrase)
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
	return s.synthesize(ctx, voiceRef, rate, text)
}

// synthesize is the one Wyoming path both previews use: same format
// validation, same PCM bound and same WAV encoding. The size-one gate
// serialises previews against each other, so only one preview request is in
// flight at a time.
func (s *Sampler) synthesize(ctx context.Context, voiceRef string, rate float64, text string) ([]byte, error) {
	if s == nil || s.Client == nil {
		return nil, ErrNotConfigured
	}
	gate := s.previewGate()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-gate }()

	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	stream, err := s.Client.Synthesize(ctx, text, wyoming.Voice{Name: voiceRef}, wyoming.WithRate(rate))
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

// previewGate lazily builds the size-one synthesis gate, so a Sampler created
// as a struct literal (as tests do) still serialises previews.
func (s *Sampler) previewGate() chan struct{} {
	s.gateOnce.Do(func() {
		s.gate = make(chan struct{}, 1)
	})
	return s.gate
}
