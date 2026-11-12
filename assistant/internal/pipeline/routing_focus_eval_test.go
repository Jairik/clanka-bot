package pipeline_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// evalFocusCapabilities mirrors the production holder declarations. A ringing
// alert handles stop and cancel; the current reply handles stop; music handles
// the transport controls. A holder that does not declare an intent must be
// skipped, not offered it.
var evalFocusCapabilities = map[string][]focus.Intent{
	"ring":  {focus.Stop, focus.Cancel},
	"reply": {focus.Stop},
	"music": {focus.Stop, focus.Pause, focus.Resume, focus.Next},
}

// evalFocusOrder is the fixed priority the case oracle checks offline.
var evalFocusOrder = []string{"ring", "reply", "music"}

func evalFocusPriority(name string) focus.Priority {
	switch name {
	case "ring":
		return focus.Ring
	case "reply":
		return focus.Reply
	default:
		return focus.Music
	}
}

func containsIntent(intents []focus.Intent, want focus.Intent) bool {
	for _, intent := range intents {
		if intent == want {
			return true
		}
	}
	return false
}

// evalFocusHolder records the intents a case delivers to one priority.
type evalFocusHolder struct {
	name string

	mu    sync.Mutex
	calls []focus.Intent
}

func (h *evalFocusHolder) Intents() []focus.Intent { return evalFocusCapabilities[h.name] }

func (h *evalFocusHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	if !containsIntent(evalFocusCapabilities[h.name], intent) {
		return "", false
	}
	h.mu.Lock()
	h.calls = append(h.calls, intent)
	h.mu.Unlock()
	return h.name + ":" + string(intent), true
}

func (h *evalFocusHolder) seen() []focus.Intent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]focus.Intent(nil), h.calls...)
}

// TestRoutingEvalFocusFixtureShape keeps the Focus oracle honest without the
// embed service: every case is a real Control phrase mapped to its intent, the
// holders and handled_by agree with the fixed priority and capabilities, and at
// least one word appears under more than one holder.
func TestRoutingEvalFocusFixtureShape(t *testing.T) {
	fixture := loadRoutingFixture(t)
	if len(fixture.Focus) < 6 {
		t.Fatalf("fixture has %d Focus cases, want at least 6", len(fixture.Focus))
	}
	cfg := loadRoutingConfig(t)
	phrases := make(map[string]string, len(cfg.ControlPhrases))
	for phrase, target := range cfg.ControlPhrases {
		phrases[pipeline.NormalizePhrase(phrase)] = target
	}

	holderSets := make(map[string]map[string]bool)
	holdersSeen := make(map[string]bool)
	delivered := 0
	fellThrough := 0
	for i, tc := range fixture.Focus {
		if strings.TrimSpace(tc.Utterance) == "" {
			t.Errorf("focus[%d] has an empty utterance", i)
			continue
		}
		seen := make(map[string]bool, len(tc.Holders))
		for _, name := range tc.Holders {
			if _, ok := evalFocusCapabilities[name]; !ok {
				t.Errorf("focus[%d] %q names unknown holder %q", i, tc.Utterance, name)
			}
			holdersSeen[name] = true
			if seen[name] {
				t.Errorf("focus[%d] %q repeats holder %q", i, tc.Utterance, name)
			}
			seen[name] = true
		}

		// The case oracle is the priority walk: the first present holder that
		// declares the intent must take it, and no holder may take it otherwise.
		intent, intentOK := focus.ParseIntent(tc.Intent)
		if !intentOK {
			t.Errorf("focus[%d] %q has unknown intent %q", i, tc.Utterance, tc.Intent)
		}
		want := ""
		for _, name := range evalFocusOrder {
			if seen[name] && containsIntent(evalFocusCapabilities[name], intent) {
				want = name
				break
			}
		}
		if tc.HandledBy != want {
			t.Errorf("focus[%d] %q handled_by = %q, want %q", i, tc.Utterance, tc.HandledBy, want)
		}
		if want == "" {
			fellThrough++
		} else {
			delivered++
		}

		// Every case must be an exact Control phrase for its intent; otherwise
		// the real evaluation would measure the semantic router instead.
		target, ok := phrases[pipeline.NormalizePhrase(tc.Utterance)]
		if !ok {
			t.Errorf("focus[%d] %q is not a Control phrase in config.yaml", i, tc.Utterance)
			continue
		}
		mapped, isIntent := focus.ParseIntent(target)
		if !focus.IsIntentTarget(target) || !isIntent {
			t.Errorf("focus[%d] %q maps to %q, want a Focus intent", i, tc.Utterance, target)
			continue
		}
		if mapped != intent {
			t.Errorf("focus[%d] %q maps to intent %q, want %q", i, tc.Utterance, mapped, tc.Intent)
		}

		signature := strings.Join(tc.Holders, ",")
		if holderSets[tc.Utterance] == nil {
			holderSets[tc.Utterance] = make(map[string]bool)
		}
		holderSets[tc.Utterance][signature] = true
	}

	sameWord := false
	for _, sets := range holderSets {
		if len(sets) >= 2 {
			sameWord = true
			break
		}
	}
	if !sameWord {
		t.Error("fixture has no utterance under two different Focus holders")
	}
	for name := range evalFocusCapabilities {
		if !holdersSeen[name] {
			t.Errorf("fixture never puts %q in Focus", name)
		}
	}
	if delivered == 0 || fellThrough == 0 {
		t.Errorf("fixture has %d delivered and %d fall-through cases, want both", delivered, fellThrough)
	}
}

