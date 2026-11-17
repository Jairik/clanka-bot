package list

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// TestListConfigValidation covers the allowed range for tools.list.max_item_text.
func TestListConfigValidation(t *testing.T) {
	def := DefaultConfig()
	if def.MaxItemText != 200 {
		t.Fatalf("DefaultConfig().MaxItemText = %d, want 200", def.MaxItemText)
	}

	cases := []struct {
		name        string
		maxItemText int
		wantErr     bool
	}{
		{"below minimum (9)", 9, true},
		{"minimum (10)", 10, false},
		{"default (200)", 200, false},
		{"maximum (1000)", 1000, false},
		{"above maximum (1001)", 1001, true},
		{"zero", 0, true},
		{"negative", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := NewModule()
			yamlContent := fmt.Sprintf("max_items: 200\nmax_item_text: %d\n", tc.maxItemText)
			var node yaml.Node
			if err := yaml.Unmarshal([]byte(yamlContent), &node); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			err := mod.Configure(node.Content[0])
			if tc.wantErr && err == nil {
				t.Errorf("Configure(max_item_text=%d) succeeded, want error", tc.maxItemText)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Configure(max_item_text=%d) failed: %v", tc.maxItemText, err)
			}
		})
	}

	// Nil section should keep code defaults.
	mod := NewModule()
	if err := mod.Configure(nil); err != nil {
		t.Fatalf("Configure(nil): %v", err)
	}
	if mod.cfg.MaxItemText != 200 {
		t.Fatalf("mod.cfg.MaxItemText = %d, want 200", mod.cfg.MaxItemText)
	}
}

// TestListItemTextLimitBoundaries verifies that exactly N runes is accepted and
// persisted, while N+1 runes returns a spoken error and writes nothing.
func TestListItemTextLimitBoundaries(t *testing.T) {
	ctx := context.Background()
	st := openListStore(t)

	const limit = 30
	m := NewManager(Options{
		DB:     st,
		Config: Config{MaxItems: 50, MaxItemText: limit},
	})

	addTool := NewAdd(m)

	countRows := func() int {
		items, err := m.Items(ctx, KeyShopping)
		if err != nil {
			t.Fatalf("Items: %v", err)
		}
		return len(items)
	}

	// 1. Exactly limit (30) ASCII runes -> accepted and written.
	itemExact := strings.Repeat("a", limit)
	out, err := addTool.Handler(ctx, AddIn{List: KeyShopping, Item: itemExact})
	if err != nil {
		t.Fatalf("NewAdd exactly %d runes failed: %v", limit, err)
	}
	if !out.Added || out.Item.ID == "" {
		t.Fatalf("expected item added, got %+v", out)
	}
	if countRows() != 1 {
		t.Fatalf("expected 1 row written, got %d", countRows())
	}

	// 2. Exactly limit+1 (31) ASCII runes -> refused with spoken error, nothing written.
	itemTooLong := strings.Repeat("a", limit+1)
	_, err = addTool.Handler(ctx, AddIn{List: KeyShopping, Item: itemTooLong})
	if err == nil {
		t.Fatalf("NewAdd %d runes succeeded, want refusal", limit+1)
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *tool.ToolError, got %T: %v", err, err)
	}
	const wantSay = "That item is too long."
	if toolErr.Say != wantSay {
		t.Fatalf("toolErr.Say = %q, want %q", toolErr.Say, wantSay)
	}
	if countRows() != 1 {
		t.Fatalf("expected row count to remain 1, got %d", countRows())
	}

	// 3. Multi-byte UTF-8 test: non-Latin runes (e.g. Japanese kanji/hiragana)
	// Each rune is 3 bytes in UTF-8. 30 runes = 90 bytes.
	// Verifies utf8.RuneCountInString is used so non-Latin text is not penalised by byte length.
	multiByteExact := strings.Repeat("本", limit)
	if utf8.RuneCountInString(multiByteExact) != limit || len(multiByteExact) == limit {
		t.Fatalf("test setup: expected %d runes but %d bytes", limit, len(multiByteExact))
	}
	out, err = addTool.Handler(ctx, AddIn{List: KeyShopping, Item: multiByteExact})
	if err != nil {
		t.Fatalf("NewAdd multi-byte exactly %d runes failed: %v", limit, err)
	}
	if !out.Added || out.Item.ID == "" {
		t.Fatalf("expected multi-byte item added, got %+v", out)
	}
	if countRows() != 2 {
		t.Fatalf("expected 2 rows written, got %d", countRows())
	}

	// 4. Multi-byte UTF-8 with limit+1 runes -> refused.
	multiByteTooLong := strings.Repeat("本", limit+1)
	_, err = addTool.Handler(ctx, AddIn{List: KeyShopping, Item: multiByteTooLong})
	if err == nil {
		t.Fatalf("NewAdd multi-byte %d runes succeeded, want refusal", limit+1)
	}
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *tool.ToolError, got %T: %v", err, err)
	}
	if toolErr.Say != wantSay {
		t.Fatalf("toolErr.Say = %q, want %q", toolErr.Say, wantSay)
	}
	if countRows() != 2 {
		t.Fatalf("expected row count to remain 2, got %d", countRows())
	}

	// 5. Direct manager calls: Add returns &Error with the spoken refusal.
	_, _, err = m.Add(ctx, KeyShopping, itemTooLong)
	var spokenErr *Error
	if !errors.As(err, &spokenErr) || spokenErr.Message != wantSay {
		t.Fatalf("Add error = %v, want Error{%q}", err, wantSay)
	}

	// 6. Direct manager calls: UpdateItem with too-long text is also refused.
	_, err = m.UpdateItem(ctx, KeyShopping, out.Item.ID, itemTooLong)
	if !errors.As(err, &spokenErr) || spokenErr.Message != wantSay {
		t.Fatalf("UpdateItem error = %v, want Error{%q}", err, wantSay)
	}
}
