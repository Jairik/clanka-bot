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
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

// settingRow and settingsResp decode the Settings view (encoding/json default
// field names).
type settingRow struct {
	Key, Label, Description, FormKind, Value, Edit, Display string
	Bool                                                    bool
	Source, SourceLabel, LowerLabel                         string
	Restart, Override                                       bool
	SaveAction, ResetAction                                 string
	PreviewAction, PreviewKind                              string
}

type settingsResp struct {
	Flash           string
	Error           string
	HasSettings     bool
	Voice           string
	CanPreview      bool
	PreviewMaxRunes int
	Tools           []struct {
		Name                              string
		Enabled                           bool
		EnabledSource, EnabledLabel       string
		EnabledLower                      string
		EnabledOverride                   bool
		Permission, PermissionLabel       string
		Declared                          string
		PermissionSource, PermissionLower string
		PermissionOverride                bool
		LocalOnly                         bool
		LocalOnlyLabel                    string
		Options                           []struct {
			Value, Label string
			Selected     bool
		}
	}
	Agent       []settingRow
	ToolWording []struct {
		Name              string
		Control, Examples *settingRow
	}
	Quiet   []settingRow
	Live    []settingRow
	Restart []settingRow
}

// setting finds one setting by key across every group the view uses.
func (v settingsResp) setting(key string) (settingRow, bool) {
	var all []settingRow
	all = append(all, v.Agent...)
	all = append(all, v.Quiet...)
	all = append(all, v.Live...)
	all = append(all, v.Restart...)
	for _, w := range v.ToolWording {
		if w.Control != nil {
			all = append(all, *w.Control)
		}
		if w.Examples != nil {
			all = append(all, *w.Examples)
		}
	}
	for _, row := range all {
		if row.Key == key {
			return row, true
		}
	}
	return settingRow{}, false
}

func (v settingsResp) mustSetting(t *testing.T, key string) settingRow {
	t.Helper()
	row, ok := v.setting(key)
	if !ok {
		t.Fatalf("view has no setting %s", key)
	}
	return row
}

func (f *settingsFixture) get(target string) (settingsResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	var v settingsResp
	rec := getJSON(f.t, f.handler, target, &v)
	return v, rec
}

// view reads /api/ui/settings, which must be a 200.
func (f *settingsFixture) view() (settingsResp, string) {
	f.t.Helper()
	v, rec := f.get("/api/ui/settings")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET settings = %d: %s", rec.Code, rec.Body.String())
	}
	return v, rec.Body.String()
}

// postView posts a write and decodes the answered view.
func (f *settingsFixture) postView(target string, values url.Values) (settingsResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.post(target, values)
	var v settingsResp
	decodeJSON(f.t, rec, &v)
	return v, rec
}

func (f *settingsFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, f.form(http.MethodPost, target, values))
	return rec
}

