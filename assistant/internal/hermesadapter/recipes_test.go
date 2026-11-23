package hermesadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestRecipeLifecycle exercises stored arguments, live grants and runtime HTTP.
func TestRecipeLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	recipe := hermesadapter.Recipe{
		Name: "weekend_weather", Description: "Check the weather in Oslo", BaseTool: "weather.current", Enabled: true,
		Arguments: map[string]json.RawMessage{"city": json.RawMessage(`"Oslo"`)},
	}
	view, err := f.adapter.SaveRecipe(ctx, recipe)
	if err != nil || len(view.Recipes) != 1 || len(view.Available) != 2 {
		t.Fatalf("save = %+v, %v", view, err)
	}
	const name = "recipe.weekend_weather"
	const path = "/tools/" + name
	assertUnavailable := func() {
		t.Helper()
		if slices.ContainsFunc(f.adapter.List(), func(d hermesadapter.Descriptor) bool { return d.Name == name }) {
			t.Fatal("unavailable recipe is discoverable")
		}
		if _, err := f.adapter.Schema(name); !errors.Is(err, hermesadapter.ErrDenied) {
			t.Fatalf("unavailable schema = %v", err)
		}
		if _, err := f.adapter.Invoke(ctx, name, nil); !errors.Is(err, hermesadapter.ErrDenied) {
			t.Fatalf("unavailable invocation = %v", err)
		}
	}
	assertUnavailable()
	f.grantWeather(t)
	handler := mustHandler(t, f)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := doHTTP(t, handler, method, path, "{}", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", method, rec.Code)
		}
	}
	list := doHTTP(t, handler, http.MethodGet, "/tools", "", testToken)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), name) || strings.Contains(list.Body.String(), "input_schema") {
		t.Fatalf("list = %d %s", list.Code, list.Body)
	}
	schema := doHTTP(t, handler, http.MethodGet, "/tools/recipe__weekend_weather", "", testToken)
	var got struct {
		Name        string `json:"name"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"input_schema"`
	}
	if err := json.Unmarshal(schema.Body.Bytes(), &got); err != nil || schema.Code != http.StatusOK || got.Name != name {
		t.Fatalf("schema = %d %s (%v)", schema.Code, schema.Body, err)
	}
	if got.InputSchema.Properties["city"] != nil || got.InputSchema.Properties["token"] == nil {
		t.Fatalf("schema properties = %v", got.InputSchema.Properties)
	}

	rec := doHTTP(t, handler, http.MethodPost, path, `{"arguments":{"city":"Paris"}}`, testToken)
	if rec.Code != http.StatusForbidden || f.calls["weather.current"] != 0 {
		t.Fatalf("override = %d %s; base calls = %d", rec.Code, rec.Body, f.calls["weather.current"])
	}
	rec = doHTTP(t, handler, http.MethodPost, path, `{"arguments":{}}`, testToken)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"city":"Oslo"`) || f.calls["weather.current"] != 1 {
		t.Fatalf("invoke = %d %s; base calls = %d", rec.Code, rec.Body, f.calls["weather.current"])
	}

	// A fresh settings service reads the saved recipe from the same SQLite file.
	db, err := store.Open(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc, err := settings.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := hermesadapter.New(hermesadapter.Options{Registry: f.pending, Settings: svc, Grants: f.grants})
	view, err = reloaded.RecipeTools(ctx)
	if err != nil || len(view.Recipes) != 1 || string(view.Recipes[0].Arguments["city"]) != `"Oslo"` || len(svc.List()) != 0 {
		t.Fatalf("reload = %+v, %v", view, err)
	}

	if err := f.grants.SetGrant(ctx, "weather", false); err != nil {
		t.Fatal(err)
	}
	assertUnavailable()
	f.grantWeather(t)
	f.pending.SetPolicy(staticPolicy{name: "weather.current", permission: tool.PermissionAct})
	assertUnavailable()
	f.pending.SetPolicy(staticPolicy{name: "weather.current"})
	assertUnavailable()
	recipe.Enabled = false
	if _, err := f.adapter.SaveRecipe(ctx, recipe); err != nil {
		t.Fatalf("disable recipe with disabled base: %v", err)
	}
	f.pending.SetPolicy(nil)
	assertUnavailable()
	recipe.Enabled = true
	if _, err := f.adapter.SaveRecipe(ctx, recipe); err != nil {
		t.Fatal(err)
	}
	view, err = f.adapter.DeleteRecipe(ctx, recipe.Name)
	if err != nil || view.Recipes == nil || len(view.Recipes) != 0 {
		t.Fatalf("delete = %+v, %v", view, err)
	}
	assertUnavailable()
}

// TestRecipeRejectsUnvettedToolsAndValues exercises policy and partial input validation.
func TestRecipeRejectsUnvettedToolsAndValues(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.secrets.Set(ctx, "GITHUB_TOKEN", "never-expose-this-value"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, description, base, args string
	}{
		{"invalid/name", "Invalid name", "weather.current", `{}`},
		{"invalid__wire", "Reserved wire identifier", "weather.current", `{}`},
		{"search", "", "weather.current", `{}`},
		{"search", "Local-only tool", "spotify.now_playing", `{}`},
		{"search", "Owner data", "github.pr_list", `{}`},
		{"search", "External write", "github.issue_create", `{}`},
		{"search", "Recipe as a base", "recipe.other", `{}`},
		{"search", "Invalid field", "weather.current", `{"endpoint":"https://example.test"}`},
		{"search", "Wrong type", "weather.current", `{"city":12}`},
		{"search", "never-expose-this-value", "weather.current", `{}`},
		{"search", "Credential in arguments", "weather.current", `{"city":"never-expose-this-value"}`},
	} {
		var recipe = hermesadapter.Recipe{Name: tc.name, Description: tc.description, BaseTool: tc.base, Enabled: true}
		if err := json.Unmarshal([]byte(tc.args), &recipe.Arguments); err != nil {
			t.Fatal(err)
		}
		if _, err := f.adapter.SaveRecipe(ctx, recipe); !errors.Is(err, hermesadapter.ErrInvalidRecipe) || strings.Contains(err.Error(), "never-expose-this-value") {
			t.Errorf("recipe %q / %q: expected a redacted validation error, got %v", tc.name, tc.base, err)
		}
	}
}
