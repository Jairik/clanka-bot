package voice_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib/voicelibtest"
	"github.com/Jairik/clanka-bot/assistant/tools/voice"
)

// fixture is the real store, settings service, voice library and matcher the
// Tools write through; no fake settings layer is involved.
type fixture struct {
	registry *tool.Registry
	deps     tool.Deps
	voices   *voicelib.Service
	settings *settings.Service
	store    *store.Store
}

type fakeProbe struct {
	revision string
}

func (p fakeProbe) Revision(context.Context) (string, error) { return p.revision, nil }
func (p fakeProbe) Validate(context.Context, string) (string, error) {
	return p.revision, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatalf("write jarvis state: %v", err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := voicelib.New(context.Background(), st, voicelib.Config{
		Dir: dir, TTSDir: "/voices", Default: "alba", Builtins: []string{"alba"},
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	settingsSvc, err := settings.New(context.Background(), st, lib.Definitions(voicelib.DefinitionConfig{Name: "alba", Rate: 1.0})...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	lib.Bind(settingsSvc)
	if err := lib.Rescan(context.Background(), fakeProbe{revision: "rev-1"}); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	reg := tool.NewRegistry(5 * 1e9)
	deps := tool.Deps{
		Voices: lib,
		Names:  namematch.New(nil, namematch.Config{Threshold: func() float64 { return 0.6 }}),
	}
	if err := voice.NewModule().Register(reg, deps); err != nil {
		t.Fatalf("register voice module: %v", err)
	}
	return &fixture{registry: reg, deps: deps, voices: lib, settings: settingsSvc, store: st}
}

func (f *fixture) run(t *testing.T, name string, args any) (any, error) {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = encoded
	}
	return f.registry.Execute(context.Background(), name, raw)
}

func TestVoiceSetSwitchesPersistsAndSpeaksTheNewVoice(t *testing.T) {
	f := newFixture(t)
	out, err := f.run(t, voice.SetName, voice.SetIn{Name: "jarvis"})
	if err != nil {
		t.Fatalf("voice.set: %v", err)
	}
	spoken, ok := f.registry.Speak(voice.SetName, out)
	if !ok || !strings.Contains(spoken, "jarvis") {
		t.Fatalf("Speak = %q, %v", spoken, ok)
	}
	if got := f.settings.String(voicelib.KeyName); got != "jarvis" {
		t.Fatalf("stored voice = %q", got)
	}
	// A fresh settings service over the same database reads the override.
	reopened, err := settings.New(context.Background(), f.store, f.voices.Definitions(voicelib.DefinitionConfig{Name: "alba", Rate: 1.0})...)
	if err != nil {
		t.Fatalf("reopen settings: %v", err)
	}
	value, ok := reopened.Get(voicelib.KeyName)
	if !ok || value.Value != "jarvis" || value.Source != settings.SourceOverride {
		t.Fatalf("reopened voice = %+v, ok=%v", value, ok)
	}
}

func TestVoiceSetResolvesThroughTheSharedNameMatcher(t *testing.T) {
	f := newFixture(t)
	args, ok := f.registry.Extract(voice.SetName, "use the jarvis voice")
	if !ok {
		t.Fatal("Extract did not find a name")
	}
	if _, err := f.registry.Execute(context.Background(), voice.SetName, args); err != nil {
		t.Fatalf("Execute extracted args: %v", err)
	}
	if got := f.settings.String(voicelib.KeyName); got != "jarvis" {
		t.Fatalf("stored voice = %q", got)
	}
}

func TestVoiceSetRejectsUnknownAndStaleVoices(t *testing.T) {
	f := newFixture(t)
	if _, err := f.run(t, voice.SetName, voice.SetIn{Name: "nobody"}); err == nil {
		t.Fatal("voice.set accepted an unknown name")
	} else {
		var toolErr *tool.ToolError
		if !errors.As(err, &toolErr) || !strings.Contains(toolErr.Say, "couldn't find a voice called nobody") {
			t.Fatalf("unknown error = %v", err)
		}
	}
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("unknown name changed the setting to %q", got)
	}

	if _, err := f.run(t, voice.SetName, voice.SetIn{Name: "jarvis"}); err != nil {
		t.Fatalf("set jarvis: %v", err)
	}
	if err := f.voices.Rescan(context.Background(), fakeProbe{revision: "rev-2"}); err != nil {
		t.Fatalf("second Rescan: %v", err)
	}
	_, err := f.run(t, voice.SetName, voice.SetIn{Name: "jarvis"})
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) || !strings.Contains(toolErr.Say, "different model revision") {
		t.Fatalf("stale error = %v", err)
	}
}

func TestVoiceListAndCurrentReflectTheLibraryAndRate(t *testing.T) {
	f := newFixture(t)
	out, err := f.run(t, voice.ListName, voice.ListIn{})
	if err != nil {
		t.Fatalf("voice.list: %v", err)
	}
	list, ok := out.(voice.ListOut)
	if !ok {
		t.Fatalf("voice.list output type %T", out)
	}
	if list.Current != "alba" || len(list.Voices) != 2 {
		t.Fatalf("list = %+v", list)
	}
	spoken, _ := f.registry.Speak(voice.ListName, list)
	if !strings.Contains(spoken, "alba") || !strings.Contains(spoken, "jarvis") {
		t.Fatalf("list Speak = %q", spoken)
	}

	if _, err := f.run(t, voice.SetName, voice.SetIn{Name: "jarvis"}); err != nil {
		t.Fatalf("set jarvis: %v", err)
	}
	if _, err := f.run(t, voice.RateName, voice.RateIn{Delta: 2}); err != nil {
		t.Fatalf("rate: %v", err)
	}
	current, err := f.run(t, voice.CurrentName, voice.CurrentIn{})
	if err != nil {
		t.Fatalf("voice.current: %v", err)
	}
	cur := current.(voice.CurrentOut)
	if cur.Voice != "jarvis" || cur.Builtin || cur.Rate != 1.2 {
		t.Fatalf("current = %+v", cur)
	}
	spoken, _ = f.registry.Speak(voice.CurrentName, cur)
	if !strings.Contains(spoken, "jarvis") || !strings.Contains(spoken, "20 percent faster") {
		t.Fatalf("current Speak = %q", spoken)
	}
}

func TestVoiceRateChangesPersistAndClamp(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		in   voice.RateIn
		want float64
	}{
		{voice.RateIn{Delta: 1}, 1.1},
		{voice.RateIn{Delta: 1}, 1.2},
		{voice.RateIn{Delta: 10}, 2.0},
		{voice.RateIn{Normal: true}, 1.0},
		// A bare call is the Control phrase "speak faster".
		{voice.RateIn{}, 1.1},
		{voice.RateIn{Delta: -1}, 1.0},
		{voice.RateIn{Delta: -10}, 0.5},
	}
	for _, tc := range cases {
		out, err := f.run(t, voice.RateName, tc.in)
		if err != nil {
			t.Fatalf("rate %+v: %v", tc.in, err)
		}
		if got := out.(voice.RateOut).Rate; got != tc.want {
			t.Fatalf("rate %+v = %v, want %v", tc.in, got, tc.want)
		}
		if got := f.settings.Float(voicelib.KeyRate); got != tc.want {
			t.Fatalf("stored rate = %v, want %v", got, tc.want)
		}
	}
}

