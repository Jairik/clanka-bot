package dashboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/wakethreshold"
)

// wakewordSettingsResp is the Wake word part of the Settings view plus the
// generic groups it must stop duplicating.
type wakewordSettingsResp struct {
	Wakeword struct {
		Enabled bool
		Addr    string
		Note    string
		Models  []struct {
			Name      string
			Threshold string
			Command   bool
			Enabled   bool
			Setting   *settingRow
		}
	}
	Live    []settingRow
	Restart []settingRow
}

// TestSettingsShowsLiveWakeThresholds proves the prefix routing and the form:
// each configured model's threshold lives in the Wake word section with a
// live setting, an unknown key is not offered, and the write/reset path works.
func TestSettingsShowsLiveWakeThresholds(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Wakeword{
		Models:     []string{"clanker"},
		Thresholds: map[string]float64{"clanker": 0.6},
		Stop:       "stop",
	}
	defs := wakethreshold.Definitions(cfg)
	// The address is still a restart-required connection setting; the Wake
	// word section must show it read-only rather than as a generic form.
	defs = append(defs, settings.String("wakeword.addr", "wakeword:10400",
		settings.WithKind(settings.RestartRequired),
		settings.WithConfig("wakeword:10400")))

	service, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	policy, err := toolpolicy.New(service, tool.NewRegistry(time.Second))
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}
	handler := newHandler(t, dashboard.Options{
		Settings:   service,
		ToolPolicy: policy,
		Wakeword: dashboard.WakewordInfo{Addr: "wakeword:10400", Models: []dashboard.WakewordModel{
			{Name: "clanker", Threshold: 0.6, Enabled: true},
			{Name: "stop", Threshold: 0.7, Command: true},
		}},
	})

	var view wakewordSettingsResp
	if rec := getJSON(t, handler, "/api/ui/settings", &view); rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d: %s", rec.Code, rec.Body.String())
	}
	if !view.Wakeword.Enabled || view.Wakeword.Addr != "wakeword:10400" {
		t.Fatalf("wakeword header = %+v", view.Wakeword)
	}
	if len(view.Wakeword.Models) != 2 {
		t.Fatalf("models = %d, want the two configured", len(view.Wakeword.Models))
	}
	for _, model := range view.Wakeword.Models {
		if model.Setting == nil {
			t.Fatalf("model %s has no live threshold form", model.Name)
		}
		wantKey := wakethreshold.Key(model.Name)
		if model.Setting.Key != wantKey {
			t.Errorf("model %s setting key = %q, want %q", model.Name, model.Setting.Key, wantKey)
		}
		if model.Setting.Restart {
			t.Errorf("model %s threshold is marked restart-required", model.Name)
		}
		if model.Setting.Display != model.Threshold {
			t.Errorf("model %s display = %q, threshold = %q", model.Name, model.Setting.Display, model.Threshold)
		}
	}
	if got := view.Wakeword.Models[0].Setting.Display; got != "0.6" {
		t.Errorf("clanker threshold = %q, want the config layer 0.6", got)
	}
	// The prefix routing owns every wakeword.* key; the address included.
	for _, row := range append(append([]settingRow{}, view.Live...), view.Restart...) {
		if strings.HasPrefix(row.Key, "wakeword.") {
			t.Errorf("%s leaked into the generic groups", row.Key)
		}
	}

	// Saving the form is an ordinary live write, and Reset returns to config.
	post := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(url.Values{"value": {"0.66"}}.Encode()))
		req.Host = "127.0.0.1:8082"
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Tailscale-User-Login", owner)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/settings/values/" + wakethreshold.Key("clanker")); rec.Code != http.StatusOK {
		t.Fatalf("save threshold = %d: %s", rec.Code, rec.Body.String())
	}
	view = wakewordSettingsResp{}
	if rec := getJSON(t, handler, "/api/ui/settings", &view); rec.Code != http.StatusOK {
		t.Fatalf("GET after save = %d", rec.Code)
	}
	if got := view.Wakeword.Models[0].Setting; got == nil || got.Display != "0.66" || !got.Override {
		t.Fatalf("after save clanker setting = %+v, want a stored 0.66", got)
	}

	if rec := post("/settings/values/" + wakethreshold.Key("clanker") + "/reset"); rec.Code != http.StatusOK {
		t.Fatalf("reset threshold = %d: %s", rec.Code, rec.Body.String())
	}
	view = wakewordSettingsResp{}
	if rec := getJSON(t, handler, "/api/ui/settings", &view); rec.Code != http.StatusOK {
		t.Fatalf("GET after reset = %d", rec.Code)
	}
	if got := view.Wakeword.Models[0].Setting; got == nil || got.Display != "0.6" || got.Override {
		t.Fatalf("after reset clanker setting = %+v, want the config 0.6", got)
	}
}
