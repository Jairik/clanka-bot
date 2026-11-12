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

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	quietcore "github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// settingsFixture wires the real settings service, Tool policy and Dashboard,
// so the page is exercised against the same validation the API and voice Tools
// use rather than a copy.
type settingsFixture struct {
	t        *testing.T
	ctx      context.Context
	handler  *dashboard.Handler
	settings *settings.Service
	policy   *toolpolicy.Policy
}

func newSettingsFixture(t *testing.T) *settingsFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	registry := tool.NewRegistry(time.Second)
	add := func(name string, permission tool.Permission, localOnly tool.LocalOnly, examples []string) {
		t.Helper()
		err := tool.Add(registry, tool.Tool[struct{}, string]{
			Name:        name,
			Description: "test Tool " + name,
			Permission:  permission,
			LocalOnly:   localOnly,
			Handler: func(context.Context, struct{}) (string, error) {
				return name, nil
			},
			Speak:    func(string) string { return name },
			Examples: examples,
		})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	add("time.now", tool.PermissionRead, tool.LocalOnlyYes, []string{"what time is it"})
	add("audio.volume_set", tool.PermissionAct, tool.LocalOnlyNo, []string{"louder"})
	add("weather.current", tool.PermissionRead, tool.LocalOnlyNo, []string{"what's the weather"})

	setup, err := wording.Build(registry, wording.Config{
		ControlPhrases:     map[string]string{"what time is it": "time.now"},
		RequestPrefixes:    []string{"ask the agent"},
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	})
	if err != nil {
		t.Fatalf("wording.Build: %v", err)
	}
	toolDefs, err := toolpolicy.Definitions(registry, toolpolicy.Config{Disabled: []string{"weather.current"}})
	if err != nil {
		t.Fatalf("toolpolicy.Definitions: %v", err)
	}
	defs := append([]settings.Definition{}, toolDefs...)
	defs = append(defs, setup.Definitions...)
	defs = append(defs,
		// The live model is part of the Map 2 settings surface, not only the
		// JSON API.
		agent.ModelDefinition("code/model", "config/model"),
		settings.String("timezone", "", settings.WithConfig("Europe/London")),
		// A future sound setting must appear without a hardcoded page list.
		settings.String("sounds.wake", "builtin/wake.wav", settings.WithConfig("builtin/wake.wav")),
		// A numeric setting needs a decimal editor, not the no-editor fallback.
		namematch.Definition(0.62),
		settings.String("http.addr", "127.0.0.1:8080",
			settings.WithKind(settings.RestartRequired),
			settings.WithConfig("127.0.0.1:8080")),
	)
	// The per-kind Ring settings are ordinary live settings; #48's page must
	// expose them dynamically, including the Timer/Alarm split.
	defs = append(defs, ring.Definitions()...)
	// Quiet hours is grouped into its own Dashboard panel, but it is the same
	// live settings service as every other value.
	defs = append(defs, quietcore.Definitions(quietcore.DefaultConfig())...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	policy, err := toolpolicy.New(svc, registry)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}
	handler := newHandler(t, dashboard.Options{Settings: svc, ToolPolicy: policy})
	return &settingsFixture{t: t, ctx: ctx, handler: handler, settings: svc, policy: policy}
}

func (f *settingsFixture) form(method, target string, values url.Values) *http.Request {
	f.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func (f *settingsFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, target, owner))
	return rec
}

func (f *settingsFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, f.form(http.MethodPost, target, values))
	return rec
}