func TestVoiceExtractors(t *testing.T) {
	setCases := map[string]string{
		"use the jarvis voice":             "jarvis",
		"switch to the alba voice":         "alba",
		"change your voice to jarvis":      "jarvis",
		"set your voice to the alba voice": "alba",
		"can you talk in the jarvis voice": "jarvis",
		"please use the alba voice":        "alba",
		"change your voice to bill boerst": "bill boerst",
	}
	for text, want := range setCases {
		in, ok := extract(t, voice.SetName, text)
		if !ok {
			t.Fatalf("Extract(%q) failed", text)
		}
		if in.(voice.SetIn).Name != want {
			t.Fatalf("Extract(%q) = %q, want %q", text, in.(voice.SetIn).Name, want)
		}
	}
	for _, text := range []string{"what voices do you have", "set a timer", "play some music", "turn up the volume"} {
		if _, ok := extract(t, voice.SetName, text); ok {
			t.Fatalf("Extract(%q) unexpectedly found a voice", text)
		}
	}

	rateCases := map[string]int{
		"speak faster":            1,
		"talk a little slower":    -1,
		"slow down your voice":    -1,
		"speed up":                1,
		"go back to normal speed": 0,
	}
	for text, want := range rateCases {
		in, ok := extract(t, voice.RateName, text)
		if !ok || in.(voice.RateIn).Delta != want {
			t.Fatalf("rate Extract(%q) = %+v, %v; want delta %d", text, in, ok, want)
		}
	}
	for _, text := range []string{"turn up the volume", "play faster music", "what time is it"} {
		if _, ok := extract(t, voice.RateName, text); ok {
			t.Fatalf("rate Extract(%q) unexpectedly matched", text)
		}
	}
}

// extract reaches the registry's Extract seam, which is what the Fast path
// uses before calling the handler.
func extract(t *testing.T, name, text string) (any, bool) {
	t.Helper()
	reg := tool.NewRegistry(1e9)
	if err := voice.NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, ok := reg.Extract(name, text)
	if !ok {
		return nil, false
	}
	switch name {
	case voice.SetName:
		var in voice.SetIn
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("decode set args: %v", err)
		}
		return in, true
	case voice.RateName:
		var in voice.RateIn
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("decode rate args: %v", err)
		}
		return in, true
	default:
		return nil, false
	}
}

func TestVoiceMetadataIsExplicit(t *testing.T) {
	reg := tool.NewRegistry(1e9)
	if err := voice.NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{voice.SetName, voice.ListName, voice.CurrentName, voice.RateName} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if info.Permission != tool.PermissionAct && info.Permission != tool.PermissionRead {
			t.Fatalf("%s permission = %q", name, info.Permission)
		}
		if !info.LocalOnly {
			t.Fatalf("%s must be LocalOnlyYes", name)
		}
	}
	set, _ := reg.Info(voice.SetName)
	if set.Permission != tool.PermissionAct || !set.HasExtract {
		t.Fatalf("voice.set = %+v", set)
	}
}

func TestVoiceToolsFailClosedWithoutTheLibrary(t *testing.T) {
	reg := tool.NewRegistry(1e9)
	if err := voice.NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := reg.Execute(context.Background(), voice.SetName, json.RawMessage(`{"name":"alba"}`))
	if err == nil {
		t.Fatal("voice.set succeeded without a library")
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) || !strings.Contains(toolErr.Say, "isn't available") {
		t.Fatalf("error = %v", err)
	}
}

func TestConcurrentVoiceRateWritesAndReads(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func(delta int) {
			defer wg.Done()
			_, _ = f.run(t, voice.RateName, voice.RateIn{Delta: delta})
		}(i%3 - 1)
		go func() {
			defer wg.Done()
			_, _ = f.run(t, voice.CurrentName, voice.CurrentIn{})
		}()
	}
	wg.Wait()
}
