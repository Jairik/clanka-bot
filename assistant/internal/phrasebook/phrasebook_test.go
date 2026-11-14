package phrasebook_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/phrasebook"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func newService(t *testing.T, active func() string) (*phrasebook.Service, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := phrasebook.New(context.Background(), st, active, nil)
	if err != nil {
		t.Fatalf("phrasebook.New: %v", err)
	}
	return svc, st
}

func TestDefaultsAreTerseAndOnlyActionsAreOverridable(t *testing.T) {
	// A couple of content-bearing replies must never be replaceable by "Done.";
	// the Fast path's whole point is that those carry the answer.
	for _, name := range []string{"time.now", "weather.current", "timer.list", "list.read", "note.recall", "assistant.repeat"} {
		if phrasebook.Overridable(name) {
			t.Errorf("%s is overridable, want content replies left alone", name)
		}
	}
	for _, name := range []string{"timer.set", "lights.power", "spotify.play", "note.remember", "persona.switch"} {
		text, ok := phrasebook.Default(name)
		if !ok || strings.TrimSpace(text) == "" {
			t.Fatalf("%s has no default", name)
		}
		if len(strings.Fields(text)) > 4 {
			t.Errorf("%s default %q is not terse", name, text)
		}
		if strings.ContainsAny(text, "\r\n") {
			t.Errorf("%s default is not one line", name)
		}
	}
}

func TestRenderUsesDefaultThenPersonaOverride(t *testing.T) {
	active := "Scuzz"
	svc, _ := newService(t, func() string { return active })
	ctx := context.Background()

	if got := svc.Render("timer.set", "Timer set for ten minutes."); got != "Set." {
		t.Fatalf("default Render = %q, want Set.", got)
	}
	if got := svc.Render("weather.current", "Sunny and 20 degrees."); got != "Sunny and 20 degrees." {
		t.Fatalf("content Render = %q, want passthrough", got)
	}
	if _, err := svc.Set(ctx, "Scuzz", "timer.set", "Timer's fucking set.", phrasebook.SourceManual); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := svc.Render("timer.set", "Timer set for ten minutes."); got != "Timer's fucking set." {
		t.Fatalf("override Render = %q", got)
	}
	// Another Persona does not inherit the override.
	active = "Blank"
	if got := svc.Render("timer.set", "Timer set for ten minutes."); got != "Set." {
		t.Fatalf("other persona Render = %q, want default", got)
	}
}

