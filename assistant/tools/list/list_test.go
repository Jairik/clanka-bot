package list

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// fakeEmbedder gives the name matcher one explicit vector table.
type fakeEmbedder struct {
	table map[string][]float32
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = f.table[text]
	}
	return vectors, nil
}

func openListStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testManager(t *testing.T, st *store.Store, embed *fakeEmbedder) *Manager {
	t.Helper()
	var names *namematch.Matcher
	if embed != nil {
		names = namematch.New(embed, namematch.Config{Threshold: func() float64 { return 0.60 }})
	}
	return NewManager(Options{
		DB:     st,
		Names:  names,
		Now:    func() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: Config{MaxItems: 3},
	})
}

func TestAddReadTickAndRemoveItems(t *testing.T) {
	ctx := context.Background()
	m := testManager(t, openListStore(t), nil)

	milk, added, err := m.Add(ctx, KeyShopping, "milk")
	if err != nil || !added {
		t.Fatalf("Add milk = %+v, %v, %v", milk, added, err)
	}
	if _, added, err := m.Add(ctx, KeyShopping, "  MILK "); err != nil || added {
		t.Fatalf("duplicate Add added = %v, %v; want false, nil", added, err)
	}
	if _, _, err := m.Add(ctx, KeyTodo, "call the dentist"); err != nil {
		t.Fatalf("Add todo: %v", err)
	}

	lists, err := m.Read(ctx, "")
	if err != nil {
		t.Fatalf("Read both: %v", err)
	}
	if len(lists) != 2 || len(lists[0].Items) != 1 || len(lists[1].Items) != 1 {
		t.Fatalf("Read = %+v", lists)
	}

	if _, err := m.SetDone(ctx, KeyShopping, milk.ID, true); err != nil {
		t.Fatalf("SetDone: %v", err)
	}
	shopping, err := m.Read(ctx, KeyShopping)
	if err != nil {
		t.Fatalf("Read shopping: %v", err)
	}
	if len(shopping[0].Items) != 0 {
		t.Fatalf("a done item is still read out: %+v", shopping[0].Items)
	}
	all, err := m.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all[0].Items) != 1 || !all[0].Items[0].Done {
		t.Fatalf("All did not keep the done row: %+v", all[0].Items)
	}

	if _, err := m.UpdateItem(ctx, KeyShopping, milk.ID, "oat milk"); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if removed, err := m.Remove(ctx, KeyShopping, "oat milk"); err != nil || removed.Text != "oat milk" {
		t.Fatalf("Remove = %+v, %v", removed, err)
	}
	if _, err := m.Remove(ctx, KeyShopping, "oat milk"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("second Remove = %v, want ErrItemNotFound", err)
	}
}

func TestAddEnforcesTheItemCap(t *testing.T) {
	ctx := context.Background()
	m := testManager(t, openListStore(t), nil)
	for _, item := range []string{"one", "two", "three"} {
		if _, _, err := m.Add(ctx, KeyShopping, item); err != nil {
			t.Fatalf("Add(%q): %v", item, err)
		}
	}
	var spoken *Error
	if _, _, err := m.Add(ctx, KeyShopping, "four"); !errors.As(err, &spoken) {
		t.Fatalf("Add past cap = %v, want a spoken Error", err)
	}
}

func TestRemoveUsesTheSharedNameMatcher(t *testing.T) {
	ctx := context.Background()
	embed := &fakeEmbedder{table: map[string][]float32{
		"eggs": {1, 0},
		"milk": {0, 1},
		"egg":  {0.9, 0.435},
	}}
	m := testManager(t, openListStore(t), embed)
	if _, _, err := m.Add(ctx, KeyShopping, "eggs"); err != nil {
		t.Fatalf("Add eggs: %v", err)
	}
	if _, _, err := m.Add(ctx, KeyShopping, "milk"); err != nil {
		t.Fatalf("Add milk: %v", err)
	}

	removed, err := m.Remove(ctx, KeyShopping, "the egg")
	if err != nil {
		t.Fatalf("Remove near miss: %v", err)
	}
	if removed.Text != "eggs" {
		t.Fatalf("removed = %q, want eggs through the name matcher", removed.Text)
	}
	if embed.calls != 1 {
		t.Fatalf("embed calls = %d, want one similarity lookup", embed.calls)
	}
}

