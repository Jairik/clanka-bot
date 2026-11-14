package note

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// fakeEmbedder is the embed boundary: it returns the one vector table the test
// declares and records the texts it was asked to embed.
type fakeEmbedder struct {
	table map[string][]float32
	calls int
	texts []string
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.texts = append([]string(nil), texts...)
	if f.err != nil {
		return nil, f.err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vec, ok := f.table[text]
		if !ok {
			return nil, fmt.Errorf("no fake vector for %q", text)
		}
		vectors[i] = vec
	}
	return vectors, nil
}

func openNoteStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testManager(t *testing.T, st *store.Store, embed *fakeEmbedder, cfg Config) *Manager {
	t.Helper()
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	opts := Options{
		DB:     st,
		Now:    func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
	}
	if embed != nil {
		opts.Embed = embed
	}
	return NewManager(opts)
}

func TestRememberListUpdateAndDelete(t *testing.T) {
	st := openNoteStore(t)
	m := testManager(t, st, nil, Config{MinScore: 0.35, MaxResults: 3, MaxText: 500, MaxNotes: 100})
	ctx := context.Background()

	first, err := m.Remember(ctx, "  the spare key is under the pot  ")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if first.Text != "the spare key is under the pot" {
		t.Fatalf("text = %q", first.Text)
	}
	second, err := m.Remember(ctx, "buy stamps")
	if err != nil {
		t.Fatalf("Remember second: %v", err)
	}

	notes, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(notes) != 2 || notes[0].ID != second.ID {
		t.Fatalf("List = %+v, want newest first", notes)
	}

	updated, err := m.Update(ctx, first.ID, "the spare key is in the drawer")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Text != "the spare key is in the drawer" {
		t.Fatalf("updated text = %q", updated.Text)
	}

	deleted, err := m.Delete(ctx, second.ID)
	if err != nil || deleted != 1 {
		t.Fatalf("Delete = %d, %v", deleted, err)
	}
	if again, err := m.Delete(ctx, second.ID); err != nil || again != 0 {
		t.Fatalf("second Delete = %d, %v; want 0, nil", again, err)
	}
}

func TestRememberValidation(t *testing.T) {
	st := openNoteStore(t)
	m := testManager(t, st, nil, Config{MinScore: 0.35, MaxResults: 3, MaxText: 10, MaxNotes: 100})
	ctx := context.Background()

	if _, err := m.Remember(ctx, "   "); err == nil {
		t.Fatal("empty note was accepted")
	}
	var spoken *Error
	if _, err := m.Remember(ctx, strings.Repeat("x", 11)); !errors.As(err, &spoken) {
		t.Fatalf("long note error = %v, want a spoken Error", err)
	}
}

func TestRecallExactSubstringSkipsTheEmbedder(t *testing.T) {
	st := openNoteStore(t)
	embed := &fakeEmbedder{table: map[string][]float32{
		"the boiler is serviced on Tuesday": {1, 0},
		"buy milk":                          {0, 1},
	}}
	m := testManager(t, st, embed, Config{MinScore: 0.35, MaxResults: 3, MaxText: 500, MaxNotes: 100})
	ctx := context.Background()
	if _, err := m.Remember(ctx, "the boiler is serviced on Tuesday"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if _, err := m.Remember(ctx, "buy milk"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// Remember captured each vector; the recall must not ask again.
	embed.calls = 0

	matches, err := m.Recall(ctx, "boiler")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(matches) != 1 || matches[0].Score != 1 || !strings.Contains(matches[0].Text, "boiler") {
		t.Fatalf("matches = %+v", matches)
	}
	if embed.calls != 0 {
		t.Fatalf("embed calls = %d, want 0 for an exact substring", embed.calls)
	}
}

func TestRecallRanksByMeaningAndFiltersByScore(t *testing.T) {
	st := openNoteStore(t)
	embed := &fakeEmbedder{table: map[string][]float32{
		"the boiler is serviced on Tuesday": {1, 0, 0},
		"buy milk":                          {0, 1, 0},
		"passport is in the drawer":         {0, 0, 1},
		"when is the plumber coming":        {0.92, 0.2, 0},
	}}
	cfg := Config{MinScore: 0.35, MaxResults: 2, MaxText: 500, MaxNotes: 100}
	m := testManager(t, st, embed, cfg)
	ctx := context.Background()
	for _, text := range []string{"the boiler is serviced on Tuesday", "buy milk", "passport is in the drawer"} {
		if _, err := m.Remember(ctx, text); err != nil {
			t.Fatalf("Remember(%q): %v", text, err)
		}
	}

	matches, err := m.Recall(ctx, "when is the plumber coming")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(matches) != 1 || !strings.Contains(matches[0].Text, "boiler") {
		t.Fatalf("matches = %+v, want the boiler note only", matches)
	}
	if matches[0].Score < 0.35 {
		t.Fatalf("score = %.3f, want at or above the floor", matches[0].Score)
	}

	// A query below the floor is a truthful no-match, not a forced guess.
	embed.table["something unrelated"] = []float32{0.1, 0.1, -0.9}
	matches, err = m.Recall(ctx, "something unrelated")
	if err != nil {
		t.Fatalf("Recall below floor: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("below-floor matches = %+v, want none", matches)
	}
}

func TestRecallEmbedFailureIsReported(t *testing.T) {
	st := openNoteStore(t)
	embed := &fakeEmbedder{err: errors.New("embed down")}
	m := testManager(t, st, embed, Config{MinScore: 0.35, MaxResults: 3, MaxText: 500, MaxNotes: 100})
	ctx := context.Background()
	if _, err := m.Remember(ctx, "buy milk"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if _, err := m.Recall(ctx, "milk run"); err == nil {
		t.Fatal("Recall succeeded with a dead embedder, want an error")
	}
}

func TestForgetQueryDeletesOnlyTheClosestNote(t *testing.T) {
	st := openNoteStore(t)
	embed := &fakeEmbedder{table: map[string][]float32{
		"the boiler is serviced on Tuesday":  {1, 0},
		"the boiler room key is in the shed": {0.8, 0.6},
		"the plumber is coming Friday":       {0.2, 0.98},
		"boiler appointment":                 {0.95, 0.1},
		"nothing like this exists anywhere":  {0, -1},
	}}
	m := testManager(t, st, embed, Config{MinScore: 0.35, MaxResults: 3, MaxText: 500, MaxNotes: 100})
	ctx := context.Background()
	for _, text := range []string{"the boiler is serviced on Tuesday", "the boiler room key is in the shed", "the plumber is coming Friday"} {
		if _, err := m.Remember(ctx, text); err != nil {
			t.Fatalf("Remember(%q): %v", text, err)
		}
	}

	deleted, err := m.ForgetQuery(ctx, "boiler appointment")
	if err != nil {
		t.Fatalf("ForgetQuery: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want exactly one", deleted)
	}
	notes, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("remaining = %d, want 2", len(notes))
	}
	for _, note := range notes {
		if note.Text == "the boiler is serviced on Tuesday" {
			t.Fatal("ForgetQuery deleted the wrong note")
		}
	}

	if deleted, err := m.ForgetQuery(ctx, "nothing like this exists anywhere"); err != nil || deleted != 0 {
		t.Fatalf("ForgetQuery miss = %d, %v; want 0, nil", deleted, err)
	}
}
