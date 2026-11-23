package hermesadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type echoIn struct {
	City   string `json:"city,omitempty"`
	Token  string `json:"token,omitempty"`
	Secret string `json:"secret,omitempty"`
}

type echoOut struct {
	City    string `json:"city,omitempty"`
	Playing string `json:"playing,omitempty"`
	Calls   int    `json:"calls"`
}

type fixture struct {
	adapter *hermesadapter.Adapter
	grants  *integrations.Service
	secrets *secrets.Service
	calls   map[string]int
	pending *tool.Registry
	path    string
}

// newFixture registers the two allowlisted weather Tools plus forbidden
// neighbours, with live grants and a write-only secret store.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	secretSvc, err := secrets.New(ctx, st.DB(), []string{"WEATHERAPI_API_KEY", "GITHUB_TOKEN", "SPOTIFY_REFRESH_TOKEN"})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	setSvc, err := settings.New(ctx, st, integrations.Definitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	grants := integrations.New(secretSvc, setSvc)
	reg := tool.NewRegistry(time.Second)
	calls := map[string]int{}
	f := &fixture{grants: grants, secrets: secretSvc, calls: calls, pending: reg, path: path}

	add := func(name string, perm tool.Permission, local tool.LocalOnly, handler func(context.Context, echoIn) (echoOut, error)) {
		t.Helper()
		if err := tool.Add(reg, tool.Tool[echoIn, echoOut]{
			Name:        name,
			Description: name,
			Permission:  perm,
			LocalOnly:   local,
			Handler:     handler,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	count := func(name string) func(context.Context, echoIn) (echoOut, error) {
		return func(_ context.Context, in echoIn) (echoOut, error) {
			calls[name]++
			return echoOut{City: in.City, Calls: calls[name]}, nil
		}
	}
	add("weather.current", tool.PermissionRead, tool.LocalOnlyNo, count("weather.current"))
	add("weather.forecast", tool.PermissionRead, tool.LocalOnlyNo, count("weather.forecast"))
	add("github.pr_list", tool.PermissionRead, tool.LocalOnlyNo, count("github.pr_list"))
	add("github.issue_create", tool.PermissionConfirm, tool.LocalOnlyNo, count("github.issue_create"))
	add("spotify.now_playing", tool.PermissionRead, tool.LocalOnlyYes, func(_ context.Context, _ echoIn) (echoOut, error) {
		calls["spotify.now_playing"]++
		return echoOut{Playing: "private-track"}, nil
	})
	add("test.act", tool.PermissionAct, tool.LocalOnlyNo, count("test.act"))
	add("secret.echo", tool.PermissionRead, tool.LocalOnlyNo, func(_ context.Context, in echoIn) (echoOut, error) {
		calls["secret.echo"]++
		return echoOut{City: in.Secret}, nil
	})

	f.adapter = hermesadapter.New(hermesadapter.Options{
		Registry: reg,
		Grants:   grants,
		Secrets:  secretSvc,
		Settings: setSvc,
	})
	return f
}

// grantWeather turns the weather account grant on for the rest of the test.
func (f *fixture) grantWeather(t *testing.T) {
	t.Helper()
	if err := f.grants.SetGrant(context.Background(), "weather", true); err != nil {
		t.Fatalf("grant weather: %v", err)
	}
}

func TestListOmitsSchemasUntilAsked(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)

	listed := f.adapter.List()
	if len(listed) != 2 {
		t.Fatalf("list = %#v, want the two weather Tools", listed)
	}
	raw, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	if strings.Contains(string(raw), "input_schema") || strings.Contains(string(raw), "InputSchema") {
		t.Fatalf("list carried a schema: %s", raw)
	}

	schema, err := f.adapter.Schema("weather.current")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	if len(schema.InputSchema) == 0 || !strings.Contains(string(schema.InputSchema), "city") {
		t.Fatalf("schema missing input fields: %s", schema.InputSchema)
	}

	if _, err := f.adapter.Schema("github.pr_list"); !denied(err, "allowlist") {
		t.Fatalf("schema for a non-allowlisted Tool = %v", err)
	}
}

func TestInvokeRequiresLiveGrant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.adapter.Invoke(ctx, "weather.current", json.RawMessage(`{"city":"Berlin"}`)); !denied(err, "grant") {
		t.Fatalf("disabled grant = %v, want grant denial", err)
	}
	if f.calls["weather.current"] != 0 {
		t.Fatal("handler ran without a grant")
	}

	f.grantWeather(t)
	out, err := f.adapter.Invoke(ctx, "weather.current", json.RawMessage(`{"city":"Berlin"}`))
	if err != nil {
		t.Fatalf("granted invoke: %v", err)
	}
	got, ok := out.(echoOut)
	if !ok || got.City != "Berlin" || got.Calls != 1 {
		t.Fatalf("result = %#v", out)
	}

	if err := f.grants.SetGrant(ctx, "weather", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := f.adapter.Invoke(ctx, "weather__forecast", json.RawMessage(`{}`)); !denied(err, "grant") {
		t.Fatalf("revoked grant = %v, want grant denial", err)
	}
	if f.calls["weather.forecast"] != 0 {
		t.Fatal("handler ran after the grant was revoked")
	}
	if len(f.adapter.List()) != 0 {
		t.Fatalf("list after revoke = %#v, want empty", f.adapter.List())
	}
}

func TestForbiddenToolsFailClosed(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	ctx := context.Background()
	if err := f.grants.SetGrant(ctx, "github", true); err != nil {
		t.Fatalf("grant github: %v", err)
	}
	if err := f.grants.SetGrant(ctx, "spotify", true); err != nil {
		t.Fatalf("grant spotify: %v", err)
	}

	cases := []struct {
		name  string
		cause string
	}{
		{"github.pr_list", "allowlist"},
		{"github.issue_create", "allowlist"},
		{"spotify.now_playing", "allowlist"},
		{"secret.echo", "allowlist"},
		{"notatool", "unknown"},
	}
	for _, tc := range cases {
		if _, err := f.adapter.Invoke(ctx, tc.name, json.RawMessage(`{}`)); !denied(err, tc.cause) {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.cause)
		}
		if f.calls[tc.name] != 0 {
			t.Errorf("%s handler ran", tc.name)
		}
	}
	if pending, ok := f.pending.Pending(); ok {
		t.Fatalf("adapter proposed a Confirmation: %+v", pending)
	}
}

