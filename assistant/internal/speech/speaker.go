// Package speech turns reply text into audio: it asks the Wyoming TTS service
// for PCM and streams each chunk to the speaker as it arrives.
package speech

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// Sink plays a PCM stream. Production uses audio.PulseSink; tests use a fake.
// Reply playback, Fillers and alerts should share a SerialSink so two sounds
// never play over each other.
type Sink interface {
	Open(ctx context.Context, format audio.Format) (io.WriteCloser, error)
}

// Result reports what was played, for the request log.
type Result struct {
	// FirstAudio is when the first PCM chunk was accepted by the sink, not when
	// the DAC emitted it. pacat and the host audio stack add buffer latency
	// that this interface cannot observe.
	FirstAudio time.Duration
	AudioBytes int
	Chunks     int
	AudioTime  time.Duration
}

// Speaker speaks one reply at a time.
type Speaker struct {
	client *wyoming.Client
	sink   Sink
	voice  wyoming.Voice
	logger *slog.Logger
}

// New builds a Speaker. A nil logger uses the default; an empty voice uses the
// service's default voice.
func New(client *wyoming.Client, sink Sink, voice wyoming.Voice, logger *slog.Logger) *Speaker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Speaker{client: client, sink: sink, voice: voice, logger: logger}
}

// SayText speaks text and discards the timing details, satisfying the api seam.
func (s *Speaker) SayText(ctx context.Context, text string) error {
	_, err := s.Say(ctx, text)
	return err
}

// Say synthesises text and plays it. Playback starts on the first chunk, not
// after the whole reply is synthesised. Cancelling ctx stops the audio.
func (s *Speaker) Say(ctx context.Context, text string) (Result, error) {
	started := time.Now()
	stream, err := s.client.Synthesize(ctx, text, s.voice)
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()

	format := audio.Format{Rate: stream.Format.Rate, Width: stream.Format.Width, Channels: stream.Format.Channels}
	writer, err := s.sink.Open(ctx, format)
	if err != nil {
		return Result{}, err
	}

	// The request's one sound slot is claimed immediately before the first
	// PCM submission. A Filler racing this hook loses and stays silent; a
	// Filler that already claimed the slot makes this first Write wait in
	// SerialSink, so reply audio never overlaps it.
	hook := FirstAudioHook(ctx)
	first := true
	result := Result{}
	var playErr error
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			playErr = err
			break
		}
		if first && len(chunk) > 0 && hook != nil {
			hook()
		}
		n, err := writer.Write(chunk)
		if err != nil {
			playErr = err
			break
		}
		if first && n > 0 {
			first = false
			result.FirstAudio = time.Since(started)
			// The first PCM sample of an actual reply is Response latency's
			// end point, measured after the sink accepted it. A Filler is
			// cached PCM played elsewhere and never reaches here. The mark is
			// deliberately after Write: measuring before submission understated
			// Response latency by the write and buffer time.
			latency.SetStage(ctx, latency.StageTTSFirstAudio, result.FirstAudio)
			latency.MarkReplyAudio(ctx)
		}
		result.Chunks++
		result.AudioBytes += n
	}
	if err := writer.Close(); playErr == nil {
		playErr = err
	}

	if bytesPerSecond := format.Rate * format.Width * format.Channels; bytesPerSecond > 0 {
		result.AudioTime = time.Duration(result.AudioBytes) * time.Second / time.Duration(bytesPerSecond)
	}
	attrs := []any{
		"text_chars", len(text),
		"chunks", result.Chunks,
		"audio_ms", result.AudioTime.Milliseconds(),
		"first_audio_ms", result.FirstAudio.Milliseconds(),
		"duration_ms", time.Since(started).Milliseconds(),
	}
	if id := latency.RequestID(ctx); id != "" {
		attrs = append([]any{"request_id", id}, attrs...)
	}
	if playErr != nil {
		// A Barge-in cancels the request context; that is expected, not a fault.
		if ctx.Err() != nil {
			s.logger.Info("speech interrupted", attrs...)
		} else {
			s.logger.Error("speech", append(attrs, "error", playErr.Error())...)
		}
		return result, fmt.Errorf("speech: %w", playErr)
	}
	s.logger.Info("speech", attrs...)
	return result, nil
}

// firstAudioKey carries the per-request hook run immediately before the first
// reply PCM is submitted. It is per request, so one shared Speaker cannot mix
// a timer alert or a later turn into this request's sound slot.
type firstAudioKey struct{}

// WithFirstAudio attaches the sound-slot claim hook to ctx.
func WithFirstAudio(ctx context.Context, hook func()) context.Context {
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, firstAudioKey{}, hook)
}

// FirstAudioHook returns the hook attached by WithFirstAudio, or nil.
func FirstAudioHook(ctx context.Context) func() {
	hook, _ := ctx.Value(firstAudioKey{}).(func())
	return hook
}
