package config_test

import (
	"reflect"
	"testing"
	"time"

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

func TestCommandModelsAreOffByDefault(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword: {addr: "wakeword:10400"}
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Wakeword.Commands.Enabled {
		t.Fatal("commands are enabled by default; they must ship off")
	}
	if got := cfg.Wakeword.CommandModels(); len(got) != 0 {
		t.Fatalf("active command models = %v, want none while disabled", got)
	}
	if got, want := cfg.Wakeword.DetectModels(), []string{"clanker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("detect models = %v, want %v", got, want)
	}
	if !cfg.Wakeword.IsCommandModel("volume_up") || !cfg.Wakeword.IsCommandModel("volume_down") {
		t.Error("the default command list must name volume_up and volume_down")
	}
	if got := cfg.Wakeword.ThresholdFor("volume_up"); got != config.DefaultCommandThreshold {
		t.Errorf("volume_up threshold = %v, want the command default %v", got, config.DefaultCommandThreshold)
	}
	if got, want := cfg.Wakeword.ModelThresholds(), map[string]float64{"clanker": 0.5}; !reflect.DeepEqual(got, want) {
		t.Errorf("thresholds = %v, want only the wake word while commands are off", got)
	}
	if cfg.Wakeword.Commands.Step != 10 || cfg.Wakeword.Commands.Cooldown.Duration() != 1500*time.Millisecond {
		t.Errorf("command defaults = %+v, want step 10 and cooldown 1.5s", cfg.Wakeword.Commands)
	}
}

func TestCommandModelsAreRequestedWhenEnabled(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  models: ["clanker"]
  thresholds: {volume_up: 0.8}
  commands:
    enabled: true
    step: 5
    cooldown: 2s
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Wakeword.CommandModels(), []string{"volume_up", "volume_down"}; !reflect.DeepEqual(got, want) {
		t.Errorf("command models = %v, want %v", got, want)
	}
	if got, want := cfg.Wakeword.DetectModels(), []string{"clanker", "volume_up", "volume_down"}; !reflect.DeepEqual(got, want) {
		t.Errorf("detect models = %v, want %v", got, want)
	}
	want := map[string]float64{"clanker": 0.5, "volume_up": 0.8, "volume_down": 0.7}
	if got := cfg.Wakeword.ModelThresholds(); !reflect.DeepEqual(got, want) {
		t.Errorf("thresholds = %v, want %v", got, want)
	}
	if cfg.Wakeword.Commands.Step != 5 || cfg.Wakeword.Commands.Cooldown.Duration() != 2*time.Second {
		t.Errorf("commands = %+v, want step 5 and cooldown 2s", cfg.Wakeword.Commands)
	}
}

func TestCommandThresholdAcceptsAConfiguredModelWhileDisabled(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword:
  addr: "wakeword:10400"
  thresholds: {volume_up: 0.75}
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Wakeword.ThresholdFor("volume_up"); got != 0.75 {
		t.Errorf("volume_up threshold = %v, want 0.75", got)
	}
}

func TestWakeCommandsEnvironmentOverrides(t *testing.T) {
	t.Setenv("ASSISTANT_WAKEWORD_COMMANDS_ENABLED", "true")
	t.Setenv("ASSISTANT_WAKEWORD_COMMANDS", "volume_up")
	t.Setenv("ASSISTANT_WAKEWORD_COMMAND_STEP", "7")
	t.Setenv("ASSISTANT_WAKEWORD_COMMAND_COOLDOWN", "900ms")
	t.Setenv("ASSISTANT_WAKEWORD_COMMAND_THRESHOLD", "0.85")

	cfg, err := config.Load(writeConfig(t, voiceBody(`wakeword: {addr: "wakeword:10400"}
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Wakeword.CommandModels(), []string{"volume_up"}; !reflect.DeepEqual(got, want) {
		t.Errorf("command models = %v, want %v", got, want)
	}
	if cfg.Wakeword.Commands.Step != 7 || cfg.Wakeword.Commands.Cooldown.Duration() != 900*time.Millisecond {
		t.Errorf("commands = %+v, want step 7 and cooldown 900ms", cfg.Wakeword.Commands)
	}
	if got := cfg.Wakeword.ThresholdFor("volume_up"); got != 0.85 {
		t.Errorf("volume_up threshold = %v, want 0.85", got)
	}
}

func TestWakeCommandsRejectInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "enabled with no models",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {enabled: true, models: []}
`),
		},
		{
			name: "step zero",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {step: 0}
`),
		},
		{
			name: "step above fifty",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {step: 51}
`),
		},
		{
			name: "cooldown zero",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {cooldown: 0s}
`),
		},
		{
			name: "threshold above one",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {threshold: 1.1}
`),
		},
		{
			name: "command name is also a wake word",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {models: ["clanker"]}
`),
		},
		{
			name: "whitespace in a command name",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  commands: {models: ["volume up"]}
`),
		},
		{
			name: "threshold for an unknown model",
			body: voiceBody(`wakeword:
  addr: "wakeword:10400"
  thresholds: {volume_sideways: 0.7}
`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, test.body)); err == nil {
				t.Fatal("Load accepted an invalid wakeword command config")
			}
		})
	}
}
