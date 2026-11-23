// Package hermesadapter is the private Go-owned Tool surface for Hermes.
//
// Credentials never leave this process: Hermes may list names, load one schema
// and invoke a Tool, and the registry runs the handler here. Local-only Tools
// and results, confirm Tools, disabled Tools, and Tools outside the allowlist
// or without a live account grant fail closed. Final Hermes runtime wiring
// belongs to the sidecar ticket; this package is the policy and HTTP seam.
package hermesadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Policy denials. Callers should treat every wrapped ErrDenied the same on
// the wire; Cause exists so tests can name the gate that closed.
var (
	ErrDenied = errors.New("hermesadapter: not allowed")
	ErrSecret = errors.New("hermesadapter: secret in payload")
)

// Denial is one fail-closed policy refusal. It never carries a secret value.
type Denial struct {
	Tool  string
	Cause string
}

// Error names the Tool and the gate without echoing arguments.
func (d *Denial) Error() string {
	if d == nil {
		return ErrDenied.Error()
	}
	if d.Tool == "" {
		return fmt.Sprintf("%v (%s)", ErrDenied, d.Cause)
	}
	return fmt.Sprintf("hermesadapter: %s: %s", d.Tool, d.Cause)
}

// Unwrap maps secret denials onto ErrSecret and every other gate onto ErrDenied.
func (d *Denial) Unwrap() error {
	if d != nil && d.Cause == causeSecret {
		return ErrSecret
	}
	return ErrDenied
}

const (
	causeAllowlist  = "allowlist"
	causeGrant      = "grant"
	causeLocalOnly  = "local_only"
	causeConfirm    = "confirm"
	causeDisabled   = "disabled"
	causeUnknown    = "unknown"
	causeSecret     = "secret"
	causeProvenance = "provenance"
)

// Descriptor is one Tool as a lazy listing: name and permission, no schemas.
type Descriptor struct {
	Name        string
	Description string
	Permission  string
}

// Schema is one Tool's contract, loaded only after the caller asks for it.
type Schema struct {
	Name         string
	Description  string
	Permission   string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// Surface is the Dashboard view of the enforced allowlist: currently callable
// Tools, and allowlisted Tools the live grant or policy is holding back.
type Surface struct {
	Allowed  []Descriptor
	Withheld []Descriptor
}

// Adapter enforces the allowlist, live grants and privacy line on every call.
type Adapter struct {
	registry  *tool.Registry
	grants    *integrations.Service
	secrets   *secrets.Service
	allowlist map[string]bool
	order     []string
}

// Options wires the adapter. An empty Allowlist uses the shipped default.
type Options struct {
	Registry  *tool.Registry
	Grants    *integrations.Service
	Secrets   *secrets.Service
	Allowlist []string
}

// New builds an adapter. A nil registry yields an empty, fail-closed surface.
func New(opts Options) *Adapter {
	names := opts.Allowlist
	if len(names) == 0 {
		names = append([]string(nil), Allowlist...)
	}
	allow := make(map[string]bool, len(names))
	order := make([]string, 0, len(names))
	for _, name := range names {
		if allow[name] {
			continue
		}
		allow[name] = true
		order = append(order, name)
	}
	return &Adapter{
		registry:  opts.Registry,
		grants:    opts.Grants,
		secrets:   opts.Secrets,
		allowlist: allow,
		order:     order,
	}
}

// Surface reports the live allowlist split into callable and withheld Tools.
// Listings never include input or output schemas.
func (a *Adapter) Surface() Surface {
	out := Surface{Allowed: []Descriptor{}, Withheld: []Descriptor{}}
	if a == nil || a.registry == nil {
		return out
	}
	for _, name := range a.order {
		desc, err := a.descriptor(name)
		if err != nil {
			continue
		}
		if a.inspect(name) == nil {
			out.Allowed = append(out.Allowed, desc)
			continue
		}
		out.Withheld = append(out.Withheld, desc)
	}
	return out
}

// List is the lazy discovery set: currently callable Tools, without schemas.
func (a *Adapter) List() []Descriptor {
	return a.Surface().Allowed
}

// Schema returns one Tool's schemas only when it is currently allowed.
func (a *Adapter) Schema(name string) (Schema, error) {
	canonical, err := a.canonical(name)
	if err != nil {
		return Schema{}, err
	}
	if err := a.inspect(canonical); err != nil {
		return Schema{}, err
	}
	info, ok := a.registry.Info(canonical)
	if !ok {
		return Schema{}, deny(canonical, causeUnknown)
	}
	return Schema{
		Name:         info.Name,
		Description:  info.Description,
		Permission:   string(info.Permission),
		InputSchema:  append(json.RawMessage(nil), info.InputSchema...),
		OutputSchema: append(json.RawMessage(nil), info.OutputSchema...),
	}, nil
}

// Invoke runs one Tool through the Go registry after the live policy check.
// Credentials stay in-process. A Local-only result is dropped. Duplicate
// read calls are allowed and each one re-checks grants.
func (a *Adapter) Invoke(ctx context.Context, name string, args json.RawMessage) (any, error) {
	canonical, err := a.canonical(name)
	if err != nil {
		return nil, err
	}
	if err := a.inspect(canonical); err != nil {
		return nil, err
	}
	if err := a.rejectSecrets(args); err != nil {
		return nil, deny(canonical, causeSecret)
	}

	req := tool.Request{
		ID:     tool.NewRequestID(),
		Path:   tool.PathHermes,
		Device: tool.DevicePi,
	}
	result, err := a.registry.Execute(tool.WithRequest(ctx, req), canonical, args)
	if err != nil {
		return nil, a.mapExecuteError(canonical, err)
	}
	// The static Local-only flag already blocked discovery; this catches a
	// declared provenance hook reporting that this one result must stay here.
	if a.registry.Provenance(canonical, result) {
		return nil, deny(canonical, causeProvenance)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, deny(canonical, causeUnknown)
	}
	if err := a.rejectSecrets(payload); err != nil {
		return nil, deny(canonical, causeSecret)
	}
	return result, nil
}

// inspect applies every gate except secret scanning. It is used by List,
// Schema and Invoke so a revoked grant disappears from the next listing.
func (a *Adapter) inspect(name string) error {
	if a == nil || a.registry == nil {
		return deny(name, causeUnknown)
	}
	if !a.allowlist[name] {
		return deny(name, causeAllowlist)
	}
	info, ok := a.registry.Info(name)
	if !ok {
		return deny(name, causeUnknown)
	}
	if !info.Enabled {
		return deny(name, causeDisabled)
	}
	if info.LocalOnly {
		return deny(name, causeLocalOnly)
	}
	if info.Permission == tool.PermissionConfirm {
		return deny(name, causeConfirm)
	}
	if account, ok := accountForTool(name); ok && !a.granted(account) {
		return deny(name, causeGrant)
	}
	return nil
}

// granted reads the live setting. A missing integrations service fails closed.
func (a *Adapter) granted(accountID string) bool {
	if a.grants == nil {
		return false
	}
	view, ok := a.grants.Get(accountID)
	return ok && view.Hermes
}

// canonical accepts a dotted name or the Agent wire name (domain__operation).
func (a *Adapter) canonical(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", deny("", causeUnknown)
	}
	if strings.Contains(name, ".") {
		if _, err := tool.WireName(name); err != nil {
			return "", deny(name, causeUnknown)
		}
		return name, nil
	}
	canonical, err := tool.CanonicalName(name)
	if err != nil {
		return "", deny(name, causeUnknown)
	}
	return canonical, nil
}

