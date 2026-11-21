package config_test

import (
	"reflect"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

// voiceBody is the minimum config that passes voice validation, so a wakeword
// test exercises only the part it means to.
func voiceBody(wakeword string) string {
	return wakeword + `
stt: {addr: "stt:10300"}
`
}

func TestWakeModelsReadsTheListAndLegacyForm(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "list",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker", "okay_nabu"]
`),
			want: []string{"clanker", "okay_nabu"},
		},
		{
			name: "legacy single model",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  model: "hey_jarvis"
`),
			want: []string{"hey_jarvis"},
		},
		{
			name: "list wins over the legacy key",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  model: "hey_jarvis"
  models: ["clanker", "okay_nabu"]
`),
			want: []string{"clanker", "okay_nabu"},
		},
		{
			name: "defaults to clanker",
			body: voiceBody(`wakeword: {addr: "wakeword:10400"}
`),
			want: []string{"clanker"},
		},
		{
			name: "trims blanks, whitespace and duplicates",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: [" clanker ", "", "clanker", "okay_nabu"]
`),
			want: []string{"clanker", "okay_nabu"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, test.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Wakeword.WakeModels(); !reflect.DeepEqual(got, test.want) {
				t.Errorf("models = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWakeThresholdsPerModel(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker", "okay_nabu"]
  threshold: 0.5
  thresholds: {clanker: 0.7}
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Wakeword.ThresholdFor("clanker"); got != 0.7 {
		t.Errorf("clanker threshold = %v, want 0.7", got)
	}
	if got := cfg.Wakeword.ThresholdFor("okay_nabu"); got != 0.5 {
		t.Errorf("okay_nabu threshold = %v, want the 0.5 default", got)
	}
	want := map[string]float64{"clanker": 0.7, "okay_nabu": 0.5}
	if got := cfg.Wakeword.ModelThresholds(); !reflect.DeepEqual(got, want) {
		t.Errorf("thresholds = %v, want %v", got, want)
	}
}

func TestWakewordEnvironmentOverrides(t *testing.T) {
	t.Setenv("ASSISTANT_WAKEWORD_MODELS", "clanker,okay_nabu")
	t.Setenv("ASSISTANT_WAKEWORD_THRESHOLD", "0.4")
	t.Setenv("ASSISTANT_WAKEWORD_THRESHOLDS", "okay_nabu:0.6")

	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  model: "hey_jarvis"
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Wakeword.WakeModels(), []string{"clanker", "okay_nabu"}; !reflect.DeepEqual(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
	want := map[string]float64{"clanker": 0.4, "okay_nabu": 0.6}
	if got := cfg.Wakeword.ModelThresholds(); !reflect.DeepEqual(got, want) {
		t.Errorf("thresholds = %v, want %v", got, want)
	}
}

func TestLegacyWakewordModelEnvironmentWinsOverTheList(t *testing.T) {
	t.Setenv("ASSISTANT_WAKEWORD_MODELS", "clanker,okay_nabu")
	t.Setenv("ASSISTANT_WAKEWORD_MODEL", "hey_jarvis")

	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker", "okay_nabu"]
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Wakeword.WakeModels(), []string{"hey_jarvis"}; !reflect.DeepEqual(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
}

func TestWakewordRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "no model",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: []
  model: ""
`),
		},
		{
			name: "whitespace in a name",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["okay nabu"]
`),
		},
		{
			name: "threshold below zero",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker"]
  threshold: -0.1
`),
		},
		{
			name: "threshold above one",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker"]
  thresholds: {clanker: 1.2}
`),
		},
		{
			name: "threshold for an unlisted model",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker"]
  thresholds: {okay_nabu: 0.6}
`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, test.body)); err == nil {
				t.Fatal("Load accepted an invalid wakeword config")
			}
		})
	}
}

func TestWakeThresholdEnvironmentRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"clanker", "clanker:0.6,clanker:0.7", "clanker:loud", ":0.5"} {
		t.Setenv("ASSISTANT_WAKEWORD_THRESHOLDS", value)
		if _, err := config.Load(writeConfig(t, voiceBody(`wakeword: {addr: "wakeword:10400"}
`))); err == nil {
			t.Errorf("Load accepted ASSISTANT_WAKEWORD_THRESHOLDS=%q", value)
		}
	}
}

func TestStopIsOffByDefault(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword: {addr: "wakeword:10400"}
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Wakeword.StopModel(); got != "" {
		t.Fatalf("stop model = %q, want none by default", got)
	}
	want := []string{"clanker"}
	if got := cfg.Wakeword.DetectModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("detect models = %v, want %v", got, want)
	}
	if got := cfg.Wakeword.ReplyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("reply models = %v, want %v", got, want)
	}
}

// TestStopJoinsOnlyTheReplyStream pins the #503 wiring: the stop model is
// requested only by the stream opened while a reply plays, never while idle.
func TestStopJoinsOnlyTheReplyStream(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker", "yo_dipshit"]
  thresholds: {stop: 0.9}
  stop: " stop "
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Wakeword.StopModel(); got != "stop" {
		t.Errorf("stop model = %q, want stop", got)
	}
	if got, want := cfg.Wakeword.DetectModels(), []string{"clanker", "yo_dipshit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("detect models = %v, want %v (never stop)", got, want)
	}
	if got, want := cfg.Wakeword.ReplyModels(), []string{"clanker", "yo_dipshit", "stop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reply models = %v, want %v", got, want)
	}
	if got := cfg.Wakeword.ThresholdFor("stop"); got != 0.9 {
		t.Errorf("stop threshold = %v, want 0.9", got)
	}
	want := map[string]float64{"clanker": 0.5, "yo_dipshit": 0.5, "stop": 0.9}
	if got := cfg.Wakeword.ModelThresholds(); !reflect.DeepEqual(got, want) {
		t.Errorf("thresholds = %v, want %v", got, want)
	}
}

func TestStopFallsBackToTheWakeThreshold(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  threshold: 0.6
  stop: "stop"
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Wakeword.ThresholdFor("stop"); got != 0.6 {
		t.Errorf("stop threshold = %v, want the wake default 0.6", got)
	}
}

func TestWakewordStopEnvironmentOverride(t *testing.T) {
	t.Setenv("ASSISTANT_WAKEWORD_STOP", "stop")

	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword: {addr: "wakeword:10400"}
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Wakeword.StopModel(); got != "stop" {
		t.Errorf("stop model = %q, want stop", got)
	}
	if got, want := cfg.Wakeword.ReplyModels(), []string{"clanker", "stop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reply models = %v, want %v", got, want)
	}
}

func TestWakewordStopRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "stop is also a wake model",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker", "stop"]
  stop: "stop"
`),
		},
		{
			name: "whitespace in the stop name",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  stop: "stop it"
`),
		},
		{
			name: "threshold for an unknown model",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  stop: "stop"
  thresholds: {volume_sideways: 0.7}
`),
		},
		{
			name: "threshold for stop when stop is not configured",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  thresholds: {stop: 0.7}
`),
		},
		{
			name: "stop threshold above one",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  stop: "stop"
  thresholds: {stop: 1.1}
`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, test.body)); err == nil {
				t.Fatal("Load accepted an invalid wakeword stop config")
			}
		})
	}
}
