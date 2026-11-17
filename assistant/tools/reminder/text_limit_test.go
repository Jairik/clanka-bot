package reminder_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
	"gopkg.in/yaml.v3"
)

// TestReminderConfigValidation covers the allowed range for tools.reminder.max_text.
func TestReminderConfigValidation(t *testing.T) {
	def := reminder.DefaultConfig()
	if def.MaxText != 500 {
		t.Fatalf("DefaultConfig().MaxText = %d, want 500", def.MaxText)
	}

	cases := []struct {
		name    string
		maxText int
		wantErr bool
	}{
		{"below minimum (19)", 19, true},
		{"minimum (20)", 20, false},
		{"default (500)", 500, false},
		{"maximum (2000)", 2000, false},
		{"above maximum (2001)", 2001, true},
		{"zero", 0, true},
		{"negative", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := reminder.NewModule()
			yamlContent := fmt.Sprintf("max_text: %d\n", tc.maxText)
			var node yaml.Node
			if err := yaml.Unmarshal([]byte(yamlContent), &node); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			// Test decoding into Module.Configure
			// node.Content[0] is the mapping node
			err := mod.Configure(node.Content[0])
			if tc.wantErr && err == nil {
				t.Errorf("Configure(%d) succeeded, want error", tc.maxText)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Configure(%d) failed: %v", tc.maxText, err)
			}
		})
	}

	// Nil section should keep code defaults.
	mod := reminder.NewModule()
	if err := mod.Configure(nil); err != nil {
		t.Fatalf("Configure(nil): %v", err)
	}
}

// TestReminderTextLimitBoundaries verifies that exactly N runes is accepted and
// persisted, while N+1 runes returns a spoken error and writes nothing.
func TestReminderTextLimitBoundaries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	const limit = 50
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	rec := newTraceRecorder()
	m := reminder.NewManager(reminder.Options{
		Now:     func() time.Time { return now },
		Speaker: rec,
		Logger:  testLogger(),
		Config:  reminder.Config{MaxText: limit},
	})
	if err := m.AttachStore(st); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	defer m.Close()

	setTool := reminder.NewSet(m)

	countRows := func() int {
		var count int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM reminders").Scan(&count); err != nil {
			t.Fatalf("count reminders: %v", err)
		}
		return count
	}

	// 1. Exactly limit (50) ASCII runes -> accepted and written.
	msgExact := strings.Repeat("a", limit)
	out, err := setTool.Handler(ctx, reminder.SetIn{Message: msgExact, DurationSeconds: 60})
	if err != nil {
		t.Fatalf("NewSet exactly %d runes failed: %v", limit, err)
	}
	if out.ID == "" {
		t.Fatalf("expected non-empty ID for accepted reminder")
	}
	if countRows() != 1 {
		t.Fatalf("expected 1 row written, got %d", countRows())
	}

	// 2. Exactly limit+1 (51) ASCII runes -> refused with spoken error, nothing written.
	msgTooLong := strings.Repeat("a", limit+1)
	_, err = setTool.Handler(ctx, reminder.SetIn{Message: msgTooLong, DurationSeconds: 60})
	if err == nil {
		t.Fatalf("NewSet %d runes succeeded, want refusal", limit+1)
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *tool.ToolError, got %T: %v", err, err)
	}
	const wantSay = "That reminder is too long."
	if toolErr.Say != wantSay {
		t.Fatalf("toolErr.Say = %q, want %q", toolErr.Say, wantSay)
	}
	if countRows() != 1 {
		t.Fatalf("expected row count to remain 1, got %d", countRows())
	}

	// 3. Multi-byte UTF-8 test: non-Latin runes (e.g. Japanese kanji/hiragana)
	// Each rune is 3 bytes in UTF-8. 50 runes = 150 bytes.
	// Verifies utf8.RuneCountInString is used so non-Latin text is not penalised by byte length.
	multiByteExact := strings.Repeat("日", limit)
	if utf8.RuneCountInString(multiByteExact) != limit || len(multiByteExact) == limit {
		t.Fatalf("test setup: expected %d runes but %d bytes", limit, len(multiByteExact))
	}
	out, err = setTool.Handler(ctx, reminder.SetIn{Message: multiByteExact, At: "12:00"})
	if err != nil {
		t.Fatalf("NewSet multi-byte exactly %d runes failed: %v", limit, err)
	}
	if out.ID == "" {
		t.Fatalf("expected non-empty ID for multi-byte reminder")
	}
	if countRows() != 2 {
		t.Fatalf("expected 2 rows written, got %d", countRows())
	}

	// 4. Multi-byte UTF-8 with limit+1 runes -> refused.
	multiByteTooLong := strings.Repeat("日", limit+1)
	_, err = setTool.Handler(ctx, reminder.SetIn{Message: multiByteTooLong, At: "12:00"})
	if err == nil {
		t.Fatalf("NewSet multi-byte %d runes succeeded, want refusal", limit+1)
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

	// 5. Direct manager calls return &reminder.Error with the spoken refusal.
	_, err = m.StartDuration(msgTooLong, time.Minute)
	var spokenErr *reminder.Error
	if !errors.As(err, &spokenErr) || spokenErr.Message != wantSay {
		t.Fatalf("StartDuration error = %v, want Error{%q}", err, wantSay)
	}
	_, err = m.StartClock(msgTooLong, 14, 0, false)
	if !errors.As(err, &spokenErr) || spokenErr.Message != wantSay {
		t.Fatalf("StartClock error = %v, want Error{%q}", err, wantSay)
	}
}
