package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/list"
)

// fakeRegistry is a minimal briefing.Registry for composer tests.
type fakeRegistry struct {
	tools map[string]fakeTool
}

type fakeTool struct {
	enabled   bool
	localOnly bool
	result    any
	err       error
	speak     func(any) (string, bool)
}

func (f *fakeRegistry) Info(name string) (tool.Info, bool) {
	t, ok := f.tools[name]
	if !ok {
		return tool.Info{}, false
	}
	return tool.Info{Name: name, Enabled: t.enabled, LocalOnly: t.localOnly}, true
}

func (f *fakeRegistry) Execute(_ context.Context, name string, _ json.RawMessage) (any, error) {
	t, ok := f.tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return t.result, t.err
}

func (f *fakeRegistry) Speak(name string, result any) (string, bool) {
	t, ok := f.tools[name]
	if !ok || t.speak == nil {
		return "", false
	}
	return t.speak(result)
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{tools: map[string]fakeTool{
		"weather.current": {enabled: true, localOnly: false, result: struct{}{}, speak: func(any) (string, bool) { return "weather.current", true }},
		"alarm.list":      {enabled: true, localOnly: true, result: struct{}{}, speak: func(any) (string, bool) { return "alarm.list", true }},
		"timer.list":      {enabled: true, localOnly: true, result: struct{}{}, speak: func(any) (string, bool) { return "timer.list", true }},
		"reminder.list":   {enabled: true, localOnly: true, result: struct{}{}, speak: func(any) (string, bool) { return "reminder.list", true }},
		"list.read":       {enabled: true, localOnly: true, result: list.ReadOut{}, speak: func(any) (string, bool) { return "list.read", true }},
	}}
}

func TestComposerEveryPartAvailable(t *testing.T) {
	reg := newFakeRegistry()
	c := New(reg, time.Now, nil)
	res := c.Compose(context.Background())
	if len(res.Parts) != 5 {
		t.Fatalf("parts = %d, want 5", len(res.Parts))
	}
	wantOrder := []string{"weather.current", "alarm.list", "timer.list", "reminder.list", "list.read"}
	for i, name := range wantOrder {
		if res.Parts[i].Name != name {
			t.Errorf("part %d = %q, want %q", i, res.Parts[i].Name, name)
		}
	}
	// weather is public; the four private sources are Local-only.
	if res.Parts[0].LocalOnly {
		t.Error("weather.current should not be Local-only")
	}
	for _, p := range res.Parts[1:] {
		if !p.LocalOnly {
			t.Errorf("%s should be Local-only", p.Name)
		}
	}
	// list.read is rendered by the composer, not the Tool's Speak.
	if !strings.Contains(res.Text, "Your to-do list is empty.") {
		t.Errorf("text = %q, want the to-do list rendered by the composer", res.Text)
	}
}

func TestComposerOnePartErrors(t *testing.T) {
	reg := newFakeRegistry()
	reg.tools["timer.list"] = fakeTool{enabled: true, localOnly: true, err: fmt.Errorf("boom"), speak: func(any) (string, bool) { return "timer.list", true }}
	c := New(reg, time.Now, nil)
	res := c.Compose(context.Background())
	for _, p := range res.Parts {
		if p.Name == "timer.list" {
			t.Fatalf("timer.list should have been skipped after its handler errored")
		}
	}
	if len(res.Parts) != 4 {
		t.Errorf("parts = %d, want 4", len(res.Parts))
	}
	if strings.Contains(res.Text, "timer.list") {
		t.Errorf("text = %q, the errored timer clause should not appear", res.Text)
	}
}

func TestComposerEveryPartDisabled(t *testing.T) {
	reg := newFakeRegistry()
	for name, tl := range reg.tools {
		tl.enabled = false
		reg.tools[name] = tl
	}
	c := New(reg, time.Now, nil)
	res := c.Compose(context.Background())
	if len(res.Parts) != 0 {
		t.Fatalf("parts = %d, want 0", len(res.Parts))
	}
	if res.Text != "" {
		t.Errorf("text = %q, want empty when every part is disabled", res.Text)
	}
}

