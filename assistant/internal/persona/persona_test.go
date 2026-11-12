package persona_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// fakeVoices is a deterministic voice library for readiness transitions that a
// real rescan can only produce with a TTS revision change.
type fakeVoices struct {
	entries map[string]voicelib.Voice
	active  string
}

func (f *fakeVoices) Get(name string) (voicelib.Voice, bool) {
	for key, voice := range f.entries {
		if strings.EqualFold(key, strings.TrimSpace(name)) {
			return voice, true
		}
	}
	return voicelib.Voice{}, false
}

func (f *fakeVoices) Active() (voicelib.Voice, bool) {
	if f.active != "" {
		return f.Get(f.active)
	}
	for _, voice := range f.entries {
		if voice.Ready() {
			return voice, true
		}
	}
	return voicelib.Voice{}, false
}

func (f *fakeVoices) DefaultName() string { return "alba" }

func (f *fakeVoices) SpeechRef(name string) (string, error) {
	voice, ok := f.Get(name)
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

// readyFake has one built-in voice and one selectable custom state.
func readyFake() *fakeVoices {
	return &fakeVoices{
		entries: map[string]voicelib.Voice{
			"alba":   {Name: "alba", Builtin: true},
			"glados": {Name: "glados", ValidatedRevision: "rev-1"},
		},
		active: "alba",
	}
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	voices   *fakeVoices
	settings *settings.Service
	service  *persona.Service
	path     string
}

func newFixture(t *testing.T, voices *fakeVoices) *fixture {
	t.Helper()
	if voices == nil {
		voices = readyFake()
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	personas, err := persona.New(ctx, st, voices, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("persona.New: %v", err)
	}
	defs := []settings.Definition{
		settings.String(voicelib.KeyName, "alba", settings.WithConfig("alba")),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig(wording.DefaultFiller)),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage, settings.WithConfig(wording.DefaultUnavailableMessage)),
	}
	defs = append(defs, personas.Definitions()...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	personas.Bind(svc)
	return &fixture{t: t, ctx: ctx, st: st, voices: voices, settings: svc, service: personas, path: path}
}

func input(name string) persona.Input {
	return persona.Input{
		Name:        name,
		Voice:       "glados",
		Personality: "You are GLaDOS: precise, dry and quietly menacing.",
		Filler:      "Processing.",
		Unavailable: "The testing chamber is offline.",
	}
}

func TestCreateListUpdateAndReopenPreserveEveryPersona(t *testing.T) {
	f := newFixture(t, nil)
	glados, err := f.service.Create(f.ctx, input("GLaDOS"))
	if err != nil {
		t.Fatalf("Create GLaDOS: %v", err)
	}
	if glados.Name != "GLaDOS" || glados.Voice != "glados" || glados.CreatedAt.IsZero() {
		t.Fatalf("created persona = %+v", glados)
	}
	alfredInput := input("Alfred")
	alfredInput.Voice = "alba"
	alfredInput.Personality = "You are a calm British butler."
	if _, err := f.service.Create(f.ctx, alfredInput); err != nil {
		t.Fatalf("Create Alfred: %v", err)
	}
	if got := f.service.Names(); len(got) != 2 || got[0] != "Alfred" || got[1] != "GLaDOS" {
		t.Fatalf("Names = %v, want case-insensitive order", got)
	}
	if _, err := f.service.Create(f.ctx, input("glados")); !errors.Is(err, persona.ErrPersonaExists) {
		t.Fatalf("duplicate Create error = %v, want ErrPersonaExists", err)
	}

	updatedInput := input("GLaDOS")
	updatedInput.Voice = "alba"
	updatedInput.Personality = "You are GLaDOS, now polite."
	updated, err := f.service.Update(f.ctx, "glados", updatedInput)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "GLaDOS" || updated.Voice != "alba" || !strings.Contains(updated.Personality, "polite") {
		t.Fatalf("updated persona = %+v", updated)
	}

	// A new store handle proves the record survives a restart, not just the
	// in-memory snapshot.
	if err := f.st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	reopened, err := store.Open(f.ctx, f.path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	service, err := persona.New(f.ctx, reopened, f.voices, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("persona.New after reopen: %v", err)
	}
	list := service.List()
	if len(list) != 2 {
		t.Fatalf("personas after reopen = %+v, want two", list)
	}
	got, ok := service.Get("GLADOS")
	if !ok || got.Voice != "alba" || got.Personality != updated.Personality || got.Filler != updated.Filler || got.Unavailable != updated.Unavailable {
		t.Fatalf("GLaDOS after reopen = %+v, %v", got, ok)
	}
}

func TestCreateRejectsInvalidFieldsAndUnreadyVoices(t *testing.T) {
	f := newFixture(t, nil)
	longName := strings.Repeat("a", persona.MaxNameRunes+1)
	cases := []struct {
		name  string
		input persona.Input
		want  error
	}{
		{"blank name", func() persona.Input { in := input(" "); return in }(), persona.ErrInvalidPersona},
		{"long name", func() persona.Input { in := input(longName); return in }(), persona.ErrInvalidPersona},
		{"slash name", func() persona.Input { in := input("bad/name"); return in }(), persona.ErrInvalidPersona},
		{"blank voice", func() persona.Input { in := input("X"); in.Voice = " "; return in }(), persona.ErrInvalidPersona},
		{"unknown voice", func() persona.Input { in := input("X"); in.Voice = "nobody"; return in }(), persona.ErrVoiceUnavailable},
		{"blank personality", func() persona.Input { in := input("X"); in.Personality = " "; return in }(), persona.ErrInvalidPersona},
		{"long personality", func() persona.Input {
			in := input("X")
			in.Personality = strings.Repeat("p", persona.MaxPersonalityRunes+1)
			return in
		}(), persona.ErrInvalidPersona},
		{"blank filler", func() persona.Input { in := input("X"); in.Filler = " "; return in }(), persona.ErrInvalidPersona},
		{"multiline filler", func() persona.Input { in := input("X"); in.Filler = "one\ntwo"; return in }(), persona.ErrInvalidPersona},
		{"blank unavailable", func() persona.Input { in := input("X"); in.Unavailable = ""; return in }(), persona.ErrInvalidPersona},
		{"long unavailable", func() persona.Input {
			in := input("X")
			in.Unavailable = strings.Repeat("u", persona.MaxWordingRunes+1)
			return in
		}(), persona.ErrInvalidPersona},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.service.Create(f.ctx, tc.input)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Create error = %v, want %v", err, tc.want)
			}
			if message := persona.Message(err); message == "" {
				t.Fatal("a rejected create has no user-safe message")
			}
			if len(f.service.List()) != 0 {
				t.Fatalf("a rejected create left rows: %+v", f.service.List())
			}
		})
	}

	// A voice that has gone stale is refused before any row is written.
	stale := readyFake()
	stale.entries["glados"] = voicelib.Voice{Name: "glados", ValidatedRevision: "rev-1", Stale: true}
	staleFixture := newFixture(t, stale)
	if _, err := staleFixture.service.Create(staleFixture.ctx, input("GLaDOS")); !errors.Is(err, persona.ErrVoiceUnavailable) {
		t.Fatalf("stale voice Create error = %v, want ErrVoiceUnavailable", err)
	}
}

