package conversation_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	convcore "github.com/Jairik/clanka-bot/assistant/internal/conversation"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
	"github.com/Jairik/clanka-bot/assistant/tools/conversation"
)

type fakeMode struct {
	started int
	ended   int
}

func (f *fakeMode) Start() { f.started++ }
func (f *fakeMode) End()   { f.ended++ }

func TestStartAndEndDriveTheMode(t *testing.T) {
	registry := tool.NewRegistry(time.Second)
	mode := &fakeMode{}
	if err := conversation.NewModule().Register(registry, tool.Deps{Conversation: mode}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := registry.Execute(context.Background(), conversation.StartName, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("start: %v", err)
	}
	if mode.started != 1 || mode.ended != 0 {
		t.Fatalf("mode after start = %+v, want one start and no end", mode)
	}
	if _, err := registry.Execute(context.Background(), conversation.EndName, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("end: %v", err)
	}
	if mode.started != 1 || mode.ended != 1 {
		t.Fatalf("mode after end = %+v, want one start and one end", mode)
	}
}

func TestSpeakMatchesTheShippedPhrases(t *testing.T) {
	registry := tool.NewRegistry(time.Second)
	if err := conversation.NewModule().Register(registry, tool.Deps{Conversation: &fakeMode{}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if text, ok := registry.Speak(conversation.StartName, conversation.StartOut{Text: "Let's talk."}); !ok || text != "Let's talk." {
		t.Errorf("start Speak = %q, %v, want %q", text, ok, "Let's talk.")
	}
	if text, ok := registry.Speak(conversation.EndName, conversation.EndOut{Text: "Okay."}); !ok || text != "Okay." {
		t.Errorf("end Speak = %q, %v, want %q", text, ok, "Okay.")
	}
}

func TestMissingModeFailsClearly(t *testing.T) {
	registry := tool.NewRegistry(time.Second)
	if err := conversation.NewModule().Register(registry, tool.Deps{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.Execute(context.Background(), conversation.StartName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("start with no latch succeeded, want a clear failure")
	}
}

// TestWordingDefinesLiveTogglePhrases pins the Dashboard surface for #500: both
// toggle phrases are ordinary live Control phrase lists, and the cue words are
// a live wording setting.
func TestWordingDefinesLiveTogglePhrases(t *testing.T) {
	registry := tool.NewRegistry(time.Second)
	if err := conversation.NewModule().Register(registry, tool.Deps{Conversation: &fakeMode{}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	setup, err := wording.Build(registry, wording.Config{
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	})
	if err != nil {
		t.Fatalf("wording.Build: %v", err)
	}
	defs := make(map[string]settings.Definition, len(setup.Definitions))
	for _, def := range setup.Definitions {
		defs[def.Key] = def
	}
	for _, key := range []string{
		wording.ControlPhrasesKey(conversation.StartName),
		wording.ControlPhrasesKey(conversation.EndName),
		wording.ExamplesKey(conversation.StartName),
		wording.ExamplesKey(conversation.EndName),
	} {
		if _, ok := defs[key]; !ok {
			t.Errorf("wording definitions lack %s", key)
		}
	}
	if !slices.Contains(setup.ControlTools, conversation.StartName) || !slices.Contains(setup.ControlTools, conversation.EndName) {
		t.Errorf("control tools = %v, want both conversation Tools", setup.ControlTools)
	}

	cue := make(map[string]settings.Definition)
	for _, def := range convcore.Definitions() {
		cue[def.Key] = def
	}
	if _, ok := cue[convcore.CueWordsKey]; !ok {
		t.Errorf("conversation definitions lack %s", convcore.CueWordsKey)
	}
}