func TestSettingsPageListsEveryToolWithEffectiveState(t *testing.T) {
	f := newSettingsFixture(t)
	rec := f.get("/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Tools", "Settings", "time.now", "audio.volume_set", "weather.current",
		"Local only", "Enabled", "Permission", "Effective", "Source",
		"Restart required", "http.addr", "agent.model", "timezone",
		"Control phrases", "Examples", "Request prefixes", "Filler", "Unavailable message",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// weather.current is disabled in config.yaml, so its enabled source must be
	// shown as the config layer rather than as a code default.
	weather := pageSection(t, body, `id="tool-weather.current"`)
	if !strings.Contains(weather, "Disabled") || !strings.Contains(weather, "config") {
		t.Errorf("weather.current section does not show the configured disable: %s", weather)
	}
	// time.now is Local-only, which is code-declared and has no editor.
	clock := pageSection(t, body, `id="tool-time.now"`)
	if !strings.Contains(clock, "yes") {
		t.Errorf("time.now section does not show Local-only: %s", clock)
	}
	if strings.Contains(clock, `name="local_only"`) {
		t.Error("the page offers a Local-only editor")
	}
	// A declared act Tool offers only act and confirm; read is a loosening.
	audio := pageSection(t, body, `id="tool-audio.volume_set"`)
	if strings.Contains(audio, `value="read"`) {
		t.Errorf("audio.volume_set offers the loosening read choice: %s", audio)
	}
	for _, want := range []string{`value="act"`, `value="confirm"`} {
		if !strings.Contains(audio, want) {
			t.Errorf("audio.volume_set lacks %s", want)
		}
	}
	// A declared read Tool may be tightened through act to confirm.
	for _, want := range []string{`value="read"`, `value="act"`, `value="confirm"`} {
		if !strings.Contains(clock, want) {
			t.Errorf("time.now lacks the %s choice", want)
		}
	}
	// The page must not dump implementation JSON instead of forms.
	if strings.Contains(body, `{"key":`) {
		t.Error("page contains raw settings JSON")
	}
}

func TestSettingsPageMarksOnlyRestartRequiredSettings(t *testing.T) {
	f := newSettingsFixture(t)
	body := f.get("/settings").Body.String()
	if got := strings.Count(body, "pill-restart"); got != 1 {
		t.Fatalf("restart badges = %d, want exactly 1 for http.addr: %s", got, body)
	}
	addrs := pageSection(t, body, `id="setting-http.addr"`)
	if !strings.Contains(addrs, "Restart required") {
		t.Error("http.addr is not marked restart-required")
	}
	model := pageSection(t, body, `id="setting-agent.model"`)
	if strings.Contains(model, "Restart required") {
		t.Error("the live model setting is marked restart-required")
	}
}

func TestSettingsPageRendersFutureSettingsFromTheServiceList(t *testing.T) {
	f := newSettingsFixture(t)
	body := f.get("/settings").Body.String()
	// sounds.wake is not one of the hardcoded wording keys; it is rendered
	// because it is in settings.Service.List.
	if !strings.Contains(body, "sounds.wake") || !strings.Contains(body, "builtin/wake.wav") {
		t.Errorf("future setting does not appear dynamically: %s", body)
	}
	if !strings.Contains(body, "/settings/values/sounds.wake") {
		t.Error("future setting has no generated form action")
	}
	// The Ring settings are per kind and appear from the same service list.
	for _, key := range []string{"ring.timer.max_ring", "ring.alarm.ramp", "ring.timer.volume", "ring.alarm.speak_label"} {
		if !strings.Contains(body, key) || !strings.Contains(body, "/settings/values/"+key) {
			t.Errorf("Ring setting %q does not appear dynamically", key)
		}
	}
	threshold := pageSection(t, body, `id="setting-name_match.threshold"`)
	if !strings.Contains(threshold, "0.62") || !strings.Contains(threshold, `step="any"`) {
		t.Errorf("numeric threshold has no decimal editor: %s", threshold)
	}
	if !strings.Contains(threshold, "/settings/values/name_match.threshold") {
		t.Error("numeric threshold has no generated form action")
	}
}

func TestSettingsFloatFormSavesAndResets(t *testing.T) {
	f := newSettingsFixture(t)

	rec := f.post("/settings/values/name_match.threshold", url.Values{"value": {"0.74"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Float(namematch.KeyThreshold); got != 0.74 {
		t.Fatalf("threshold = %v, want the saved 0.74", got)
	}

	rec = f.post("/settings/values/name_match.threshold", url.Values{"value": {"not-a-number"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid number status = %d, want 400", rec.Code)
	}
	if got := f.settings.Float(namematch.KeyThreshold); got != 0.74 {
		t.Fatalf("threshold = %v after a rejected form, want 0.74", got)
	}

	rec = f.post("/settings/values/name_match.threshold/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Float(namematch.KeyThreshold); got != 0.62 {
		t.Fatalf("threshold after reset = %v, want the config 0.62", got)
	}
}

func TestSettingsToolFormSavesTightensAndResets(t *testing.T) {
	f := newSettingsFixture(t)

	rec := f.post("/settings/tools/time.now", url.Values{"field": {"enabled"}, "value": {"false"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings?flash=saved" {
		t.Errorf("save Location = %q", loc)
	}
	state := f.policy.State("time.now")
	if state.Enabled || state.EnabledSource != settings.SourceOverride {
		t.Fatalf("after disable: %+v", state)
	}
	timeNow := pageSection(t, f.get("/settings").Body.String(), `id="tool-time.now"`)
	if !strings.Contains(timeNow, "Reset enabled to code default") || !strings.Contains(timeNow, `action="/settings/tools/time.now/reset"`) {
		t.Errorf("overridden enabled field has no per-field reset: %s", timeNow)
	}

	rec = f.post("/settings/tools/time.now", url.Values{"field": {"permission"}, "value": {"confirm"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tighten status = %d, body = %s", rec.Code, rec.Body.String())
	}
	state = f.policy.State("time.now")
	if state.Permission != tool.PermissionConfirm || state.PermissionSource != settings.SourceOverride {
		t.Fatalf("after tighten: %+v", state)
	}

	rec = f.post("/settings/tools/time.now/reset", url.Values{"field": {"enabled"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("enabled reset status = %d", rec.Code)
	}
	if state := f.policy.State("time.now"); !state.Enabled || state.EnabledSource != settings.SourceDefault {
		t.Fatalf("after enabled reset: %+v", state)
	}
	rec = f.post("/settings/tools/time.now/reset", url.Values{"field": {"permission"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("permission reset status = %d", rec.Code)
	}
	if state := f.policy.State("time.now"); state.Permission != tool.PermissionRead || state.PermissionSource != settings.SourceDefault {
		t.Fatalf("after permission reset: %+v", state)
	}
}

func TestSettingsToolFormRejectsLooseningAndLocalOnlyWrites(t *testing.T) {
	f := newSettingsFixture(t)
	before := f.policy.State("audio.volume_set")

	rec := f.post("/settings/tools/audio.volume_set", url.Values{"field": {"permission"}, "value": {"read"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("loosening status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "loosen") || !strings.Contains(body, "role=\"alert\"") {
		t.Errorf("loosening error is not visible and accessible: %s", body)
	}

	rec = f.post("/settings/tools/audio.volume_set", url.Values{"field": {"permission"}, "value": {"act"}, "local_only": {"true"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("local-only write status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Local-only") {
		t.Errorf("local-only rejection is not explained: %s", body)
	}
	after := f.policy.State("audio.volume_set")
	if after != before {
		t.Fatalf("a rejected write changed the Tool: before %+v after %+v", before, after)
	}
}

func TestSettingsWordingFormSavesResetsAndPreservesInvalidInput(t *testing.T) {
	f := newSettingsFixture(t)

	invalid := url.Values{"value": {"same phrase\nsame phrase"}}
	rec := f.post("/settings/values/tools.time.now.control_phrases", invalid)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid list status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "repeated") {
		t.Errorf("validation error missing: %s", body)
	}
	if !strings.Contains(body, "same phrase\nsame phrase") {
		t.Errorf("invalid textarea input was not preserved: %s", body)
	}
	// The failed edit must not have been stored.
	if got := f.settings.Strings(wording.ControlPhrasesKey("time.now")); strings.Join(got, "|") != "what time is it" {
		t.Errorf("control phrases after rejected write = %v", got)
	}

	rec = f.post("/settings/values/tools.time.now.control_phrases", url.Values{"value": {"what time is it\ncurrent time"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Strings(wording.ControlPhrasesKey("time.now")); strings.Join(got, "|") != "what time is it|current time" {
		t.Errorf("control phrases = %v", got)
	}
	if value, _ := f.settings.Get(wording.ControlPhrasesKey("time.now")); value.Source != settings.SourceOverride {
		t.Errorf("source = %q, want override", value.Source)
	}
	phraseSection := pageSection(t, f.get("/settings").Body.String(), `id="setting-tools.time.now.control_phrases"`)
	if !strings.Contains(phraseSection, "Reset to config.yaml") || !strings.Contains(phraseSection, `action="/settings/values/tools.time.now.control_phrases/reset"`) {
		t.Errorf("overridden wording has no lower-layer reset: %s", phraseSection)
	}

	// Filler is a single string, not a JSON editor.
	rec = f.post("/settings/values/agent.filler", url.Values{"value": {"Just a moment."}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("filler save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(wording.FillerKey); got != "Just a moment." {
		t.Errorf("filler = %q", got)
	}

	rec = f.post("/settings/values/agent.filler/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("filler reset status = %d", rec.Code)
	}
	if value, _ := f.settings.Get(wording.FillerKey); value.Source != settings.SourceConfig || value.Value != "One moment." {
		t.Errorf("after reset: %+v", value)
	}

	// The live model setting is served and writable through the same form.
	rec = f.post("/settings/values/agent.model", url.Values{"value": {"other/model"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("model save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(agent.ModelKey); got != "other/model" {
		t.Errorf("model = %q", got)
	}
}

func TestSettingsPageEscapesValuesAndPreservesSingleLineInput(t *testing.T) {
	f := newSettingsFixture(t)
	if _, err := f.settings.Set(f.ctx, wording.FillerKey, "<script>alert(1)</script>"); err != nil {
		t.Fatalf("set filler: %v", err)
	}
	body := f.get("/settings").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a setting value was not HTML-escaped")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("page lacks the escaped setting value")
	}

	rec := f.post("/settings/values/agent.filler", url.Values{"value": {"   "}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank filler status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `value="   "`) {
		t.Errorf("rejected single-line input was not preserved: %s", body)
	}
	if got := f.settings.String(wording.FillerKey); got != "<script>alert(1)</script>" {
		t.Errorf("filler changed after a rejected write: %q", got)
	}
}

func TestSettingsRestartSettingShowsAndKeepsTheRestartNotice(t *testing.T) {
	f := newSettingsFixture(t)
	rec := f.post("/settings/values/http.addr", url.Values{"value": {"127.0.0.1:9999"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings?flash=restart" {
		t.Errorf("Location = %q, want the restart flash", loc)
	}
	body := f.get("/settings?flash=restart").Body.String()
	if !strings.Contains(body, "Restart the assistant") {
		t.Errorf("restart flash missing: %s", body)
	}
	if !strings.Contains(body, "pill-restart") {
		t.Error("restart-required badge missing after saving")
	}
	if got := f.settings.String("http.addr"); got != "127.0.0.1:9999" {
		t.Errorf("http.addr = %q", got)
	}
}

func TestSettingsFormsWorkAsHTMXFragments(t *testing.T) {
	f := newSettingsFixture(t)

	req := f.form(http.MethodPost, "/settings/values/agent.filler", url.Values{"value": {"Hold on."}})
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("htmx response is a full page")
	}
	if !strings.Contains(body, `id="settings-content"`) || !strings.Contains(body, "Saved") {
		t.Errorf("htmx response lacks the swapped content or flash: %s", body)
	}
	if got := f.settings.String(wording.FillerKey); got != "Hold on." {
		t.Errorf("filler = %q", got)
	}

	req = f.form(http.MethodPost, "/settings/values/tools.time.now.control_phrases", url.Values{"value": {"x\nx"}})
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx invalid status = %d, want a swap-friendly 200; body = %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("htmx error response is a full page")
	}
	if !strings.Contains(body, "repeated") || !strings.Contains(body, "x\nx") {
		t.Errorf("htmx error lacks the message or preserved input: %s", body)
	}
}

func TestSettingsFormsRequireIdentityAndSameOrigin(t *testing.T) {
	f := newSettingsFixture(t)

	rec := serve(f.handler, request(http.MethodGet, "/settings", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated page status = %d, want 403", rec.Code)
	}

	values := url.Values{"field": {"enabled"}, "value": {"false"}}
	req := httptest.NewRequest(http.MethodPost, "/settings/tools/time.now", strings.NewReader(values.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
	}
	if state := f.policy.State("time.now"); !state.Enabled {
		t.Error("cross-origin POST changed the Tool")
	}
}

func TestSettingsPageWithoutServicesExplainsTheWiring(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/settings", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not configured") {
		t.Errorf("unwired page does not explain itself: %s", body)
	}
}

// pageSection returns the HTML from one element's id to the next Tool/setting
// boundary. The tests use it to assert per-row controls without relying on
// global text that another row could satisfy.
func pageSection(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, id)
	if start < 0 {
		t.Fatalf("page lacks %s", id)
	}
	section := body[start+len(id):]
	end := len(section)
	for _, next := range []string{`id="tool-`, `id="setting-`} {
		if i := strings.Index(section, next); i >= 0 && i < end {
			end = i
		}
	}
	return section[:end]
}

func TestSettingsQuietHoursPanelSavesTheSameOverride(t *testing.T) {
	f := newSettingsFixture(t)
	rec := f.get("/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Quiet hours") || !strings.Contains(body, `id="setting-quiet.volume"`) {
		t.Fatalf("the Quiet hours panel is missing: %s", body)
	}
	resp := f.post("/settings/values/quiet.volume", url.Values{"value": {"40"}})
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body = %s", resp.Code, resp.Body.String())
	}
	value, _ := f.settings.Get(quietcore.VolumeKey)
	if value.Value != 40 || value.Source != settings.SourceOverride {
		t.Fatalf("quiet.volume = %+v, want a stored Dashboard override", value)
	}
	resp = f.post("/settings/values/quiet.start", url.Values{"value": {"21:15"}})
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("start save status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := f.settings.String(quietcore.StartKey); got != "21:15" {
		t.Fatalf("quiet.start = %q, want the Dashboard value", got)
	}
}