func TestComposerNoLocation(t *testing.T) {
	reg := newFakeRegistry()
	// A missing location makes weather.current fail, as it does in production.
	reg.tools["weather.current"] = fakeTool{enabled: true, localOnly: false, err: fmt.Errorf("no location"), speak: func(any) (string, bool) { return "weather.current", true }}
	c := New(reg, time.Now, nil)
	res := c.Compose(context.Background())
	for _, p := range res.Parts {
		if p.Name == "weather.current" {
			t.Fatalf("weather.current should be skipped without a location")
		}
	}
	if len(res.Parts) != 4 {
		t.Errorf("parts = %d, want 4 (weather omitted)", len(res.Parts))
	}
	if strings.Contains(res.Text, "weather.current") {
		t.Errorf("text = %q, the weather clause should be omitted", res.Text)
	}
}

func TestComposerLocalOnlyPart(t *testing.T) {
	reg := newFakeRegistry()
	// Only alarm.list is Local-only; the rest are public for this case.
	reg.tools["weather.current"] = fakeTool{enabled: true, localOnly: false, result: struct{}{}, speak: func(any) (string, bool) { return "weather.current", true }}
	reg.tools["alarm.list"] = fakeTool{enabled: true, localOnly: true, result: struct{}{}, speak: func(any) (string, bool) { return "alarm.list", true }}
	reg.tools["timer.list"] = fakeTool{enabled: true, localOnly: false, result: struct{}{}, speak: func(any) (string, bool) { return "timer.list", true }}
	reg.tools["reminder.list"] = fakeTool{enabled: true, localOnly: false, result: struct{}{}, speak: func(any) (string, bool) { return "reminder.list", true }}
	reg.tools["list.read"] = fakeTool{enabled: true, localOnly: false, result: list.ReadOut{}, speak: func(any) (string, bool) { return "list.read", true }}
	c := New(reg, time.Now, nil)
	res := c.Compose(context.Background())
	byName := map[string]Part{}
	for _, p := range res.Parts {
		byName[p.Name] = p
	}
	if !byName["alarm.list"].LocalOnly {
		t.Error("alarm.list should be Local-only")
	}
	for name, p := range byName {
		if name != "alarm.list" && p.LocalOnly {
			t.Errorf("%s should not be Local-only", name)
		}
	}
}

func TestComposerRunesCapHolds(t *testing.T) {
	reg := newFakeRegistry()
	// The weather and alarm clauses are each 250 runes, so two fit under 600
	// and the large timer/reminder clauses are skipped. list.read renders a
	// short, bounded clause of its own, so it still fits.
	for name, tl := range reg.tools {
		if name == "list.read" {
			continue
		}
		n := name
		tl.speak = func(any) (string, bool) {
			return n + strings.Repeat("x", 250-len(n)), true
		}
		reg.tools[name] = tl
	}
	c := New(reg, time.Now, nil)
	c.maxRunes = 600
	res := c.Compose(context.Background())
	for _, dropped := range []string{"timer.list", "reminder.list"} {
		if strings.Contains(res.Text, dropped) {
			t.Errorf("text = %q, %s should have been dropped by the cap", res.Text, dropped)
		}
	}
	if !strings.Contains(res.Text, "Ask me for the rest.") {
		t.Errorf("text = %q, want the cap suffix", res.Text)
	}
	// The composed clauses must stay under the cap.
	joined := strings.TrimSuffix(res.Text, "Ask me for the rest.")
	if utf8.RuneCountInString(joined) >= 600 {
		t.Errorf("joined clauses are %d runes, want under 600", utf8.RuneCountInString(joined))
	}
	// The dropped clauses must not appear as parts.
	for _, p := range res.Parts {
		if p.Name == "timer.list" || p.Name == "reminder.list" {
			t.Errorf("part %q should have been dropped by the cap", p.Name)
		}
	}
}