func TestSettingsReadListsEveryToolWithEffectiveState(t *testing.T) {
	f := newSettingsFixture(t)
	v, body := f.view()
	if !v.HasSettings {
		t.Fatal("HasSettings = false with services wired")
	}
	tools := map[string]int{}
	for i, row := range v.Tools {
		tools[row.Name] = i
	}
	for _, name := range []string{"time.now", "audio.volume_set", "weather.current"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("view lacks Tool %s: %+v", name, tools)
		}
	}
	for _, key := range []string{
		"http.addr", "agent.model", "timezone",
		"agent.filler", "agent.unavailable_message",
	} {
		if _, ok := v.setting(key); !ok {
			t.Errorf("view lacks setting %s", key)
		}
	}
	wording := map[string]bool{}
	for _, w := range v.ToolWording {
		wording[w.Name] = w.Control != nil && w.Examples != nil
	}
	if !wording["time.now"] {
		t.Errorf("time.now lacks its Control phrases and Examples: %v", wording)
	}
	if !strings.Contains(body, "agent.request_prefixes") {
		t.Error("view lacks the Request prefixes setting")
	}

	// weather.current is disabled in config.yaml, so its enabled source must be
	// shown as the config layer rather than as a code default.
	weather := v.Tools[tools["weather.current"]]
	if weather.Enabled || weather.EnabledSource != "config" || weather.EnabledLabel != "config.yaml" {
		t.Errorf("weather.current = %+v, want disabled by config", weather)
	}
	// time.now is Local-only, which is code-declared and has no editor.
	clock := v.Tools[tools["time.now"]]
	if !clock.LocalOnly || clock.LocalOnlyLabel != "yes" {
		t.Errorf("time.now = %+v, want Local-only", clock)
	}
	// A declared act Tool offers only act and confirm; read is a loosening.
	audio := v.Tools[tools["audio.volume_set"]]
	var got []string
	for _, option := range audio.Options {
		got = append(got, option.Value)
	}
	if strings.Join(got, ",") != "act,confirm" {
		t.Errorf("audio.volume_set options = %v, want act,confirm (no loosening)", got)
	}
	// A declared read Tool may be tightened through act to confirm.
	got = nil
	for _, option := range clock.Options {
		got = append(got, option.Value)
	}
	if strings.Join(got, ",") != "read,act,confirm" {
		t.Errorf("time.now options = %v, want read,act,confirm", got)
	}
	// The view carries typed rows, not implementation JSON blobs.
	if strings.Contains(body, `{"key":`) {
		t.Error("view contains raw settings JSON")
	}
}

func TestSettingsReadMarksOnlyRestartRequiredSettings(t *testing.T) {
	f := newSettingsFixture(t)
	v, _ := f.view()
	var restart []string
	for _, row := range v.Restart {
		restart = append(restart, row.Key)
	}
	if strings.Join(restart, ",") != "http.addr" {
		t.Fatalf("restart group = %v, want only http.addr", restart)
	}
	flagged := 0
	for _, group := range [][]settingRow{v.Agent, v.Quiet, v.Live, v.Restart} {
		for _, row := range group {
			if row.Restart {
				flagged++
			}
		}
	}
	if flagged != 1 {
		t.Errorf("restart-flagged settings = %d, want exactly 1 (http.addr)", flagged)
	}
	if !v.mustSetting(t, "http.addr").Restart {
		t.Error("http.addr is not marked restart-required")
	}
	if v.mustSetting(t, "agent.model").Restart {
		t.Error("the live model setting is marked restart-required")
	}
}

func TestSettingsReadRendersFutureSettingsFromTheServiceList(t *testing.T) {
	f := newSettingsFixture(t)
	v, _ := f.view()
	// sounds.wake is not one of the hardcoded wording keys; it is listed
	// because it is in settings.Service.List.
	wake := v.mustSetting(t, "sounds.wake")
	if wake.Value != "builtin/wake.wav" || wake.SaveAction != "/settings/values/sounds.wake" {
		t.Errorf("future setting = %+v, want its value and a generated save action", wake)
	}
	// The Ring settings are per kind and appear from the same service list.
	for _, key := range []string{"ring.timer.max_ring", "ring.alarm.ramp", "ring.timer.volume", "ring.alarm.speak_label"} {
		row := v.mustSetting(t, key)
		if row.SaveAction != "/settings/values/"+key {
			t.Errorf("Ring setting %q save action = %q", key, row.SaveAction)
		}
	}
	threshold := v.mustSetting(t, "name_match.threshold")
	if threshold.Value != "0.62" || threshold.FormKind != "decimal" {
		t.Errorf("numeric threshold = %+v, want a decimal editor holding 0.62", threshold)
	}
	if threshold.SaveAction != "/settings/values/name_match.threshold" {
		t.Errorf("numeric threshold save action = %q", threshold.SaveAction)
	}
}

