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
	LocalModel string `json:"local_model"`
	LocalReady bool   `json:"local_ready"`
	// LocalProblem says why Local isn't ready, without connection details.
	LocalProblem   string   `json:"local_problem,omitempty"`
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

// sidecarStatus is what the running sidecar says about its local route. The
// sidecar reads its own copy of the settings, so after a partial recreate or a
// typo it can reject every local job, or route this side's key to a different
// endpoint, while this side looks configured.
type sidecarStatus struct {
	probed    bool   // a probe ran for this decision
	reachable bool   // /healthz answered 200 with JSON
	reports   bool   // the answer carries local_model and local_base_url
	model     string // the local model the sidecar routes, or ""
	baseURL   string // the local endpoint it sends this side's key to, or ""
}

// localProblem explains why Local can't run given the sidecar's status, or
// returns "" when it can. Without a sidecar (no runtime token) only this
// side's settings count, since no job can run anyway. An older sidecar image
// without the fields fails closed: it would check only the model, not which
// endpoint receives this side's key.
func (m *Manager) localProblem(sidecar sidecarStatus) string {
	switch {
	case !m.config.localReady():
		return "Hermes needs its local endpoint, model, and key."
	case m.run == nil:
		return ""
	case !sidecar.probed:
		// This request skipped the probe: the selection switched to Local, or
		// a running job finished, after it looked.
		return "Hermes isn't ready for a new job yet. Try again."
	case !sidecar.reachable:
		return "The Hermes sidecar isn't reachable right now."
	case !sidecar.reports:
		return "The Hermes sidecar image is out of date. Deploy matching assistant and hermes images."
	case sidecar.model != strings.TrimSpace(m.config.LocalModel) || sidecar.baseURL != strings.TrimSpace(m.config.LocalBaseURL):
		return "The Hermes sidecar has different local settings. Recreate assistant and hermes with the same settings."
	}
	return ""
}

// sidecarStatus probes the sidecar without holding m.mu. It skips the probe
// when local inference isn't configured on this side.
func (m *Manager) sidecarStatus(ctx context.Context) sidecarStatus {
	if m.probe == nil || !m.config.localReady() {
		return sidecarStatus{}
	}
	return m.probe(ctx)
}

// sidecarForSelection probes only while Local is selected, so OpenCode Go
// jobs never wait on the sidecar's health check. A new job also skips it while
// another job runs, since that request is refused as already working.
func (m *Manager) sidecarForSelection(ctx context.Context, newJob bool) sidecarStatus {
	m.mu.Lock()
	local := m.selection.Provider == localProvider && !m.closed && !(newJob && m.active != 0)
	m.mu.Unlock()
	if !local {
		return sidecarStatus{}
	}
	return m.sidecarStatus(ctx)
}

// Runtime returns the live dashboard view, including for an unwired dashboard.
func (m *Manager) Runtime() RuntimeView {
	if m == nil {
		return RuntimeView{Selection: Selection{Provider: cloudProvider, OpenCodeModel: cloudModel}, OpenCodeModels: slices.Clone(cloudModels)}
	}
	sidecar := m.sidecarStatus(m.ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimeView(sidecar)
}

func (m *Manager) runtimeView(sidecar sidecarStatus) RuntimeView {
	return RuntimeView{
		Selection:      m.selection,
		LocalModel:     strings.TrimSpace(m.config.LocalModel),
		LocalReady:     m.localProblem(sidecar) == "",
		LocalProblem:   m.localProblem(sidecar),
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
	sidecar := m.sidecarStatus(ctx)
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
	return m.runtimeView(sidecar), nil
}

// jobRequest carries only the submitted task and the selected model credential.
type jobRequest struct {
	Task     string `json:"task"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Key      string `json:"key"`
}

func (m *Manager) job(task string, sidecar sidecarStatus) (jobRequest, string) {
	job := jobRequest{Task: task, Provider: m.selection.Provider}
	if job.Provider == localProvider {
		if problem := m.localProblem(sidecar); problem != "" {
			return jobRequest{}, problem
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