func TestSetReplaceDeleteAndReopen(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	svc, err := phrasebook.New(ctx, st, func() string { return "Scuzz" }, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := svc.Set(ctx, "scuzz", "timer.set", "Set, fucker.", phrasebook.SourceManual); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := svc.Set(ctx, "Scuzz", "lights.power", "Light's on.", phrasebook.SourceManual); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	if got := len(svc.Entries("Scuzz")); got != 2 {
		t.Fatalf("Entries = %d, want 2 (case-insensitive persona)", got)
	}
	if err := svc.Delete(ctx, "Scuzz", "lights.power"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := len(svc.Entries("Scuzz")); got != 1 {
		t.Fatalf("Entries after delete = %d, want 1", got)
	}
	replaced, err := svc.Replace(ctx, "Scuzz", map[string]string{
		"timer.set":    "Done.",
		"lights.power": "Lit.",
		"lights.color": "Painted.",
	}, phrasebook.SourceLLM)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(replaced) != 3 {
		t.Fatalf("Replace returned %d entries, want 3", len(replaced))
	}
	for _, entry := range replaced {
		if entry.Source != phrasebook.SourceLLM {
			t.Errorf("%s source = %q, want agent", entry.Tool, entry.Source)
		}
	}
	// Reopening the same database keeps the set.
	reopened, err := phrasebook.New(ctx, st, func() string { return "Scuzz" }, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(reopened.Entries("Scuzz")); got != 3 {
		t.Fatalf("reopened entries = %d, want 3", got)
	}
}

func TestSetRejectsUnknownToolAndBadText(t *testing.T) {
	svc, _ := newService(t, func() string { return "Scuzz" })
	ctx := context.Background()
	if _, err := svc.Set(ctx, "Scuzz", "weather.current", "Sunny.", phrasebook.SourceManual); !errors.Is(err, phrasebook.ErrUnknownTool) {
		t.Fatalf("unknown tool err = %v, want ErrUnknownTool", err)
	}
	if _, err := svc.Set(ctx, "Scuzz", "timer.set", "  ", phrasebook.SourceManual); !errors.Is(err, phrasebook.ErrInvalidPhrase) {
		t.Fatalf("empty err = %v, want ErrInvalidPhrase", err)
	}
	long := strings.Repeat("a", phrasebook.MaxPhraseRunes+1)
	if _, err := svc.Set(ctx, "Scuzz", "timer.set", long, phrasebook.SourceManual); !errors.Is(err, phrasebook.ErrInvalidPhrase) {
		t.Fatalf("long err = %v, want ErrInvalidPhrase", err)
	}
	if _, err := svc.Set(ctx, "Scuzz", "timer.set", "one\ntwo", phrasebook.SourceManual); !errors.Is(err, phrasebook.ErrInvalidPhrase) {
		t.Fatalf("newline err = %v, want ErrInvalidPhrase", err)
	}
}

type fakeCompleter struct {
	reply string
	err   error
	got   string
}

func (f *fakeCompleter) Complete(_ context.Context, _, user string) (string, error) {
	f.got = user
	return f.reply, f.err
}

func TestGenerateParsesFencedJSONAndFilters(t *testing.T) {
	slots := []phrasebook.Slot{
		{Tool: "timer.set", Group: "timer", Description: "Set a timer.", Default: "Set."},
		{Tool: "lights.power", Group: "lights", Description: "Turn a light on or off.", Default: "Done."},
	}
	fake := &fakeCompleter{reply: "```json\n{\"timer.set\":\"Timer's fucking set.\",\"lights.power\":\"Lit, bitch.\",\"weather.current\":\"Sunny.\",\"list.add\":\"\"}\n```"}
	got, err := phrasebook.Generate(context.Background(), "Scuzz", "Swears constantly.", slots, fake)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Generate = %v, want only the two known tools", got)
	}
	if got["timer.set"] != "Timer's fucking set." || got["lights.power"] != "Lit, bitch." {
		t.Fatalf("Generate values = %v", got)
	}
	if !strings.Contains(fake.got, "Swears constantly.") || !strings.Contains(fake.got, "timer.set") {
		t.Fatalf("prompt missing persona or slots: %s", fake.got)
	}
}

func TestGenerateErrors(t *testing.T) {
	slots := []phrasebook.Slot{{Tool: "timer.set", Description: "Set a timer.", Default: "Set."}}
	if _, err := phrasebook.Generate(context.Background(), "Scuzz", "x", slots, nil); !errors.Is(err, phrasebook.ErrNoCompleter) {
		t.Fatalf("nil completer err = %v", err)
	}
	fake := &fakeCompleter{reply: "not json"}
	if _, err := phrasebook.Generate(context.Background(), "Scuzz", "x", slots, fake); err == nil {
		t.Fatal("bad JSON succeeded, want error")
	}
	fake = &fakeCompleter{reply: `{"weather.current":"Sunny."}`}
	if _, err := phrasebook.Generate(context.Background(), "Scuzz", "x", slots, fake); err == nil {
		t.Fatal("no usable phrases succeeded, want error")
	}
	fake = &fakeCompleter{err: errors.New("provider down")}
	if _, err := phrasebook.Generate(context.Background(), "Scuzz", "x", slots, fake); err == nil {
		t.Fatal("provider error succeeded, want error")
	}
}

type fakeInfo struct{ info map[string]tool.Info }

func (f fakeInfo) Info(name string) (tool.Info, bool) {
	info, ok := f.info[name]
	return info, ok
}

func TestSlotsUsesLiveToolMetadata(t *testing.T) {
	src := fakeInfo{info: map[string]tool.Info{
		"timer.set":    {Name: "timer.set", Description: "Set a timer.", HasSpeak: true},
		"lights.power": {Name: "lights.power", Description: "Power a light.", HasSpeak: true},
		"time.now":     {Name: "time.now", Description: "Tell the time.", HasSpeak: true},
		"timer.list":   {Name: "timer.list", Description: "List timers.", HasSpeak: true},
		"note.forget":  {Name: "note.forget", Description: "Forget a note.", HasSpeak: false},
	}}
	slots := phrasebook.Slots(src)
	if len(slots) != 2 {
		t.Fatalf("Slots = %d, want 2: %+v", len(slots), slots)
	}
	for _, slot := range slots {
		if slot.Group == "" || slot.Default == "" {
			t.Errorf("slot %+v is incomplete", slot)
		}
	}
}

func TestRenderHasNoPersonaFallback(t *testing.T) {
	svc, _ := newService(t, func() string { return "" })
	if got := svc.Render("timer.set", "Timer set."); got != "Set." {
		t.Fatalf("no-persona Render = %q, want default", got)
	}
	// A nil active resolver is also safe (the conformance wiring may build the
	// service before settings exist).
	plain, err := phrasebook.New(context.Background(), mustStore(t), nil, nil)
	if err != nil {
		t.Fatalf("New nil active: %v", err)
	}
	if got := plain.Render("timer.set", "Timer set."); got != "Set." {
		t.Fatalf("nil-active Render = %q, want default", got)
	}
}

func mustStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
