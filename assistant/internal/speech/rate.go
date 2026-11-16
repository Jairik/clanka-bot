package speech

import "context"

// rateKey carries one per-utterance speech-rate multiplier for a Say call. It
// is applied to the TTS request alone, so the persistent voice.rate setting is
// never written and the next synthesis inherits it unchanged. A Filler (cached
// PCM) or a Cue plays outside TTS and never reads this value.
type rateKey struct{}

// WithRate attaches a per-utterance speech-rate multiplier to ctx. Values <= 0
// mean "no override", so the context is left unchanged; a nested caller cannot
// accidentally clear an outer value. The TTS transport clamps to its own range.
func WithRate(ctx context.Context, rate float64) context.Context {
	if rate <= 0 {
		return ctx
	}
	return context.WithValue(ctx, rateKey{}, rate)
}

// RateFrom returns the attached speech-rate multiplier, or 0 when none is
// attached. The caller treats 0 as "use the live voice.rate setting".
func RateFrom(ctx context.Context) float64 {
	if ctx == nil {
		return 0
	}
	rate, _ := ctx.Value(rateKey{}).(float64)
	return rate
}
