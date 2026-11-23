package dashboard_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestHermesPageShowsEnforcedAllowlist(t *testing.T) {
	f := newPhraseFixture(t, nil)
	reg := tool.NewRegistry(time.Second)
	addRead := func(name string) {
		t.Helper()
		if err := tool.Add(reg, tool.Tool[struct{}, struct{}]{
			Name:        name,
			Description: name,
			Permission:  tool.PermissionRead,
			LocalOnly:   tool.LocalOnlyNo,
			Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	addRead("weather.current")
	addRead("weather.forecast")
	addRead("github.pr_list")
	if err := tool.Add(reg, tool.Tool[struct{}, struct{}]{
		Name:        "github.issue_create",
		Description: "create",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
	}); err != nil {
		t.Fatalf("register confirm: %v", err)
	}

	adapter := hermesadapter.New(hermesadapter.Options{
		Registry: reg,
		Grants:   f.integrations,
	})
	handler := newHandler(t, dashboard.Options{
		Settings:      f.settings,
		Integrations:  f.integrations,
		Registry:      reg,
		HermesAdapter: adapter,
	})

	var view hermesResp
	rec := getJSON(t, handler, "/api/ui/hermes", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !view.AdapterReady || !view.SchemasDeferred {
		t.Fatalf("adapter flags = %+v", view)
	}
	if view.SurfaceCount != 0 || view.WithheldCount != 2 {
		t.Fatalf("ungranted surface = %+v", view)
	}

	rec = serve(handler, formRequest(http.MethodPost, "/hermes/integrations/weather/grant", url.Values{"allowed": {"on"}}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status = %d error = %s", rec.Code, view.Error)
	}
	if view.Flash != "Hermes access granted. The adapter enforces it on the next call." {
		t.Fatalf("flash = %q", view.Flash)
	}
	if view.SurfaceCount != 2 || view.WithheldCount != 0 {
		t.Fatalf("granted surface = count %d withheld %d names %#v", view.SurfaceCount, view.WithheldCount, view.Surface)
	}
	got := map[string]bool{}
	for _, item := range view.Surface {
		got[item.Name] = true
	}
	if !got["weather.current"] || !got["weather.forecast"] || got["github.pr_list"] {
		t.Fatalf("surface = %#v, want only the weather allowlist", view.Surface)
	}
}

func TestHermesPageWithoutAdapterHasEmptySurface(t *testing.T) {
	f := newPhraseFixture(t, nil)
	var view hermesResp
	getJSON(t, f.handler, "/api/ui/hermes", &view)
	if view.AdapterReady || view.SurfaceCount != 0 {
		t.Fatalf("empty adapter view = %+v", view)
	}
}
