package config_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

func TestVoiceDefaultsAreReadyForTheDevBox(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.VoiceEnabled() {
		t.Fatal("voice input is enabled without a wakeword address")
	}
	if cfg.Capture.Binary != "parec" || cfg.Capture.Rate != 16000 || cfg.Capture.Width != 2 || cfg.Capture.Channels != 1 {
		t.Fatalf("capture defaults = %+v", cfg.Capture)
	}
	if cfg.STT.Model != "parakeet-tdt-0.6b-v2" {
		t.Errorf("stt model = %q", cfg.STT.Model)
	}
	if !cfg.STTEndpointVAD() {
		t.Error("endpoint VAD is disabled by default")
	}
	if cfg.STT.Endpoint.VADThreshold != 0.45 {
		t.Errorf("endpoint vad_threshold = %v, want 0.45", cfg.STT.Endpoint.VADThreshold)
	}
	if cfg.STT.Endpoint.Silence.Duration() != 750*time.Millisecond {
		t.Errorf("endpoint silence = %s, want 750ms", cfg.STT.Endpoint.Silence.Duration())
	}
	if cfg.STT.Endpoint.ThresholdDBFS != -38 {
		t.Errorf("threshold = %v dBFS", cfg.STT.Endpoint.ThresholdDBFS)
	}
	if cfg.STT.Endpoint.MaxUtterance.Duration() != 20*time.Second {
		t.Errorf("max utterance = %s, want 20s", cfg.STT.Endpoint.MaxUtterance.Duration())
	}
	if !cfg.Wakeword.Chime.Enabled {
		t.Error("wake chime is disabled by default")
	}
}

func TestLoadReadsVoiceSettings(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
capture:
  source: "alsa_input.usb-webcam.mono-fallback"
  frame_ms: 20
wakeword:
  addr: "wakeword:10400"
  model: "hey_jarvis"
  timeout: 7s
stt:
  addr: "stt:10300"
  model: "parakeet-tdt-0.6b-v3"
  language: "en"
  timeout: 12s
  endpoint:
    vad: false
    vad_threshold: 0.55
    silence: 350ms
    threshold_dbfs: -42
    max_utterance: 8s
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.VoiceEnabled() {
		t.Fatal("voice input should be enabled")
	}
	if cfg.Capture.Source != "alsa_input.usb-webcam.mono-fallback" || cfg.Capture.FrameMS != 20 {
		t.Errorf("capture = %+v", cfg.Capture)
	}
	if cfg.Wakeword.Model != "hey_jarvis" || cfg.Wakeword.Timeout.Duration() != 7*time.Second {
		t.Errorf("wakeword = %+v", cfg.Wakeword)
	}
	if cfg.STT.Model != "parakeet-tdt-0.6b-v3" || cfg.STT.Timeout.Duration() != 12*time.Second {
		t.Errorf("stt = %+v", cfg.STT)
	}
	if cfg.STTEndpointVAD() {
		t.Error("endpoint VAD should be false when disabled in yaml")
	}
	if cfg.STT.Endpoint.VADThreshold != 0.55 {
		t.Errorf("vad_threshold = %v, want 0.55", cfg.STT.Endpoint.VADThreshold)
	}
	if cfg.STT.Endpoint.Silence.Duration() != 350*time.Millisecond {
		t.Errorf("silence = %s", cfg.STT.Endpoint.Silence.Duration())
	}
	if cfg.STT.Endpoint.ThresholdDBFS != -42 {
		t.Errorf("threshold = %v", cfg.STT.Endpoint.ThresholdDBFS)
	}
	if cfg.STT.Endpoint.MaxUtterance.Duration() != 8*time.Second {
		t.Errorf("max utterance = %s", cfg.STT.Endpoint.MaxUtterance.Duration())
	}
}

func TestVoiceConfigRejectsIncompleteSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wakeword without stt", body: `
wakeword: {addr: "wakeword:10400"}
`},
		{name: "unknown stt model", body: `
wakeword: {addr: "wakeword:10400"}
stt: {addr: "stt:10300", model: "small-streaming", language: en}
`},
		{name: "vad threshold out of range", body: `
wakeword: {addr: "wakeword:10400"}
stt:
  addr: "stt:10300"
  endpoint: {vad: true, vad_threshold: 1.2}
`},
		{name: "positive threshold", body: `
wakeword: {addr: "wakeword:10400"}
stt:
  addr: "stt:10300"
  endpoint: {threshold_dbfs: 3}
`},
		{name: "cap shorter than silence", body: `
wakeword: {addr: "wakeword:10400"}
stt:
  addr: "stt:10300"
  endpoint: {silence: 2s, max_utterance: 1s}
`},
		{name: "unsupported sample width", body: `
capture: {width: 1}
wakeword: {addr: "wakeword:10400"}
stt: {addr: "stt:10300"}
`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, test.body)); err == nil {
				t.Fatal("Load accepted invalid voice config")
			}
		})
	}
}

func TestVoiceEnvironmentOverrides(t *testing.T) {
	t.Setenv("ASSISTANT_PULSE_SOURCE", "webcam")
	t.Setenv("ASSISTANT_WAKEWORD_ADDR", "wakeword:10400")
	t.Setenv("ASSISTANT_WAKEWORD_MODEL", "hey_jarvis")
	t.Setenv("ASSISTANT_STT_ADDR", "stt:10300")
	t.Setenv("ASSISTANT_STT_MODEL", "parakeet-ctc-0.6b")
	t.Setenv("ASSISTANT_STT_LANGUAGE", "de")
	t.Setenv("ASSISTANT_STT_VAD", "false")
	t.Setenv("ASSISTANT_STT_VAD_THRESHOLD", "0.6")

	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Capture.Source != "webcam" {
		t.Errorf("source = %q", cfg.Capture.Source)
	}
	if cfg.Wakeword.Addr != "wakeword:10400" || cfg.Wakeword.Model != "hey_jarvis" {
		t.Errorf("wakeword = %+v", cfg.Wakeword)
	}
	if cfg.STT.Addr != "stt:10300" || cfg.STT.Model != "parakeet-ctc-0.6b" || cfg.STT.Language != "de" {
		t.Errorf("stt = %+v", cfg.STT)
	}
	if cfg.STTEndpointVAD() {
		t.Errorf("vad = true, want false")
	}
	if cfg.STT.Endpoint.VADThreshold != 0.6 {
		t.Errorf("vad_threshold = %v, want 0.6", cfg.STT.Endpoint.VADThreshold)
	}
}
