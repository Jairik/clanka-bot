package dashboard

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	monstertools "github.com/Jairik/clanka-bot/assistant/tools/monster"
)

type monsterModules struct{ m *monstertools.Module }

func (p monsterModules) Module(string) tool.Module { return p.m }

type monsterFake struct {
	devices []core.Device
	props   map[string]map[string]any
	listErr error
	setErr  error
	sets    []string
}

func (f *monsterFake) Devices(context.Context, string, string) ([]core.Device, error) {
	return f.devices, f.listErr
}
func (f *monsterFake) Properties(_ context.Context, _, _, dsn string) (map[string]any, error) {
	return f.props[dsn], nil
}
func (f *monsterFake) Set(_ context.Context, _, _, dsn, name string, value any) error {
	f.sets = append(f.sets, dsn+":"+name)
	return f.setErr
}

func monsterHandler(t *testing.T, api core.API, hasSecrets bool) *Handler {
	t.Helper()
	credentials := func() (string, string, bool) {
		if !hasSecrets {
			return "", "", false
		}
		return "owner@example.com", "secret", true
	}
	m := &monstertools.Module{Service: core.NewService(core.Options{API: api, Credentials: credentials})}
	return &Handler{toolModules: monsterModules{m: m}}
}

func TestMonsterAPIWithoutModule(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("GET", "/api/ui/monster", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("POST", "/api/ui/monster", strings.NewReader(`{"action":"power"}`)))
	if w.Code != 503 {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
}

func TestMonsterAPIShowsTheAccountAndWrites(t *testing.T) {
	api := &monsterFake{
		devices: []core.Device{{DSN: "DSN1", Name: "Duo+", Model: "MNW7-2027-ICW"}},
		props: map[string]map[string]any{"DSN1": {
			"power": 1, "mode": "white", "brightness": 40, "color_temp": 50,
		}},
	}
	h := monsterHandler(t, api, true)

	w := httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("GET", "/api/ui/monster", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"name":"Duo+"`) || !strings.Contains(body, `"kelvin":4600`) {
		t.Fatalf("GET: %d %s", w.Code, body)
	}

	w = httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("POST", "/api/ui/monster", strings.NewReader(`{"action":"power","device":"DSN1","value":"on"}`)))
	if w.Code != 200 || len(api.sets) != 1 || api.sets[0] != "DSN1:power" {
		t.Fatalf("POST: %d %s sets=%v", w.Code, w.Body.String(), api.sets)
	}

	w = httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("POST", "/api/ui/monster", strings.NewReader(`{"action":"dance","device":"DSN1"}`)))
	if w.Code != 400 {
		t.Fatalf("bad action: %d %s", w.Code, w.Body.String())
	}
}

func TestMonsterAPIWithoutSecretsPointsAtSecrets(t *testing.T) {
	h := monsterHandler(t, &monsterFake{}, false)
	w := httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("GET", "/api/ui/monster", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "MONSTER_EMAIL") {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
}

func TestMonsterAPIUpstreamFailureIsBadGateway(t *testing.T) {
	api := &monsterFake{
		devices: []core.Device{{DSN: "DSN1"}},
		setErr:  &core.StatusError{Code: 503},
	}
	h := monsterHandler(t, api, true)
	w := httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("POST", "/api/ui/monster", strings.NewReader(`{"action":"power","device":"DSN1","value":"on"}`)))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "HTTP 503") {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
}

func TestMonsterAPIListErrorStaysInTheView(t *testing.T) {
	h := monsterHandler(t, &monsterFake{listErr: errors.New("boom")}, true)
	w := httptest.NewRecorder()
	h.monsterAPI(w, httptest.NewRequest("GET", "/api/ui/monster", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":true`) || !strings.Contains(w.Body.String(), `"error":"Monster request failed."`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
}
