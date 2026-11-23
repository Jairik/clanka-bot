package dashboard_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestHermesToolsAPI exercises the three owner routes and their mutation guards.
func TestHermesToolsAPI(t *testing.T) {
	f := newPhraseFixture(t, nil)
	reg := tool.NewRegistry(time.Second)
	type input struct {
		City string `json:"city,omitempty"`
	}
	if err := tool.Add(reg, tool.Tool[input, struct{}]{
		Name: "weather.current", Description: "Current conditions", Permission: tool.PermissionRead,
		LocalOnly: tool.LocalOnlyNo, Handler: func(context.Context, input) (struct{}, error) { return struct{}{}, nil },
	}); err != nil {
		t.Fatal(err)
	}
	adapter := hermesadapter.New(hermesadapter.Options{Registry: reg, Grants: f.integrations, Settings: f.settings})
	handler := newHandler(t, dashboard.Options{Settings: f.settings, HermesAdapter: adapter})
	const endpoint = "/api/ui/hermes/tools"
	const sameOrigin = "http://127.0.0.1:8082"
	const recipe = `{"name":"london","description":"Weather in London","base_tool":"weather.current","arguments":{"city":"London"},"enabled":true}`

	var view hermesadapter.RecipeTools
	if rec := getJSON(t, handler, endpoint, &view); rec.Code != http.StatusOK || len(view.Available) != 1 || view.Recipes == nil || len(view.Recipes) != 0 {
		t.Fatalf("initial manager response = %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		name, method, path, login, origin, body string
		status                                  int
	}{
		{"wrong owner", http.MethodGet, endpoint, "other@example.net", "", "", http.StatusForbidden},
		{"wrong owner save", http.MethodPost, endpoint, "other@example.net", sameOrigin, recipe, http.StatusForbidden},
		{"cross-origin save", http.MethodPost, endpoint, owner, "https://other.example", recipe, http.StatusForbidden},
		{"missing origin", http.MethodPost, endpoint, owner, "", recipe, http.StatusForbidden},
		{"unknown field", http.MethodPost, endpoint, owner, sameOrigin, `{"url":"https://example.test"}`, http.StatusBadRequest},
		{"argument list", http.MethodPost, endpoint, owner, sameOrigin, strings.Replace(recipe, `{"city":"London"}`, `["London"]`, 1), http.StatusBadRequest},
		{"trailing JSON", http.MethodPost, endpoint, owner, sameOrigin, recipe + "{}", http.StatusBadRequest},
		{"body too large", http.MethodPost, endpoint, owner, sameOrigin, recipe + strings.Repeat(" ", 16<<10), http.StatusBadRequest},
		{"save", http.MethodPost, endpoint, owner, sameOrigin, recipe, http.StatusOK},
		{"cross-origin delete", http.MethodPost, endpoint + "/london/delete", owner, "https://other.example", "", http.StatusForbidden},
		{"update", http.MethodPost, endpoint, owner, sameOrigin, strings.Replace(recipe, "true", "false", 1), http.StatusOK},
		{"delete", http.MethodPost, endpoint + "/london/delete", owner, sameOrigin, "", http.StatusOK},
		{"missing recipe", http.MethodPost, endpoint + "/london/delete", owner, sameOrigin, "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request(tc.method, tc.path, tc.login)
			req.Body = http.NoBody
			if tc.body != "" {
				req.Body = io.NopCloser(strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := serve(handler, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if tc.method != http.MethodPost || tc.status != http.StatusOK {
				return
			}
			var got hermesadapter.RecipeTools
			decodeJSON(t, rec, &got)
			if got.Available == nil || got.Recipes == nil {
				t.Fatalf("manager view = %+v", got)
			}
			want := 1
			if tc.name == "delete" {
				want = 0
			}
			if len(got.Recipes) != want || (want == 1 && (got.Recipes[0].Enabled != (tc.name == "save") || string(got.Recipes[0].Arguments["city"]) != `"London"`)) {
				t.Fatalf("manager view = %+v", got)
			}
		})
	}
}
