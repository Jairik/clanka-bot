package dashboard

import (
	"context"
	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/lights"
	"net/http/httptest"
	"strings"
	"testing"
)

// lightModules supplies the registered module, as tools.Set does in production.
type lightModules struct{ m *lights.Module }

func (p lightModules) Module(string) tool.Module { return p.m }

// TestLightsAPI exercises the opt-in, saved-device validation and shared settings.
func TestLightsAPI(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := settings.New(ctx, st, core.Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	m := lights.NewModule()
	if err := m.Register(tool.NewRegistry(0), tool.Deps{Settings: func() *settings.Service { return ss }}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{settings: ss, toolModules: lightModules{m}}
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"action":"scan"}`, 502},
		{`{"action":"save","devices":[{"address":"bad","name":"Desk","protocol":"happy"}]}`, 400},
		{`{"action":"save","devices":[{"address":"AA:BB:CC:DD:EE:FF","name":"Desk","protocol":"happy"}]}`, 200},
		{`{"action":"enable","value":"true"}`, 200},
		{`{"action":"power","device":"unknown","value":"on"}`, 400},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/ui/lights", strings.NewReader(tc.body))
		h.lightsAPI(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	if !ss.Bool("lights.enabled") || len(m.Service.Devices()) != 1 {
		t.Fatal("settings did not persist")
	}
}
