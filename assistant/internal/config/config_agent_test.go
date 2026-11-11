package config_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

func TestAgentDefaultsPointAtOpenRouter(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	provider, err := cfg.Agent.ActiveProvider()
	if err != nil {
		t.Fatalf("ActiveProvider: %v", err)
	}
	if provider.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("base url = %q", provider.BaseURL)
	}
	if provider.APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Errorf("api key env = %q", provider.APIKeyEnv)
	}
	if provider.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("model = %q", provider.Model)
	}
	if provider.AlternateModel != "deepseek/deepseek-v4-pro" {
		t.Errorf("alternate model = %q", provider.AlternateModel)
	}
	if provider.Routing["sort"] != "latency" {
		t.Errorf("routing = %v, want sort: latency", provider.Routing)
	}
	if cfg.Agent.FirstTokenTimeout.Duration() != 8*time.Second {
		t.Errorf("first token timeout = %s", cfg.Agent.FirstTokenTimeout.Duration())
	}
	if cfg.Agent.History.Turns != 10 || cfg.Agent.History.Quiet.Duration() != 5*time.Minute {
		t.Errorf("history = %+v", cfg.Agent.History)
	}
	if len(cfg.Agent.RequestPrefixes) == 0 {
		t.Error("request prefixes are empty")
	}
}

func TestLoadReadsAgentSettings(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
agent:
  provider: openrouter
  providers:
    openrouter:
      base_url: https://example.test/v1
      api_key_env: TEST_KEY
      model: test/model
      headers:
        x-session: abc
      routing:
        sort: throughput
  system_prompt: "Be brief."
  unavailable_message: "Brain offline."
  request_prefixes: ["hey agent"]
  history:
    turns: 4
    quiet: 2m
  first_token_timeout: 3s
  request_timeout: 30s
  max_steps: 3
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	provider, err := cfg.Agent.ActiveProvider()
	if err != nil {
		t.Fatalf("ActiveProvider: %v", err)
	}
	if provider.Model != "test/model" || provider.Headers["x-session"] != "abc" {
		t.Errorf("provider = %+v", provider)
	}
	if provider.Routing["sort"] != "throughput" {
		t.Errorf("routing = %v", provider.Routing)
	}
	if cfg.Agent.SystemPrompt != "Be brief." || cfg.Agent.UnavailableMessage != "Brain offline." {
		t.Errorf("agent = %+v", cfg.Agent)
	}
	if cfg.Agent.History.Turns != 4 || cfg.Agent.FirstTokenTimeout.Duration() != 3*time.Second {
		t.Errorf("agent = %+v", cfg.Agent)
	}
}

func TestLoadRejectsInvalidAgentSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing provider", "agent:\n  provider: \"\"\n"},
		{"unknown provider", "agent:\n  provider: nope\n"},
		{"missing model", `agent:
  providers:
    openrouter:
      base_url: https://example.test/v1
      api_key_env: TEST_KEY
      model: ""
`},
		{"missing unavailable message", "agent:\n  unavailable_message: \"\"\n"},
		{"empty request prefix", "agent:\n  request_prefixes: [\"\"]\n"},
		{"zero history turns", "agent:\n  history:\n    turns: 0\n"},
		{"zero quiet window", "agent:\n  history:\n    quiet: 0s\n"},
		{"zero first token timeout", "agent:\n  first_token_timeout: 0s\n"},
		{"zero max steps", "agent:\n  max_steps: 0\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, tt.body)); err == nil {
				t.Fatal("Load succeeded, want error")
			}
		})
	}
}