func TestRenderTodo(t *testing.T) {
	cases := []struct {
		name string
		in   list.ReadOut
		want string
	}{
		{
			name: "empty",
			in:   list.ReadOut{Lists: []list.List{{Key: list.KeyTodo, Title: "To-do", Items: nil}}},
			want: "Your to-do list is empty.",
		},
		{
			name: "one item",
			in:   list.ReadOut{Lists: []list.List{{Key: list.KeyTodo, Items: []list.Item{{Text: "a"}}}}},
			want: "One thing on your to-do list: a.",
		},
		{
			name: "three items",
			in:   list.ReadOut{Lists: []list.List{{Key: list.KeyTodo, Items: []list.Item{{Text: "a"}, {Text: "b"}, {Text: "c"}}}}},
			want: "Three things on your to-do list: a, b and c.",
		},
		{
			name: "ten items",
			in: list.ReadOut{Lists: []list.List{{Key: list.KeyTodo, Items: []list.Item{
				{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"},
				{Text: "f"}, {Text: "g"}, {Text: "h"}, {Text: "i"}, {Text: "j"},
			}}}},
			want: "Three things on your to-do list: a, b and c, and 7 more.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderTodo(tc.in); got != tc.want {
				t.Errorf("RenderTodo = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderTodoNeverReadsShopping(t *testing.T) {
	// Only the shopping list has items: the composer must not read it.
	shoppingOnly := list.ReadOut{Lists: []list.List{{
		Key:   list.KeyShopping,
		Title: "Shopping",
		Items: []list.Item{{Text: "milk"}, {Text: "eggs"}},
	}}}
	if got := RenderTodo(shoppingOnly); got != "Your to-do list is empty." {
		t.Errorf("RenderTodo(shopping only) = %q, want the to-do list empty", got)
	}

	// When both lists have items, only the to-do items are rendered.
	both := list.ReadOut{Lists: []list.List{
		{Key: list.KeyShopping, Title: "Shopping", Items: []list.Item{{Text: "milk"}}},
		{Key: list.KeyTodo, Title: "To-do", Items: []list.Item{{Text: "call mum"}}},
	}}
	if got := RenderTodo(both); got != "One thing on your to-do list: call mum." {
		t.Errorf("RenderTodo(both) = %q, want only the to-do item", got)
	}
}

func TestComposerListReadUsesRenderTodo(t *testing.T) {
	reg := newFakeRegistry()
	// list.read returns ten to-do items; the composer must render at most three
	// itself, never the unbounded SpeakRead.
	items := make([]list.Item, 10)
	for i := range items {
		items[i] = list.Item{Text: fmt.Sprintf("task-%d", i)}
	}
	reg.tools["list.read"] = fakeTool{enabled: true, localOnly: true, result: list.ReadOut{Lists: []list.List{{Key: list.KeyTodo, Items: items}}}}
	c := New(reg, time.Now, nil)
	res := c.Compose(context.Background())
	if len(res.Parts) != 5 {
		t.Fatalf("parts = %d, want 5", len(res.Parts))
	}
	last := res.Parts[len(res.Parts)-1]
	if last.Name != "list.read" {
		t.Fatalf("last part = %q, want list.read", last.Name)
	}
	if !strings.Contains(last.Text, "and 7 more") {
		t.Errorf("list.read clause = %q, want bounded rendering with \"and 7 more\"", last.Text)
	}
	if strings.Contains(last.Text, "task-3") {
		t.Errorf("list.read clause = %q, want at most three items", last.Text)
	}
}

func TestMatches(t *testing.T) {
	c := New(newFakeRegistry(), time.Now, nil)
	cases := []struct {
		normalized string
		want       bool
	}{
		{"good morning", true},
		{"what's my day", true},
		{"what does my day look like", true},
		{"brief me", true},
		{"morning briefing", true},
		{"good morning and set a timer for five minutes", true},
		{"what does my day look like and then set a timer", true},
		{"good morning everyone", false},
		{"what's my day today", false},
		{"tell me the time", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.Matches(tc.normalized); got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.normalized, got, tc.want)
		}
	}
}