func TestRemoveRefusesAnAmbiguousItem(t *testing.T) {
	ctx := context.Background()
	m := testManager(t, openListStore(t), nil)
	for _, item := range []string{"whole milk", "oat milk"} {
		if _, _, err := m.Add(ctx, KeyShopping, item); err != nil {
			t.Fatalf("Add(%q): %v", item, err)
		}
	}
	if _, err := m.Remove(ctx, KeyShopping, "milk"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("ambiguous Remove = %v, want ErrItemNotFound and no forced guess", err)
	}
}

func TestParseKey(t *testing.T) {
	cases := map[string]string{
		"shopping":      KeyShopping,
		"shopping list": KeyShopping,
		"groceries":     KeyShopping,
		"to-do":         KeyTodo,
		"to do list":    KeyTodo,
		"todo":          KeyTodo,
		"tasks":         KeyTodo,
	}
	for raw, want := range cases {
		if got, ok := ParseKey(raw); !ok || got != want {
			t.Errorf("ParseKey(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	if _, ok := ParseKey("wish list"); ok {
		t.Error("ParseKey accepted a third list")
	}
}

func TestExtraction(t *testing.T) {
	addCases := []struct {
		text string
		list string
		item string
	}{
		{"add milk to the shopping list", KeyShopping, "milk"},
		{"put eggs on my shopping list", KeyShopping, "eggs"},
		{"can you add bread to the shopping list", KeyShopping, "bread"},
		{"add call the dentist to my to-do list", KeyTodo, "call the dentist"},
		{"put finish the report on my to do list", KeyTodo, "finish the report"},
	}
	for _, tc := range addCases {
		got, ok := extractAdd(tc.text)
		if !ok || got.List != tc.list || got.Item != tc.item {
			t.Errorf("extractAdd(%q) = %+v, %v; want %s/%q", tc.text, got, ok, tc.list, tc.item)
		}
	}
	for _, text := range []string{
		"add milk to the wish list",
		"remind me to buy milk",
		"remember that I parked on level 3",
	} {
		if got, ok := extractAdd(text); ok {
			t.Errorf("extractAdd(%q) = %+v, want no match", text, got)
		}
	}

	readCases := map[string]string{
		"what's on my shopping list":      KeyShopping,
		"what is on the to-do list":       KeyTodo,
		"read me the shopping list":       KeyShopping,
		"show me my to do list":           KeyTodo,
		"what's left on my shopping list": KeyShopping,
		"what do I need to buy":           KeyShopping,
		"what do I still need to buy":     KeyShopping,
		"what's on the list for shopping": KeyShopping,
	}
	for text, want := range readCases {
		got, ok := extractRead(text)
		if !ok || got.List != want {
			t.Errorf("extractRead(%q) = %+v, %v; want list %q", text, got, ok, want)
		}
	}
	if got, ok := extractRead("what do I need to buy"); !ok || got.List != KeyShopping {
		t.Errorf("extractRead(buy) = %+v, %v; want the shopping list", got, ok)
	}
	for _, text := range []string{"what's on my wish list", "what's on my calendar", "remind me to buy milk"} {
		if got, ok := extractRead(text); ok {
			t.Errorf("extractRead(%q) = %+v, want no match", text, got)
		}
	}

	removeCases := []struct {
		text string
		list string
		item string
	}{
		{"remove milk from the shopping list", KeyShopping, "milk"},
		{"take the eggs off the shopping list", KeyShopping, "eggs"},
		{"delete bread from the shopping list", KeyShopping, "bread"},
		{"take call mum off my to do list", KeyTodo, "call mum"},
		{"cross wash the car off the to-do list", KeyTodo, "wash the car"},
	}
	for _, tc := range removeCases {
		got, ok := extractRemove(tc.text)
		if !ok || got.List != tc.list || got.Item != tc.item {
			t.Errorf("extractRemove(%q) = %+v, %v; want %s/%q", tc.text, got, ok, tc.list, tc.item)
		}
	}
	for _, text := range []string{"remove milk from the wish list", "cancel the timer"} {
		if got, ok := extractRemove(text); ok {
			t.Errorf("extractRemove(%q) = %+v, want no match", text, got)
		}
	}
}