// runFocusEvalCases runs every Focus case against the real pipeline and the
// registry the main corpus used (which has no holders). It returns how many
// cases were delivered and how many fell through.
func runFocusEvalCases(t *testing.T, p *pipeline.Pipeline, registry *focus.Registry, cases []routingFocusCase, classify func(string) evalVerdict) (delivered, fellThrough int) {
	t.Helper()
	for _, tc := range cases {
		holders := make(map[string]*evalFocusHolder, len(tc.Holders))
		var claims []*focus.Claim
		for _, name := range tc.Holders {
			holder := &evalFocusHolder{name: name}
			holders[name] = holder
			claims = append(claims, registry.Claim(evalFocusPriority(name), holder))
		}

		got := classify(tc.Utterance)
		for _, claim := range claims {
			claim.Release()
		}

		if tc.HandledBy == "" {
			fellThrough++
			for name, holder := range holders {
				if calls := holder.seen(); len(calls) != 0 {
					t.Errorf("FOCUS %q with %s held delivered %v, want a fall-through", tc.Utterance, name, calls)
				}
			}
			if got.Path == tool.PathFocusIntent {
				t.Errorf("FOCUS %q with no capable holder took the Focus path", tc.Utterance)
			}
			t.Logf("ROUTING_EVAL_FOCUS utterance=%q holders=%v fallthrough path=%s tool=%s", tc.Utterance, tc.Holders, got.Path, got.Tool)
			continue
		}

		delivered++
		if got.Path != tool.PathFocusIntent {
			t.Errorf("FOCUS %q with %v took path %s, want %s", tc.Utterance, tc.Holders, got.Path, tool.PathFocusIntent)
		}
		for name, holder := range holders {
			calls := holder.seen()
			if name == tc.HandledBy {
				if len(calls) != 1 || string(calls[0]) != tc.Intent {
					t.Errorf("FOCUS %q: %s saw %v, want one %s", tc.Utterance, name, calls, tc.Intent)
				}
				continue
			}
			if len(calls) != 0 {
				t.Errorf("FOCUS %q: %s saw %v, want only %s to handle it", tc.Utterance, name, calls, tc.HandledBy)
			}
		}
		t.Logf("ROUTING_EVAL_FOCUS utterance=%q holders=%v intent=%s handled_by=%s path=%s", tc.Utterance, tc.Holders, tc.Intent, tc.HandledBy, got.Path)
	}
	return delivered, fellThrough
}
