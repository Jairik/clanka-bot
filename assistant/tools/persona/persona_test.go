package persona_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
	personatools "github.com/Jairik/clanka-bot/assistant/tools/persona"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	settings *settings.Service
	service  *persona.Service
	module   *personatools.Module
	registry *tool.Registry
}

// newFixture wires the real Persona service and settings service over a real
// SQLite database, with the library's built-in voices (no TTS or files).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	voices, err := voicelib.New(ctx, st, voicelib.Config{
		Dir:      filepath.Join(dir, "voices"),
		TTSDir:   "/voices",
		Default:  "alba",
		Builtins: []string{"alba", "jarvis"},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	service, err := persona.New(ctx, st, voices, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("persona.New: %v", err)
	}
	defs := []settings.Definition{
		settings.String(voicelib.KeyName, "alba", settings.WithConfig("alba")),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig(wording.DefaultFiller)),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage, settings.WithConfig(wording.DefaultUnavailableMessage)),
	}
	defs = append(defs, service.Definitions()...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	voices.Bind(svc)
	service.Bind(svc)

	registry := tool.NewRegistry(time.Second)
	module := personatools.NewModule()
	module.Bind(service)
	if err := module.Register(registry, tool.Deps{Voices: voices}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &fixture{t: t, ctx: ctx, settings: svc, service: service, module: module, registry: registry}
}

func (f *fixture) create(t *testing.T, name, voice string) persona.Persona {
	t.Helper()
	p, err := f.service.Create(f.ctx, persona.Input{
		Name:        name,
		Voice:       voice,
		Personality: "You are " + name + ".",
		Filler:      "Processing.",
		Unavailable: "The chamber is offline.",
	})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return p
}

func (f *fixture) execute(t *testing.T, name string, args any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return f.registry.Execute(f.ctx, name, raw)
}

func TestPersonaSwitchAppliesTheBundleAndSpeaksTheConfirmation(t *testing.T) {
	f := newFixture(t)
	f.create(t, "GLaDOS", "jarvis")

	result, err := f.execute(t, personatools.SwitchName, personatools.SwitchIn{Name: "glados"})
	if err != nil {
		t.Fatalf("persona.switch: %v", err)
	}
	out, ok := result.(personatools.SwitchOut)
	if !ok {
		t.Fatalf("result = %T, want SwitchOut", result)
	}
	if out.Persona != "GLaDOS" || out.Voice != "jarvis" {
		t.Fatalf("SwitchOut = %+v", out)
	}
	if got, ok := f.registry.Speak(personatools.SwitchName, out); !ok || got != "Okay, switching to the GLaDOS persona." {
		t.Fatalf("Speak = %q, %v", got, ok)
	}
	if got := f.settings.String(voicelib.KeyName); got != "jarvis" {
		t.Fatalf("voice.name = %q, want jarvis", got)
	}
	if got := f.settings.String(wording.FillerKey); got != "Processing." {
		t.Fatalf("agent.filler = %q", got)
	}
	if got := f.settings.String(wording.UnavailableKey); got != "The chamber is offline." {
		t.Fatalf("agent.unavailable_message = %q", got)
	}
	if got := f.settings.String(persona.ActiveKey); got != "GLaDOS" {
		t.Fatalf("persona.active = %q", got)
	}
}

func TestPersonaSwitchResolvesThroughTheSharedNameMatcher(t *testing.T) {
	f := newFixture(t)
	f.create(t, "GLaDOS", "alba")
	// The production matcher is late-bound and shared with sounds and voices.
	// Exact names resolve without an embed call; a nil embedder proves the
	// lookup did not need one.
	registry := tool.NewRegistry(time.Second)
	module := personatools.NewModule()
	module.Bind(f.service)
	if err := module.Register(registry, tool.Deps{
		Voices: nil,
		Names:  namematch.New(nil, namematch.Config{}),
	}); err != nil {
		t.Fatalf("Register with the shared matcher: %v", err)
	}
	raw, err := json.Marshal(personatools.SwitchIn{Name: "glados"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if _, err := registry.Execute(f.ctx, personatools.SwitchName, raw); err != nil {
		t.Fatalf("switch through the shared matcher: %v", err)
	}
	if got := f.settings.String(persona.ActiveKey); got != "GLaDOS" {
		t.Fatalf("persona.active = %q", got)
	}
}

func TestPersonaSwitchResolvesThroughExtractAndRejectsUnknownOrStale(t *testing.T) {
	f := newFixture(t)
	f.create(t, "GLaDOS", "jarvis")
	f.create(t, "Alfred", "alba")

	args, ok := f.registry.Extract(personatools.SwitchName, "switch to the GLaDOS persona")
	if !ok {
		t.Fatal("Extract did not recognise the persona switch")
	}
	var in personatools.SwitchIn
	if err := json.Unmarshal(args, &in); err != nil {
		t.Fatalf("unmarshal extracted args: %v", err)
	}
	if in.Name != "glados" {
		t.Fatalf("Extract name = %q, want glados", in.Name)
	}
	if _, err := f.execute(t, personatools.SwitchName, in); err != nil {
		t.Fatalf("switch after Extract: %v", err)
	}

	// A voice request must never be claimed by persona.switch extraction.
	if _, ok := f.registry.Extract(personatools.SwitchName, "use the jarvis voice"); ok {
		t.Fatal("persona.switch Extract claimed a voice request")
	}
	if _, ok := f.registry.Extract(personatools.SwitchName, "what personas do you have"); ok {
		t.Fatal("persona.switch Extract claimed a list request")
	}

	if _, err := f.execute(t, personatools.SwitchName, personatools.SwitchIn{Name: "missing"}); err == nil {
		t.Fatal("persona.switch accepted an unknown persona")
	} else if message := tool.FailureMessage(err); !strings.Contains(message, "couldn't find a persona") {
		t.Fatalf("unknown failure = %q", message)
	}

	// A stale state after creation must be refused and leave the bundle alone.
	stale := newStaleFixture(t)
	if _, err := stale.execute(t, personatools.SwitchName, personatools.SwitchIn{Name: "GLaDOS"}); err == nil {
		t.Fatal("persona.switch accepted a stale voice")
	} else if message := tool.FailureMessage(err); !strings.Contains(message, "needs re-exporting") {
		t.Fatalf("stale failure = %q", message)
	}
	if got := stale.settings.String(persona.ActiveKey); got != "" {
		t.Fatalf("persona.active = %q after a rejected switch", got)
	}
	if got := stale.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("voice.name = %q after a rejected switch", got)
	}
}

func TestPersonaListAndCurrentSpeakHonestly(t *testing.T) {
	f := newFixture(t)
	f.create(t, "GLaDOS", "jarvis")
	f.create(t, "Alfred", "alba")
	if _, err := f.execute(t, personatools.SwitchName, personatools.SwitchIn{Name: "GLaDOS"}); err != nil {
		t.Fatalf("switch: %v", err)
	}

	listResult, err := f.execute(t, personatools.ListName, personatools.ListIn{})
	if err != nil {
		t.Fatalf("persona.list: %v", err)
	}
	list := listResult.(personatools.ListOut)
	if len(list.Personas) != 2 || list.Active != "GLaDOS" {
		t.Fatalf("ListOut = %+v", list)
	}
	activeFlags := 0
	for _, entry := range list.Personas {
		if entry.Active {
			activeFlags++
			if entry.Name != "GLaDOS" {
				t.Fatalf("active flag on %q", entry.Name)
			}
		}
	}
	if activeFlags != 1 {
		t.Fatalf("active flags = %d, want one", activeFlags)
	}
	if text, _ := f.registry.Speak(personatools.ListName, list); !strings.Contains(text, "GLaDOS is active") {
		t.Fatalf("list Speak = %q", text)
	}

	currentResult, err := f.execute(t, personatools.CurrentName, personatools.CurrentIn{})
	if err != nil {
		t.Fatalf("persona.current: %v", err)
	}
	current := currentResult.(personatools.CurrentOut)
	if !current.Exact || current.Persona != "GLaDOS" {
		t.Fatalf("CurrentOut = %+v", current)
	}
	if text, _ := f.registry.Speak(personatools.CurrentName, current); text != "I'm using the GLaDOS persona." {
		t.Fatalf("exact Speak = %q", text)
	}

	// A manual voice override is reported as a difference, not the bundle.
	if _, err := f.settings.Set(f.ctx, voicelib.KeyName, "alba"); err != nil {
		t.Fatalf("override voice: %v", err)
	}
	currentResult, err = f.execute(t, personatools.CurrentName, personatools.CurrentIn{})
	if err != nil {
		t.Fatalf("persona.current after override: %v", err)
	}
	current = currentResult.(personatools.CurrentOut)
	if current.Exact || !current.VoiceDiffers || current.EffectiveVoice != "alba" {
		t.Fatalf("CurrentOut after override = %+v", current)
	}
	text, _ := f.registry.Speak(personatools.CurrentName, current)
	if !strings.Contains(text, "voice is set to alba instead of jarvis") {
		t.Fatalf("override Speak = %q", text)
	}
}

func TestConformanceMetadataPermissionsAndLocalOnly(t *testing.T) {
	f := newFixture(t)
	for name, want := range map[string]tool.Permission{
		personatools.SwitchName:  tool.PermissionAct,
		personatools.ListName:    tool.PermissionRead,
		personatools.CurrentName: tool.PermissionRead,
	} {
		info, ok := f.registry.Info(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if info.Permission != want {
			t.Errorf("%s Permission = %s, want %s", name, info.Permission, want)
		}
		if !info.LocalOnly {
			t.Errorf("%s is not Local-only", name)
		}
		if len(info.Examples) == 0 {
			t.Errorf("%s has no Examples", name)
		}
		if !info.HasSpeak {
			t.Errorf("%s has no Speak renderer", name)
		}
	}
}

// staleFixture uses a fake voice library so a ready state can become stale
// without a TTS revision change.
type staleFixture struct {
	t        *testing.T
	ctx      context.Context
	settings *settings.Service
	service  *persona.Service
	registry *tool.Registry
	voices   *staleVoices
}

type staleVoices struct {
	entries map[string]voicelib.Voice
	active  string
}

func (s *staleVoices) Get(name string) (voicelib.Voice, bool) {
	for key, voice := range s.entries {
		if strings.EqualFold(key, name) {
			return voice, true
		}
	}
	return voicelib.Voice{}, false
}

func (s *staleVoices) Active() (voicelib.Voice, bool) { return s.Get(s.active) }

func (s *staleVoices) DefaultName() string { return "alba" }

func (s *staleVoices) SpeechRef(name string) (string, error) {
	voice, ok := s.Get(name)
	if !ok {
		return "", voicelib.ErrUnknownVoice
	}
	if voice.Stale {
		return "", voicelib.ErrStaleVoice
	}
	if !voice.Ready() {
		return "", voicelib.ErrNotValidated
	}
	return voice.Name, nil
}

func newStaleFixture(t *testing.T) *staleFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	voices := &staleVoices{
		entries: map[string]voicelib.Voice{
			"alba":   {Name: "alba", Builtin: true},
			"glados": {Name: "glados", ValidatedRevision: "rev-1"},
		},
		active: "alba",
	}
	service, err := persona.New(ctx, st, voices, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("persona.New: %v", err)
	}
	defs := []settings.Definition{
		settings.String(voicelib.KeyName, "alba", settings.WithConfig("alba")),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig(wording.DefaultFiller)),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage, settings.WithConfig(wording.DefaultUnavailableMessage)),
	}
	defs = append(defs, service.Definitions()...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	service.Bind(svc)
	registry := tool.NewRegistry(time.Second)
	module := personatools.NewModule()
	module.Bind(service)
	if err := module.Register(registry, tool.Deps{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The state is ready for create, then goes stale before selection.
	if _, err := service.Create(ctx, persona.Input{
		Name: "GLaDOS", Voice: "glados", Personality: "You are GLaDOS.",
		Filler: "Processing.", Unavailable: "Offline.",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	voices.entries["glados"] = voicelib.Voice{Name: "glados", ValidatedRevision: "rev-1", Stale: true}
	return &staleFixture{t: t, ctx: ctx, settings: svc, service: service, registry: registry, voices: voices}
}

func (f *staleFixture) execute(t *testing.T, name string, args any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return f.registry.Execute(f.ctx, name, raw)
}
