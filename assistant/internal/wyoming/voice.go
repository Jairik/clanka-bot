package wyoming

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// VoiceLibrary is the TTS service's voice-library report: the model revision every
// custom voice state must match, the built-in voices warm in the image, and the
// folder the service sees custom states in.
type VoiceLibrary struct {
	Revision string
	Voices   []string
	Dir      string
}

// VoiceLibrary asks the service for its revision and built-in voice list without
// generating audio. It is the startup probe and the stale check's source of
// truth.
func (c *Client) VoiceInfo(ctx context.Context) (VoiceLibrary, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return VoiceLibrary{}, err
	}
	defer conn.Close()
	if err := writeEvent(conn, c.timeout, "voice-info", nil, nil); err != nil {
		return VoiceLibrary{}, err
	}
	ev, err := readEvent(conn, c.readTimeout)
	if err != nil {
		return VoiceLibrary{}, fmt.Errorf("wyoming: voice info: %w", err)
	}
	if ev.Type == "error" {
		return VoiceLibrary{}, errorEvent(ev)
	}
	if ev.Type != "voice-info-result" {
		return VoiceLibrary{}, fmt.Errorf("wyoming: voice info got %q, want voice-info-result", ev.Type)
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		return VoiceLibrary{}, fmt.Errorf("wyoming: encode voice info: %w", err)
	}
	var payload struct {
		Revision string   `json:"revision"`
		Voices   []string `json:"voices"`
		Dir      string   `json:"dir"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return VoiceLibrary{}, fmt.Errorf("wyoming: decode voice info: %w", err)
	}
	return VoiceLibrary{
		Revision: strings.TrimSpace(payload.Revision),
		Voices:   payload.Voices,
		Dir:      payload.Dir,
	}, nil
}

// ValidateVoice asks TTS to load one custom state (a path inside its read-only
// voices mount) and run a real short synthesis. It returns the revision the
// file passed against. The state stays cached for later synthesis; a rescan
// uses this form.
func (c *Client) ValidateVoice(ctx context.Context, path string) (string, error) {
	return c.validateVoice(ctx, path, true)
}

// ValidateVoiceEphemeral runs the same smoke test without adding the state to
// the TTS process cache. Uploads validate a hidden staging path and then rename
// it; caching that temp path would keep a dead entry for the process lifetime.
func (c *Client) ValidateVoiceEphemeral(ctx context.Context, path string) (string, error) {
	return c.validateVoice(ctx, path, false)
}

// validateVoice sends the smoke-test request with the cache choice.
func (c *Client) validateVoice(ctx context.Context, path string, cache bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("wyoming: voice path is required")
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	data := map[string]any{"path": path}
	if !cache {
		// Older services ignore the extra field and default to caching.
		data["cache"] = false
	}
	if err := writeEvent(conn, c.timeout, "voice-validate", data, nil); err != nil {
		return "", err
	}
	ev, err := readEvent(conn, c.readTimeout)
	if err != nil {
		return "", fmt.Errorf("wyoming: voice validate: %w", err)
	}
	if ev.Type == "error" {
		return "", errorEvent(ev)
	}
	if ev.Type != "voice-validated" {
		return "", fmt.Errorf("wyoming: voice validate got %q, want voice-validated", ev.Type)
	}
	ok, _ := ev.Data["ok"].(bool)
	if text, _ := ev.Data["error"].(string); !ok || strings.TrimSpace(text) != "" {
		if strings.TrimSpace(text) == "" {
			text = "voice state did not pass the model smoke test"
		}
		return "", &ServerError{Text: text, Code: "voice-invalid"}
	}
	revision, _ := ev.Data["revision"].(string)
	return strings.TrimSpace(revision), nil
}

// ForgetVoice drops one cached voice state, named by a built-in name or a path
// inside the TTS voices mount. Uploads call it before validating a replacement:
// the cache is keyed by path, so without this a re-upload would validate the
// old bytes still resident in the model process.
func (c *Client) ForgetVoice(ctx context.Context, voice string) error {
	if strings.TrimSpace(voice) == "" {
		return errors.New("wyoming: voice is required")
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeEvent(conn, c.timeout, "voice-forget", map[string]any{"voice": voice}, nil); err != nil {
		return err
	}
	ev, err := readEvent(conn, c.readTimeout)
	if err != nil {
		return fmt.Errorf("wyoming: voice forget: %w", err)
	}
	if ev.Type == "error" {
		return errorEvent(ev)
	}
	if ev.Type != "voice-forgotten" {
		return fmt.Errorf("wyoming: voice forget got %q, want voice-forgotten", ev.Type)
	}
	ok, _ := ev.Data["ok"].(bool)
	if text, _ := ev.Data["error"].(string); !ok || strings.TrimSpace(text) != "" {
		if strings.TrimSpace(text) == "" {
			text = "voice cache was not cleared"
		}
		return &ServerError{Text: text, Code: "voice-forget-failed"}
	}
	return nil
}

// WarmVoice loads and caches one voice at TTS startup, so the first reply in
// the active voice does not pay the state-load cost. A built-in name or a path
// inside the tts voices mount is accepted by the service.
func (c *Client) WarmVoice(ctx context.Context, voice string) error {
	if strings.TrimSpace(voice) == "" {
		return nil
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeEvent(conn, c.timeout, "voice-warm", map[string]any{"voice": voice}, nil); err != nil {
		return err
	}
	ev, err := readEvent(conn, c.readTimeout)
	if err != nil {
		return fmt.Errorf("wyoming: voice warm: %w", err)
	}
	if ev.Type == "error" {
		return errorEvent(ev)
	}
	if ev.Type != "voice-warmed" {
		return fmt.Errorf("wyoming: voice warm got %q, want voice-warmed", ev.Type)
	}
	ok, _ := ev.Data["ok"].(bool)
	if text, _ := ev.Data["error"].(string); !ok || strings.TrimSpace(text) != "" {
		if strings.TrimSpace(text) == "" {
			text = "voice did not warm"
		}
		return &ServerError{Text: text, Code: "voice-warm-failed"}
	}
	return nil
}

// Rate is the synthesizer's speech-rate multiplier. It is not part of the
// voice identity; it is sent alongside the voice for every synthesis.
const (
	MinRate = 0.5
	MaxRate = 2.0
)

// SynthesisOption adjusts one synthesize request.
type SynthesisOption func(map[string]any)

// WithRate sends the speech-rate multiplier. The TTS server resamples its
// output, so the PCM stays at the announced rate and playback needs no change.
// Values outside [MinRate, MaxRate] are clamped before they reach the wire.
func WithRate(rate float64) SynthesisOption {
	return func(data map[string]any) {
		if rate < MinRate {
			rate = MinRate
		}
		if rate > MaxRate {
			rate = MaxRate
		}
		data["rate"] = rate
	}
}

// addRate applies the per-request synthesis options.
func addRate(data map[string]any, opts []SynthesisOption) {
	for _, opt := range opts {
		if opt != nil {
			opt(data)
		}
	}
}
