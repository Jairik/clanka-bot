package conversation_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
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