func TestSettingsFloatFormSavesAndResets(t *testing.T) {
	f := newSettingsFixture(t)

	rec := f.post("/settings/values/name_match.threshold", url.Values{"value": {"0.74"}})
	if rec.Code != http.StatusOK {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Float(namematch.KeyThreshold); got != 0.62 {
		t.Fatalf("threshold after reset = %v, want the config 0.62", got)
	}
}

func TestSettingsToolFormSavesTightensAndResets(t *testing.T) {
	f := newSettingsFixture(t)

	saved, rec := f.postView("/settings/tools/time.now", url.Values{"field": {"enabled"}, "value": {"false"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(saved.Flash, "Saved.") {
		t.Errorf("save Flash = %q", saved.Flash)
	}
	state := f.policy.State("time.now")
	if state.Enabled || state.EnabledSource != settings.SourceOverride {
		t.Fatalf("after disable: %+v", state)
	}
	for _, tool := range saved.Tools {
		if tool.Name == "time.now" && (tool.Enabled || !tool.EnabledOverride || tool.EnabledLower != "code default") {
			t.Errorf("overridden enabled field = %+v, want a per-field reset to the code default", tool)
		}
	}

	rec = f.post("/settings/tools/time.now", url.Values{"field": {"permission"}, "value": {"confirm"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("tighten status = %d, body = %s", rec.Code, rec.Body.String())
	}
	state = f.policy.State("time.now")
	if state.Permission != tool.PermissionConfirm || state.PermissionSource != settings.SourceOverride {
		t.Fatalf("after tighten: %+v", state)
	}

	rec = f.post("/settings/tools/time.now/reset", url.Values{"field": {"enabled"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled reset status = %d", rec.Code)
	}
	if state := f.policy.State("time.now"); !state.Enabled || state.EnabledSource != settings.SourceDefault {
		t.Fatalf("after enabled reset: %+v", state)
	}
	rec = f.post("/settings/tools/time.now/reset", url.Values{"field": {"permission"}})
	if rec.Code != http.StatusOK {
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
	var v settingsResp
	decodeJSON(t, rec, &v)
	if !strings.Contains(v.Error, "loosen") {
		t.Errorf("loosening error is not reported: %q", v.Error)
	}

	rec = f.post("/settings/tools/audio.volume_set", url.Values{"field": {"permission"}, "value": {"act"}, "local_only": {"true"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("local-only write status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	v = settingsResp{}
	decodeJSON(t, rec, &v)
	if !strings.Contains(v.Error, "Local-only") {
		t.Errorf("local-only rejection is not explained: %q", v.Error)
	}
	after := f.policy.State("audio.volume_set")
	if after != before {
		t.Fatalf("a rejected write changed the Tool: before %+v after %+v", before, after)
	}
}

// TestSettingsFormsBoundTheRequestBody proves a settings write cannot read an
// unbounded body into memory: the form read is capped like every other write
// on this surface.
func TestSettingsFormsBoundTheRequestBody(t *testing.T) {
	f := newSettingsFixture(t)
	// Twice the cap the server applies, so the read has to refuse it.
	huge := url.Values{
		"field": {"enabled"},
		"value": {"false"},
		"pad":   {strings.Repeat("x", 2<<20)},
	}
	rec := f.post("/settings/tools/time.now", huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized form status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var v settingsResp
	decodeJSON(t, rec, &v)
	if !strings.Contains(v.Error, "form") {
		t.Errorf("oversized form error is not explained: %q", v.Error)
	}
	if state := f.policy.State("time.now"); !state.Enabled {
		t.Fatalf("an oversized form still wrote the Tool: %+v", state)
	}
}

func TestSettingsWordingFormSavesResetsAndPreservesInvalidInput(t *testing.T) {
	f := newSettingsFixture(t)

	invalid := url.Values{"value": {"same phrase\nsame phrase"}}
	rec := f.post("/settings/values/tools.time.now.control_phrases", invalid)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid list status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var rejected settingsResp
	decodeJSON(t, rec, &rejected)
	if !strings.Contains(rejected.Error, "repeated") {
		t.Errorf("validation error missing: %q", rejected.Error)
	}
	if got := rejected.mustSetting(t, "tools.time.now.control_phrases").Edit; got != "same phrase\nsame phrase" {
		t.Errorf("invalid textarea input was not preserved: %q", got)
	}
	// The failed edit must not have been stored.
	if got := f.settings.Strings(wording.ControlPhrasesKey("time.now")); strings.Join(got, "|") != "what time is it" {
		t.Errorf("control phrases after rejected write = %v", got)
	}

	saved, rec := f.postView("/settings/values/tools.time.now.control_phrases", url.Values{"value": {"what time is it\ncurrent time"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Strings(wording.ControlPhrasesKey("time.now")); strings.Join(got, "|") != "what time is it|current time" {
		t.Errorf("control phrases = %v", got)
	}
	if value, _ := f.settings.Get(wording.ControlPhrasesKey("time.now")); value.Source != settings.SourceOverride {
		t.Errorf("source = %q, want override", value.Source)
	}
	phrases := saved.mustSetting(t, "tools.time.now.control_phrases")
	if !phrases.Override || phrases.LowerLabel != "config.yaml" || phrases.ResetAction != "/settings/values/tools.time.now.control_phrases/reset" {
		t.Errorf("overridden wording has no lower-layer reset: %+v", phrases)
	}

	// Filler is a single string, not a JSON editor.
	rec = f.post("/settings/values/agent.filler", url.Values{"value": {"Just a moment."}})
	if rec.Code != http.StatusOK {
		t.Fatalf("filler save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(wording.FillerKey); got != "Just a moment." {
		t.Errorf("filler = %q", got)
	}

	rec = f.post("/settings/values/agent.filler/reset", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("filler reset status = %d", rec.Code)
	}
	if value, _ := f.settings.Get(wording.FillerKey); value.Source != settings.SourceConfig || value.Value != "One moment." {
		t.Errorf("after reset: %+v", value)
	}

	// The live model setting is served and writable through the same form.
	rec = f.post("/settings/values/agent.model", url.Values{"value": {"other/model"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("model save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(agent.ModelKey); got != "other/model" {
		t.Errorf("model = %q", got)
	}
}

func TestSettingsHostileValuesRoundTripInertlyAndSingleLineInputIsPreserved(t *testing.T) {
	f := newSettingsFixture(t)
	const hostile = "<script>alert(1)</script>"
	if _, err := f.settings.Set(f.ctx, wording.FillerKey, hostile); err != nil {
		t.Fatalf("set filler: %v", err)
	}
	v, body := f.view()
	if got := v.mustSetting(t, wording.FillerKey).Value; got != hostile {
		t.Errorf("filler value = %q, want it unchanged", got)
	}
	noRawScript(t, body)

	rec := f.post("/settings/values/agent.filler", url.Values{"value": {"   "}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank filler status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var rejected settingsResp
	decodeJSON(t, rec, &rejected)
	if got := rejected.mustSetting(t, wording.FillerKey).Edit; got != "   " {
		t.Errorf("rejected single-line input was not preserved: %q", got)
	}
	if got := f.settings.String(wording.FillerKey); got != hostile {
		t.Errorf("filler changed after a rejected write: %q", got)
	}
}

func TestSettingsRestartSettingShowsAndKeepsTheRestartNotice(t *testing.T) {
	f := newSettingsFixture(t)
	saved, rec := f.postView("/settings/values/http.addr", url.Values{"value": {"127.0.0.1:9999"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(saved.Flash, "Restart the assistant") {
		t.Errorf("save Flash = %q, want the restart notice", saved.Flash)
	}
	if row := saved.mustSetting(t, "http.addr"); !row.Restart || row.Value != "127.0.0.1:9999" {
		t.Errorf("http.addr after save = %+v", row)
	}
	// A reload with ?flash=restart keeps the notice, and the badge stays.
	v, _ := f.get("/api/ui/settings?flash=restart")
	if !strings.Contains(v.Flash, "Restart the assistant") {
		t.Errorf("restart flash missing on the read: %q", v.Flash)
	}
	if !v.mustSetting(t, "http.addr").Restart {
		t.Error("restart-required flag missing after saving")
	}
	if got := f.settings.String("http.addr"); got != "127.0.0.1:9999" {
		t.Errorf("http.addr = %q", got)
	}
}

// TestSettingsWritesAnswerJSONWithTheRealStatus pins the write contract the
// React app relies on: success is 200 with the refreshed view and a Flash, a
// rejected edit is the view with the real 4xx status, an Error and the
// preserved input.
func TestSettingsWritesAnswerJSONWithTheRealStatus(t *testing.T) {
	f := newSettingsFixture(t)

	v, rec := f.postView("/settings/values/agent.filler", url.Values{"value": {"Hold on."}})
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("save status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(v.Flash, "Saved") || v.mustSetting(t, wording.FillerKey).Value != "Hold on." {
		t.Errorf("save view = flash %q filler %+v", v.Flash, v.mustSetting(t, wording.FillerKey))
	}
	if got := f.settings.String(wording.FillerKey); got != "Hold on." {
		t.Errorf("filler = %q", got)
	}

	v, rec = f.postView("/settings/values/tools.time.now.control_phrases", url.Values{"value": {"x\nx"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(v.Error, "repeated") || v.Flash != "" {
		t.Errorf("invalid view = error %q flash %q", v.Error, v.Flash)
	}
	if got := v.mustSetting(t, "tools.time.now.control_phrases").Edit; got != "x\nx" {
		t.Errorf("preserved input = %q", got)
	}

	if _, rec = f.postView("/settings/values/nope.key", url.Values{"value": {"x"}}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown setting status = %d, want 404", rec.Code)
	}
}

func TestSettingsFormsRequireIdentityAndSameOrigin(t *testing.T) {
	f := newSettingsFixture(t)

	rec := serve(f.handler, request(http.MethodGet, "/api/ui/settings", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated page status = %d, want 403", rec.Code)
	}

	values := url.Values{"field": {"enabled"}, "value": {"false"}}
	req := httptest.NewRequest(http.MethodPost, "/settings/tools/time.now", strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
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

func TestSettingsReadWithoutServicesIsEmptyAndReadOnly(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	var v settingsResp
	rec := getJSON(t, handler, "/api/ui/settings", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if v.HasSettings || len(v.Tools) != 0 || len(v.Live) != 0 {
		t.Errorf("unwired view = %+v, want empty with HasSettings false", v)
	}
	req := request(http.MethodPost, "/settings/values/agent.filler", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := serve(handler, req); rec.Code != http.StatusNotFound {
		t.Errorf("write without the services = %d, want 404", rec.Code)
	}
}

func TestSettingsQuietHoursPanelSavesTheSameOverride(t *testing.T) {
	f := newSettingsFixture(t)
	v, _ := f.view()
	var quiet []string
	for _, row := range v.Quiet {
		quiet = append(quiet, row.Key)
	}
	found := false
	for _, key := range quiet {
		found = found || key == "quiet.volume"
	}
	if !found {
		t.Fatalf("the Quiet hours group lacks quiet.volume: %v", quiet)
	}
	if _, ok := func() (settingRow, bool) {
		for _, row := range v.Live {
			if strings.HasPrefix(row.Key, "quiet.") {
				return row, true
			}
		}
		return settingRow{}, false
	}(); ok {
		t.Error("a quiet.* setting leaked into the generic Live group")
	}
	resp := f.post("/settings/values/quiet.volume", url.Values{"value": {"40"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", resp.Code, resp.Body.String())
	}
	value, _ := f.settings.Get(quietcore.VolumeKey)
	if value.Value != 40 || value.Source != settings.SourceOverride {
		t.Fatalf("quiet.volume = %+v, want a stored Dashboard override", value)
	}
	resp = f.post("/settings/values/quiet.start", url.Values{"value": {"21:15"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("start save status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := f.settings.String(quietcore.StartKey); got != "21:15" {
		t.Fatalf("quiet.start = %q, want the Dashboard value", got)
	}
}
