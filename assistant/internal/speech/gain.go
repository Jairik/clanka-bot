package speech

import (
	"context"
	"io"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// gainKey carries one software PCM gain percent for a speech or Filler write.
// The value is applied to PCM16 samples as they are written, so the media/sink
// volume is never changed and there is nothing to restore if playback fails.
type gainKey struct{}

// WithGain attaches a software gain percent (0-100) to ctx. Values outside the
// range are clamped; 100 is the fast no-op path.
func WithGain(ctx context.Context, percent int) context.Context {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if percent >= 100 {
		// No gain needed; keep the context unchanged so a nested caller cannot
		// accidentally clear an outer value.
		return ctx
	}
	return context.WithValue(ctx, gainKey{}, percent)
}

// GainFrom returns the attached gain percent, or 100 when none is attached.
func GainFrom(ctx context.Context) int {
	if ctx == nil {
		return 100
	}
	if percent, ok := ctx.Value(gainKey{}).(int); ok {
		return percent
	}
	return 100
}

// newGainWriter wraps one sink writer with a software PCM16 gain. The wrapper
// buffers a trailing odd byte so a chunk boundary that splits a sample cannot
// corrupt the next one.
func newGainWriter(inner io.WriteCloser, format audio.Format, percent int) io.WriteCloser {
	if inner == nil || percent >= 100 || percent < 0 {
		return inner
	}
	if format.Width != 2 {
		// Only PCM16 has a canonical sample width here. Passing through is
		// safer than reinterpreting u8 or s32 bytes as samples.
		return inner
	}
	return &gainWriter{inner: inner, gain: float64(percent) / 100.0}
}

// gainWriter scales signed 16-bit little-endian samples in each Write.
type gainWriter struct {
	inner io.WriteCloser
	gain  float64
	carry byte
	has   bool
	err   error
}

func (w *gainWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.gain >= 1 {
		return w.inner.Write(p)
	}
	consumed := len(p)
	if w.has {
		// The previous chunk ended mid-sample; complete it with this byte.
		p = append([]byte{w.carry}, p...)
		w.has = false
	}
	even := len(p) &^ 1
	if even < len(p) {
		w.carry = p[len(p)-1]
		w.has = true
	}
	if even > 0 {
		scaled := make([]byte, even)
		for i := 0; i < even; i += 2 {
			raw := int16(uint16(p[i]) | uint16(p[i+1])<<8)
			value := float64(raw) * w.gain
			if value > 32767 {
				value = 32767
			}
			if value < -32768 {
				value = -32768
			}
			out := uint16(int16(value))
			scaled[i] = byte(out)
			scaled[i+1] = byte(out >> 8)
		}
		if _, err := w.inner.Write(scaled); err != nil {
			w.err = err
			return 0, err
		}
	}
	// Report every input byte as accepted; the trailing byte is held inside
	// the wrapper and flushed on Close.
	return consumed, nil
}

func (w *gainWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.has {
		if _, err := w.inner.Write([]byte{w.carry}); err != nil {
			w.err = err
			return err
		}
		w.has = false
	}
	return w.inner.Close()
}
