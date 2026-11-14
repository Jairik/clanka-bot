package monster

import (
	"context"
	"strings"
	"testing"

	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type fakeAPI struct {
	devices []core.Device
	err     error
}

func (f fakeAPI) Devices(context.Context, string, string) ([]core.Device, error) {
	return f.devices, f.err
}
func (fakeAPI) Properties(context.Context, string, string, string) (map[string]any, error) {
	return nil, nil
}
func (fakeAPI) Set(context.Context, string, string, string, string, any) error { return nil }

func configured(api core.API) *Module {
	return &Module{Service: core.NewService(core.Options{
		API:         api,
		Credentials: func() (string, string, bool) { return "owner@example.com", "secret", true },
	})}
}

func TestRequiredSecrets(t *testing.T) {
	got := (&Module{}).RequiredSecrets()
	if len(got) != 2 || got[0] != core.SecretEmail || got[1] != core.SecretPassword {
		t.Fatalf("secrets = %v", got)
	}
}

func TestAuxHealthWithoutCredentialsIsNotConfigured(t *testing.T) {
	m := &Module{Service: core.NewService(core.Options{API: fakeAPI{}})}
	rows := m.AuxHealth(context.Background())
	if len(rows) != 1 || rows[0].State != tool.StatusNotConfigured || !strings.Contains(rows[0].Detail, "MONSTER_EMAIL") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAuxHealthReportsTheAccount(t *testing.T) {
	m := configured(fakeAPI{devices: []core.Device{{DSN: "DSN1"}, {DSN: "DSN2"}}})
	rows := m.AuxHealth(context.Background())
	if len(rows) != 1 || rows[0].State != tool.StatusOK || rows[0].Detail != "2 lights" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAuxHealthReportsACloudFailure(t *testing.T) {
	m := configured(fakeAPI{err: core.ErrCredentials})
	rows := m.AuxHealth(context.Background())
	if len(rows) != 1 || rows[0].State != tool.StatusError || !strings.Contains(rows[0].Detail, "rejected") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAuxHealthWithoutAService(t *testing.T) {
	if rows := (&Module{}).AuxHealth(context.Background()); rows != nil {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestRegisterWithoutSecretsLeavesThePageNotConfigured(t *testing.T) {
	m := NewModule()
	if err := m.Register(tool.NewRegistry(0), tool.Deps{}); err != nil {
		t.Fatal(err)
	}
	if m.Service.Configured() {
		t.Fatal("Register with no Secrets resolver must not claim an account")
	}
	if _, ok, detail := m.Service.Health(context.Background()); ok || !strings.Contains(detail, "MONSTER_EMAIL") {
		t.Fatalf("health = %v %q", ok, detail)
	}
}
