package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

const (
	localProvider = "local"
	cloudProvider = "opencode-go"
	cloudModel    = "deepseek-v4.1-flash"
	runtimeKey    = "hermes.runtime"
)

// Keep the sidecar's validation in sync with this zero-day retention allowlist.
var cloudModels = []string{cloudModel, "deepseek-v4-pro", "glm-5.3-flash", "kimi-k3"}

var (
	ErrRuntimeUnavailable = errors.New("The Hermes runtime settings are unavailable.")
	ErrProvider           = errors.New("Choose Local or OpenCode Go.")
	ErrCloudModel         = errors.New("Choose a supported OpenCode Go model.")
	ErrCloudKey           = errors.New("OpenCode Go needs its dedicated Hermes key. Add it in Secrets.")
)

// RuntimeConfig keeps operator-owned connection settings separate from the dashboard selection.
type RuntimeConfig struct {
	Settings     *settings.Service
	LocalBaseURL string
	LocalModel   string
	LocalKey     string
	OpenCodeKey  func() string
}

// Selection is stored as one private settings value, so both fields change together.
type Selection struct {
	Provider      string `json:"provider"`
	OpenCodeModel string `json:"open_code_go_model"`
}

// RuntimeView exposes the selection and readiness without connection details.
type RuntimeView struct {
	Selection
	LocalModel     string   `json:"local_model"`
	LocalReady     bool     `json:"local_ready"`
	OpenCodeReady  bool     `json:"open_code_go_ready"`
	OpenCodeModels []string `json:"open_code_go_models"`
}

func (s Selection) validate() error {
	if s.Provider != localProvider && s.Provider != cloudProvider {
		return ErrProvider
	}
	if !slices.Contains(cloudModels, s.OpenCodeModel) {
		return ErrCloudModel
	}
	return nil
}

func (c RuntimeConfig) loadSelection(ctx context.Context) (Selection, error) {
	selection := Selection{Provider: cloudProvider, OpenCodeModel: cloudModel}
	if strings.TrimSpace(c.LocalBaseURL) != "" {
		selection.Provider = localProvider
	}
	if c.Settings == nil {
		return selection, nil
	}
	raw, found, err := c.Settings.GetPrivate(ctx, runtimeKey)
	if err != nil || !found {
		return selection, err
	}
	if err := json.Unmarshal([]byte(raw), &selection); err != nil {
		return Selection{}, err
	}
	return selection, selection.validate()
}

func (c RuntimeConfig) cloudKey() string {
	if c.OpenCodeKey == nil {
		return ""
	}
	return strings.TrimSpace(c.OpenCodeKey())
}

func (c RuntimeConfig) localReady() bool {
	return strings.TrimSpace(c.LocalBaseURL) != "" && strings.TrimSpace(c.LocalModel) != "" && strings.TrimSpace(c.LocalKey) != ""
}

// Runtime returns the live dashboard view, including for an unwired dashboard.
func (m *Manager) Runtime() RuntimeView {
	if m == nil {
		return RuntimeView{Selection: Selection{Provider: cloudProvider, OpenCodeModel: cloudModel}, OpenCodeModels: slices.Clone(cloudModels)}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimeView()
}

func (m *Manager) runtimeView() RuntimeView {
	return RuntimeView{
		Selection:      m.selection,
		LocalModel:     strings.TrimSpace(m.config.LocalModel),
		LocalReady:     m.config.localReady(),
		OpenCodeReady:  m.config.cloudKey() != "",
		OpenCodeModels: slices.Clone(cloudModels),
	}
}

// SetRuntime persists one selection before making it visible to new jobs.
func (m *Manager) SetRuntime(ctx context.Context, selection Selection) (RuntimeView, error) {
	if m == nil || m.config.Settings == nil {
		return RuntimeView{}, ErrRuntimeUnavailable
	}
	if err := selection.validate(); err != nil {
		return RuntimeView{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if selection.Provider == cloudProvider && m.config.cloudKey() == "" {
		return RuntimeView{}, ErrCloudKey
	}
	raw, err := json.Marshal(selection)
	if err != nil {
		return RuntimeView{}, err
	}
	if err := m.config.Settings.SetPrivate(ctx, runtimeKey, string(raw)); err != nil {
		return RuntimeView{}, err
	}
	m.selection = selection
	return m.runtimeView(), nil
}

// jobRequest carries only the submitted task and the selected model credential.
type jobRequest struct {
	Task     string `json:"task"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Key      string `json:"key"`
}

func (m *Manager) job(task string) (jobRequest, string) {
	job := jobRequest{Task: task, Provider: m.selection.Provider}
	if job.Provider == localProvider {
		if !m.config.localReady() {
			return jobRequest{}, "Hermes needs its local endpoint, model, and key."
		}
		job.Model, job.Key = strings.TrimSpace(m.config.LocalModel), strings.TrimSpace(m.config.LocalKey)
	} else {
		job.Model, job.Key = m.selection.OpenCodeModel, m.config.cloudKey()
		if job.Key == "" {
			return jobRequest{}, "Hermes needs its OpenCode Go key."
		}
	}
	return job, ""
}
