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
	if cfg.STT.Model != "small-streaming" {
		t.Errorf("stt model = %q", cfg.STT.Model)
	}
	if cfg.STT.Endpoint.Silence.Duration() != 500*time.Millisecond {
		t.Errorf("endpoint silence = %s, want 500ms", cfg.STT.Endpoint.Silence.Duration())
	}
	if cfg.STT.Endpoint.ThresholdDBFS != -38 {
		t.Errorf("threshold = %v dBFS", cfg.STT.Endpoint.ThresholdDBFS)
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
  model: "tiny-streaming"
  language: "en"
  timeout: 12s
  endpoint:
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
	if cfg.STT.Model != "tiny-streaming" || cfg.STT.Timeout.Duration() != 12*time.Second {
		t.Errorf("stt = %+v", cfg.STT)
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
stt: {addr: "stt:10300", model: "large-streaming", language: en}
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
	t.Setenv("ASSISTANT_STT_MODEL", "tiny-streaming")
	t.Setenv("ASSISTANT_STT_LANGUAGE", "de")

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
	if cfg.STT.Addr != "stt:10300" || cfg.STT.Model != "tiny-streaming" || cfg.STT.Language != "de" {
		t.Errorf("stt = %+v", cfg.STT)
	}
}
