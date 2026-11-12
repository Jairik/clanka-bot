package voicelib

import (
	"context"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// TTSProbe adapts the Wyoming client to the library's Probe seam. The timeout
// bounds one revision request and one smoke test; a slow CPU smoke test is
// allowed its own budget by the caller.
type TTSProbe struct {
	Client  *wyoming.Client
	Timeout time.Duration
}

// NewTTSProbe builds the production probe. A nil client returns nil so Rescan
// stays header-only.
func NewTTSProbe(client *wyoming.Client, timeout time.Duration) *TTSProbe {
	if client == nil {
		return nil
	}
	return &TTSProbe{Client: client, Timeout: timeout}
}

// Revision reports the TTS model revision without generating audio.
func (p *TTSProbe) Revision(ctx context.Context) (string, error) {
	if p == nil || p.Client == nil {
		return "", nil
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	info, err := p.Client.VoiceInfo(ctx)
	if err != nil {
		return "", err
	}
	return info.Revision, nil
}

// Validate runs the real TTS model smoke test for one state file.
func (p *TTSProbe) Validate(ctx context.Context, remotePath string) (string, error) {
	if p == nil || p.Client == nil {
		return "", nil
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	return p.Client.ValidateVoice(ctx, remotePath)
}

// ValidateEphemeral proves a staging file generates audio without adding the
// temporary path to the TTS process cache. Uploads call this before the final
// rename, so validation never leaves a dead path cached forever.
func (p *TTSProbe) ValidateEphemeral(ctx context.Context, remotePath string) (string, error) {
	if p == nil || p.Client == nil {
		return "", nil
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	return p.Client.ValidateVoiceEphemeral(ctx, remotePath)
}

// Forget releases one cached state path before a replacement is validated.
func (p *TTSProbe) Forget(ctx context.Context, remotePath string) error {
	if p == nil || p.Client == nil {
		return nil
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	return p.Client.ForgetVoice(ctx, remotePath)
}

// withTimeout adds a bound only when the caller has not set an earlier one.
func (p *TTSProbe) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.Timeout)
}