func TestLocalOnlyOnAllowlistStillFailsClosed(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[echoIn, echoOut]{
		Name:        "weather.current",
		Description: "private weather",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, echoIn) (echoOut, error) {
			return echoOut{Playing: "secret-weather"}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	adapter := hermesadapter.New(hermesadapter.Options{Registry: reg, Allowlist: []string{"weather.current"}})
	if _, err := adapter.Invoke(context.Background(), "weather.current", nil); !denied(err, "local_only") {
		t.Fatalf("local-only allowlisted Tool = %v", err)
	}
}

func TestActionOnAllowlistStillFailsClosed(t *testing.T) {
	f := newFixture(t)
	adapter := hermesadapter.New(hermesadapter.Options{
		Registry: f.pending, Allowlist: []string{"test.act"},
	})
	if _, err := adapter.Invoke(context.Background(), "test.act", nil); !denied(err, "permission") {
		t.Fatalf("allowlisted action = %v", err)
	}
	if len(adapter.List()) != 0 || f.calls["test.act"] != 0 {
		t.Fatal("allowlisted action was exposed or executed")
	}
}

func TestSecretsInPayloadsFailClosed(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	ctx := context.Background()
	if _, err := f.secrets.Set(ctx, "WEATHERAPI_API_KEY", "super-secret-weather-key"); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	args := json.RawMessage(`{"city":"Berlin","token":"super-secret-weather-key"}`)
	if _, err := f.adapter.Invoke(ctx, "weather.current", args); !errors.Is(err, hermesadapter.ErrSecret) {
		t.Fatalf("secret in args = %v, want ErrSecret", err)
	}
	if _, err := f.adapter.Invoke(ctx, "weather.current", json.RawMessage(`{"super-secret-weather-key":"Berlin"}`)); !errors.Is(err, hermesadapter.ErrSecret) {
		t.Fatalf("secret in object key = %v, want ErrSecret", err)
	}
	if f.calls["weather.current"] != 0 {
		t.Fatal("handler ran with a secret in the arguments")
	}

	leaky := hermesadapter.New(hermesadapter.Options{
		Registry:  f.pending,
		Grants:    f.grants,
		Secrets:   f.secrets,
		Allowlist: []string{"secret.echo"},
	})
	if err := f.grants.SetGrant(ctx, "weather", true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := leaky.Invoke(ctx, "secret.echo", json.RawMessage(`{"secret":"super-secret-weather-key"}`)); !errors.Is(err, hermesadapter.ErrSecret) {
		t.Fatalf("secret in result = %v, want ErrSecret", err)
	}
}

func TestDuplicateReadsAreAllowedAndRechecked(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		out, err := f.adapter.Invoke(ctx, "weather.current", json.RawMessage(`{"city":"Oslo"}`))
		if err != nil {
			t.Fatalf("invoke %d: %v", i, err)
		}
		got := out.(echoOut)
		if got.Calls != i {
			t.Fatalf("calls = %d, want %d", got.Calls, i)
		}
	}

	confirm := hermesadapter.New(hermesadapter.Options{
		Registry:  f.pending,
		Grants:    f.grants,
		Allowlist: []string{"github.issue_create"},
	})
	if err := f.grants.SetGrant(ctx, "github", true); err != nil {
		t.Fatalf("grant github: %v", err)
	}
	if _, err := confirm.Invoke(ctx, "github.issue_create", json.RawMessage(`{}`)); !denied(err, "confirm") {
		t.Fatalf("confirm Tool = %v, want confirm denial", err)
	}
	if _, err := confirm.Invoke(ctx, "github.issue_create", json.RawMessage(`{}`)); !denied(err, "confirm") {
		t.Fatalf("duplicate confirm = %v, want confirm denial", err)
	}
	if f.calls["github.issue_create"] != 0 {
		t.Fatal("confirm handler ran")
	}
	if pending, ok := f.pending.Pending(); ok {
		t.Fatalf("duplicate confirm proposed a pending call: %+v", pending)
	}
}

func TestDisabledToolIsWithheld(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	f.pending.SetPolicy(staticPolicy{name: "weather.current"})
	if _, err := f.adapter.Invoke(context.Background(), "weather.current", json.RawMessage(`{}`)); !denied(err, "disabled") {
		t.Fatalf("disabled Tool = %v", err)
	}
	surface := f.adapter.Surface()
	if len(surface.Allowed) != 1 || surface.Allowed[0].Name != "weather.forecast" {
		t.Fatalf("allowed = %#v", surface.Allowed)
	}
	if len(surface.Withheld) != 1 || surface.Withheld[0].Name != "weather.current" {
		t.Fatalf("withheld = %#v", surface.Withheld)
	}
}

type staticPolicy struct {
	name       string
	permission tool.Permission
}

func (p staticPolicy) Effective(name string, declared tool.Permission) tool.ToolSettings {
	if name == p.name && p.permission != "" {
		return tool.ToolSettings{Enabled: true, Permission: p.permission}
	}
	return tool.ToolSettings{Enabled: name != p.name, Permission: declared}
}

// denied reports whether err is a policy Denial with the expected cause.
func denied(err error, cause string) bool {
	var denial *hermesadapter.Denial
	return errors.As(err, &denial) && denial.Cause == cause
}
