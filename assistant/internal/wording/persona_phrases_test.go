package wording_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// TestPersonaTriggerPhrasesAddToTheGlobalControlPhrases pins #500's routing
// rule: a Persona's phrases are extra claims, so the shipped phrases keep
// working, and a phrase claimed by two owners still falls through.
func TestPersonaTriggerPhrasesAddToTheGlobalControlPhrases(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	defs := []settings.Definition{
		settings.Strings(wording.ControlPhrasesKey("conversation.start"), []string{}, settings.WithConfig([]string{"let's talk"})),
		settings.Strings(wording.ControlPhrasesKey("conversation.end"), []string{}, settings.WithConfig([]string{"that's all"})),
		settings.Strings(wording.ControlPhrasesKey("timer.set"), []string{}, settings.WithConfig([]string{"set a timer"})),
	}
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	personaPhrases := map[string][]string{
		"conversation.start": {"let's fucking talk"},
		"conversation.end":   {"that's enough"},
	}
	mgr, err := wording.NewManager(svc, wording.Setup{
		ControlTools: []string{"conversation.start", "conversation.end", "timer.set"},
	}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		wording.WithPersonaPhrases(func() map[string][]string { return personaPhrases }),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	words := mgr.Routing()
	for phrase, want := range map[string]string{
		"let's talk":         "conversation.start",
		"let's fucking talk": "conversation.start",
		"that's all":         "conversation.end",
		"that's enough":      "conversation.end",
		"set a timer":        "timer.set",
	} {
		if got := words.ControlPhrases[phrase]; got != want {
			t.Errorf("control phrase %q = %q, want %q", phrase, got, want)
		}
	}

	// A Persona phrase that collides with another Tool never removes the global
	// route: Persona validation refuses it at save time, and a later global
	// edit still leaves the shipped phrase in charge.
	personaPhrases["conversation.start"] = append(personaPhrases["conversation.start"], "set a timer")
	words = mgr.Routing()
	if got := words.ControlPhrases["set a timer"]; got != "timer.set" {
		t.Errorf("global phrase after a Persona collision = %q, want timer.set", got)
	}
	if got := words.ControlPhrases["that's enough"]; got != "conversation.end" {
		t.Errorf("unrelated Persona phrase = %q, want conversation.end", got)
	}

	// Two Persona Tools claiming one phrase are ambiguous and fall through.
	personaPhrases["conversation.end"] = append(personaPhrases["conversation.end"], "let's fucking talk")
	words = mgr.Routing()
	if got := words.ControlPhrases["let's fucking talk"]; got != "" {
		t.Errorf("phrase claimed twice by Persona Tools = %q, want no route", got)
	}

	// With no Persona source the global phrases stand alone.
	plain, err := wording.NewManager(svc, wording.Setup{
		ControlTools: []string{"conversation.start", "conversation.end", "timer.set"},
	}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager without a Persona: %v", err)
	}
	t.Cleanup(plain.Close)
	if got := plain.Routing().ControlPhrases["let's fucking talk"]; got != "" {
		t.Errorf("plain routing included a Persona phrase: %q", got)
	}
	if got := slices.Clone(plain.Routing().RequestPrefixes); len(got) != 0 {
		t.Errorf("plain routing prefixes = %v, want none", got)
	}
}
