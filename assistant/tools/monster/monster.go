// Package monster is the Dashboard and Health seam for Monster Smart
// Lighting. It declares the account secrets and owns one shared cloud client;
// it registers no Tools because the feature is driven from the Dashboard.
package monster

import (
	"context"
	"strings"

	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the Monster Smart Lighting domain.
type Module struct {
	Service *core.Service
}

// NewModule builds the module with no service until Register runs.
func NewModule() *Module { return &Module{} }

// Name is the module name and the secret-declaration domain.
func (*Module) Name() string { return "monster" }

// RequiredSecrets declares the Monster app account. The Secrets page lists
// both names automatically; the stored values are write-only.
func (*Module) RequiredSecrets() []string {
	return []string{core.SecretEmail, core.SecretPassword}
}

// Register builds the shared service from the write-only secrets store. A nil
// resolver leaves the page in its not-configured state rather than creating a
// second credential store.
func (m *Module) Register(_ *tool.Registry, deps tool.Deps) error {
	credentials := func() (string, string, bool) {
		if deps.Secrets == nil {
			return "", "", false
		}
		email, emailOK := deps.Secrets.Get(core.SecretEmail)
		password, passwordOK := deps.Secrets.Get(core.SecretPassword)
		if !emailOK || !passwordOK || strings.TrimSpace(email) == "" || password == "" {
			return "", "", false
		}
		return strings.TrimSpace(email), password, true
	}
	m.Service = core.NewService(core.Options{
		API:         core.NewClient(),
		Credentials: credentials,
	})
	return nil
}

// AuxHealth reports the account and cloud reachability as one row. Missing
// credentials are a not-configured capability, not a broken module.
func (m *Module) AuxHealth(ctx context.Context) []tool.AuxHealth {
	if m == nil || m.Service == nil {
		return nil
	}
	configured, ok, detail := m.Service.Health(ctx)
	state := tool.StatusOK
	switch {
	case !configured:
		state = tool.StatusNotConfigured
	case !ok:
		state = tool.StatusError
	}
	return []tool.AuxHealth{{Name: "Monster lights", State: state, Detail: detail}}
}
