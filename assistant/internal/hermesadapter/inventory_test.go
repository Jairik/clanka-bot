package hermesadapter_test

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

// TestAllowlistIsVettedPublicReads verifies the shipped tools and their account grants.
func TestAllowlistIsVettedPublicReads(t *testing.T) {
	reg := productionRegistry(t)
	accounts := map[string]string{"weather.current": "weather", "weather.forecast": "weather", "web.search": ""}
	if want := slices.Sorted(maps.Keys(accounts)); !slices.Equal(want, slices.Sorted(slices.Values(hermesadapter.Allowlist))) {
		t.Fatalf("allowlist = %v, want %v", hermesadapter.Allowlist, want)
	}
	for _, name := range hermesadapter.Allowlist {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("allowlisted %s is not registered", name)
		}
		if info.LocalOnly {
			t.Errorf("%s is Local-only", name)
		}
		if info.Permission != tool.PermissionRead {
			t.Errorf("%s permission = %s, want read", name, info.Permission)
		}
		if hermesadapter.Classify(info) != hermesadapter.DecisionAllowlist {
			t.Errorf("%s classified as %s", name, hermesadapter.Classify(info))
		}
		account, ok := accountOf(name)
		if want := accounts[name]; account != want || ok != (want != "") {
			t.Errorf("%s account = %q, want %q", name, account, want)
		}
	}

	np, ok := reg.Info("spotify.now_playing")
	if !ok || !np.LocalOnly {
		t.Fatal("spotify.now_playing must stay Local-only")
	}
	if hermesadapter.Classify(np) != hermesadapter.DecisionLocalOnly {
		t.Fatalf("spotify.now_playing classified as %s", hermesadapter.Classify(np))
	}
}

func TestInventoryClassifiesEveryRegisteredTool(t *testing.T) {
	reg := productionRegistry(t)
	counts := map[hermesadapter.Decision]int{}
	for _, name := range reg.Names() {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("missing info for %s", name)
		}
		decision := hermesadapter.Classify(info)
		counts[decision]++
		if decision == hermesadapter.DecisionAllowlist && (info.LocalOnly || info.Permission != tool.PermissionRead) {
			t.Errorf("allowlisted %s is not a public read Tool", name)
		}
		if info.LocalOnly && hermesadapter.Allowlisted(name) {
			t.Errorf("Local-only %s is on the allowlist", name)
		}
		if info.Permission == tool.PermissionConfirm && hermesadapter.Allowlisted(name) {
			t.Errorf("confirm %s is on the allowlist", name)
		}
	}
	if counts[hermesadapter.DecisionAllowlist] != len(hermesadapter.Allowlist) {
		t.Fatalf("unexpected allowlist count; decisions = %v", counts)
	}
	if counts[hermesadapter.DecisionLocalOnly] == 0 || counts[hermesadapter.DecisionConfirm] == 0 || counts[hermesadapter.DecisionLater] == 0 {
		t.Fatalf("inventory collapsed: %v", counts)
	}
}

func productionRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(5 * time.Second)
	deps := tool.Deps{
		Clock:  func() time.Time { return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
		},
		Speaker: inventorySpeaker{},
		Audio:   &inventoryAudio{},
	}
	if err := tools.NewSet().RegisterAll(reg, deps, nil); err != nil {
		t.Fatalf("register production Tool modules: %v", err)
	}
	return reg
}

func accountOf(name string) (string, bool) {
	for _, account := range integrations.Catalog {
		for _, toolName := range account.Tools {
			if toolName == name {
				return account.ID, true
			}
		}
	}
	return "", false
}

type inventorySpeaker struct{}

func (inventorySpeaker) SayText(context.Context, string) error { return nil }

type inventoryAudio struct{}

func (*inventoryAudio) SetVolume(context.Context, int) error            { return nil }
func (*inventoryAudio) AdjustVolume(context.Context, int) (bool, error) { return true, nil }
func (*inventoryAudio) Level(context.Context) (int, error)              { return 40, nil }
func (*inventoryAudio) SetMute(context.Context, bool) error             { return nil }
func (*inventoryAudio) Stop(context.Context) error                      { return nil }