// descriptor is metadata for a registered allowlisted Tool, even when withheld.
func (a *Adapter) descriptor(name string) (Descriptor, error) {
	info, ok := a.registry.Info(name)
	if !ok {
		return Descriptor{}, deny(name, causeUnknown)
	}
	return Descriptor{
		Name:        info.Name,
		Description: info.Description,
		Permission:  string(info.Permission),
	}, nil
}

// mapExecuteError turns registry refusals into policy denials and leaves a
// genuine handler failure for the caller to report as a Tool error.
func (a *Adapter) mapExecuteError(name string, err error) error {
	var disabled *tool.DisabledError
	if errors.As(err, &disabled) {
		return deny(name, causeDisabled)
	}
	if errors.Is(err, tool.ErrConfirmationRequired) {
		return deny(name, causeConfirm)
	}
	return err
}

// rejectSecrets fails closed when JSON text contains a stored secret value.
func (a *Adapter) rejectSecrets(raw json.RawMessage) error {
	if a.secrets == nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	for _, item := range a.secrets.List() {
		secret, ok := a.secrets.Get(item.Name)
		if !ok || secret == "" {
			continue
		}
		if jsonContainsSecret(value, secret) {
			return ErrSecret
		}
	}
	return nil
}

// jsonContainsSecret walks maps, arrays and strings looking for a secret.
func jsonContainsSecret(value any, secret string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, secret)
	case []any:
		for _, item := range v {
			if jsonContainsSecret(item, secret) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if jsonContainsSecret(item, secret) {
				return true
			}
		}
	}
	return false
}

// accountForTool returns the integrations catalog id that unlocks name.
func accountForTool(name string) (string, bool) {
	for _, account := range integrations.Catalog {
		for _, toolName := range account.Tools {
			if toolName == name {
				return account.ID, true
			}
		}
	}
	return "", false
}

// deny builds a policy refusal. The cause is for tests, not a secret value.
func deny(name, cause string) error {
	return &Denial{Tool: name, Cause: cause}
}