func TestSelectAppliesOneAtomicBundle(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.service.Create(f.ctx, input("GLaDOS")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var observed []string
	f.settings.SetObserver(func(keys []string) { observed = append(observed, keys...) })
	selected, err := f.service.Select(f.ctx, "glados")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selected.Name != "GLaDOS" {
		t.Fatalf("Select returned %+v", selected)
	}
	// The fake's Active reads the persistent selection; production voicelib
	// resolves it from the same settings key that SetMany just wrote.
	f.voices.active = "glados"
	values := f.settings.GetMany(voicelib.KeyName, wording.FillerKey, wording.UnavailableKey, persona.ActiveKey)
	for key, want := range map[string]string{
		voicelib.KeyName:       "glados",
		wording.FillerKey:      "Processing.",
		wording.UnavailableKey: "The testing chamber is offline.",
		persona.ActiveKey:      "GLaDOS",
	} {
		if got := values[key].Value; got != want {
			t.Errorf("%s = %v, want %q", key, got, want)
		}
		if values[key].Source != settings.SourceOverride {
			t.Errorf("%s source = %s, want override", key, values[key].Source)
		}
	}
	// One observer call with the four keys proves one committed batch, not
	// four independent writes.
	if len(observed) != 4 {
		t.Fatalf("observed keys = %v, want one four-key notification", observed)
	}

	current, ok := f.service.Current()
	if !ok || current.Persona.Name != "GLaDOS" || !current.Exact {
		t.Fatalf("Current after select = %+v, %v", current, ok)
	}
}

func TestSelectStaleVoiceLeavesEverySettingUntouched(t *testing.T) {
	voices := readyFake()
	f := newFixture(t, voices)
	if _, err := f.service.Create(f.ctx, input("GLaDOS")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The stored Persona is valid, but the voice state goes stale after the
	// create. Selection must fail before SetMany writes anything.
	voices.entries["glados"] = voicelib.Voice{Name: "glados", ValidatedRevision: "rev-1", Stale: true}
	var observed []string
	f.settings.SetObserver(func(keys []string) { observed = append(observed, keys...) })

	if _, err := f.service.Select(f.ctx, "GLaDOS"); !errors.Is(err, persona.ErrVoiceUnavailable) {
		t.Fatalf("Select error = %v, want ErrVoiceUnavailable", err)
	}
	values := f.settings.GetMany(voicelib.KeyName, wording.FillerKey, wording.UnavailableKey, persona.ActiveKey)
	for key, want := range map[string]any{
		voicelib.KeyName:       "alba",
		wording.FillerKey:      wording.DefaultFiller,
		wording.UnavailableKey: wording.DefaultUnavailableMessage,
		persona.ActiveKey:      "",
	} {
		if got := values[key].Value; got != want {
			t.Errorf("%s = %v, want %v after a rejected select", key, got, want)
		}
		if values[key].Source == settings.SourceOverride {
			t.Errorf("%s was overridden by a rejected select", key)
		}
	}
	if len(observed) != 0 {
		t.Fatalf("a rejected select notified observers: %v", observed)
	}
	if _, ok := f.service.Current(); ok {
		t.Fatal("Current reports an active persona after a rejected select")
	}

	// The same atomicity holds when the voice is removed entirely.
	delete(voices.entries, "glados")
	if _, err := f.service.Select(f.ctx, "GLaDOS"); !errors.Is(err, persona.ErrVoiceUnavailable) {
		t.Fatalf("Select after removal error = %v, want ErrVoiceUnavailable", err)
	}
}

func TestCurrentReportsManualOverridesHonestly(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.service.Create(f.ctx, input("GLaDOS")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.service.Select(f.ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}

	f.voices.active = "alba"
	if _, err := f.settings.Set(f.ctx, voicelib.KeyName, "alba"); err != nil {
		t.Fatalf("override voice: %v", err)
	}
	current, ok := f.service.Current()
	if !ok {
		t.Fatal("Current lost the active persona")
	}
	if !current.VoiceDiffers || current.Exact || current.EffectiveVoice != "alba" {
		t.Fatalf("Current after voice override = %+v, want an honest voice difference", current)
	}
	if current.FillerDiffers || current.UnavailableDiffers {
		t.Fatalf("Current invented unrelated differences: %+v", current)
	}

	if _, err := f.settings.Reset(f.ctx, voicelib.KeyName); err != nil {
		t.Fatalf("reset voice: %v", err)
	}
	if _, err := f.settings.Set(f.ctx, wording.FillerKey, "Manual filler."); err != nil {
		t.Fatalf("override filler: %v", err)
	}
	current, ok = f.service.Current()
	if !ok || !current.FillerDiffers || current.Exact {
		t.Fatalf("Current after filler override = %+v, want a filler difference", current)
	}

	// Selecting again applies the bundle and clears every difference.
	if _, err := f.service.Select(f.ctx, "GLaDOS"); err != nil {
		t.Fatalf("re-select: %v", err)
	}
	f.voices.active = "glados"
	if current, _ := f.service.Current(); !current.Exact {
		t.Fatalf("Current after re-select = %+v, want the exact bundle", current)
	}
}

func TestPersonalityFollowsTheActiveRecordAndClearKeepsSettings(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.service.Personality(); got.Name != "" || got.Prompt != "" {
		t.Fatalf("Personality before select = %+v", got)
	}
	if _, err := f.service.Create(f.ctx, input("GLaDOS")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.service.Select(f.ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	got := f.service.Personality()
	if got.Name != "GLaDOS" || !strings.Contains(got.Prompt, "GLaDOS") {
		t.Fatalf("Personality = %+v", got)
	}

	edited := input("GLaDOS")
	edited.Personality = "You are GLaDOS, now entirely sincere."
	if _, err := f.service.Update(f.ctx, "GLaDOS", edited); err != nil {
		t.Fatalf("Update active persona: %v", err)
	}
	if got := f.service.Personality(); !strings.Contains(got.Prompt, "entirely sincere") {
		t.Fatalf("Personality after edit = %+v, want the stored prompt", got)
	}

	if err := f.service.ClearActive(f.ctx); err != nil {
		t.Fatalf("ClearActive: %v", err)
	}
	if got := f.service.Personality(); got.Name != "" {
		t.Fatalf("Personality after clear = %+v", got)
	}
	if got := f.settings.String(voicelib.KeyName); got != "glados" {
		t.Fatalf("voice after clear = %q, want the applied voice kept", got)
	}
	if got := f.settings.String(wording.FillerKey); got != "Processing." {
		t.Fatalf("filler after clear = %q, want the applied wording kept", got)
	}
}

func TestActiveVoiceRefReportsNoPersonaAndUnreadyVoice(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.service.ActiveVoiceRef(); !errors.Is(err, persona.ErrNoActivePersona) {
		t.Fatalf("ActiveVoiceRef without a persona = %v", err)
	}
	if _, err := f.service.Create(f.ctx, input("GLaDOS")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.service.Select(f.ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	ref, err := f.service.ActiveVoiceRef()
	if err != nil || ref != "glados" {
		t.Fatalf("ActiveVoiceRef = %q, %v", ref, err)
	}
	f.voices.entries["glados"] = voicelib.Voice{Name: "glados", Stale: true}
	if _, err := f.service.ActiveVoiceRef(); !errors.Is(err, persona.ErrVoiceUnavailable) {
		t.Fatalf("ActiveVoiceRef with a stale voice = %v, want ErrVoiceUnavailable", err)
	}
}

func TestActiveMarkerResetLeavesTheAppliedBundle(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.service.Create(f.ctx, input("GLaDOS")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.service.Select(f.ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	reset, err := f.settings.Reset(f.ctx, persona.ActiveKey)
	if err != nil {
		t.Fatalf("reset active marker: %v", err)
	}
	if reset.Value != "" || reset.Source != settings.SourceDefault {
		t.Fatalf("active marker after reset = %+v", reset)
	}
	if _, ok := f.service.Current(); ok {
		t.Fatal("Current still reports a persona after the marker was reset")
	}
	if f.settings.String(voicelib.KeyName) != "glados" || f.settings.String(wording.FillerKey) != "Processing." {
		t.Fatal("resetting the marker changed the applied bundle")
	}
}

func TestTimestampsSurviveTheStore(t *testing.T) {
	f := newFixture(t, nil)
	p, err := f.service.Create(f.ctx, input("GLaDOS"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	updated, err := f.service.Update(f.ctx, "GLaDOS", input("GLaDOS"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated.UpdatedAt.After(p.CreatedAt) {
		t.Fatalf("updated_at = %s, want after created_at %s", updated.UpdatedAt, p.CreatedAt)
	}
}
