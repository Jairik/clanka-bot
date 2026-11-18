package speech_test

import (
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

func TestCleanStripsMarkdownAndEmoji(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"bold", "**Sure.** I can help.", "Sure. I can help."},
		{"italic", "This is _very_ important.", "This is very important."},
		{"strike", "~~Old~~ New plan.", "Old New plan."},
		{"heading", "## Weather\nIt is sunny.", "Weather. It is sunny."},
		{"unordered list", "You need:\n- Milk\n- Eggs", "You need: Milk. Eggs"},
		{"star list", "Pick one:\n* Tea\n* Coffee", "Pick one: Tea. Coffee"},
		{"numbered list", "Steps:\n1. Boil water\n2) Add tea", "Steps: Boil water. Add tea"},
		{"inline code", "Run `make test` now.", "Run make test now."},
		{"emoji", "Done! 🎉👍🏽", "Done!"},
		{"emoji in sentence", "The weather is ☀️ sunny.", "The weather is sunny."},
		{"blank paragraph", "First line.\n\nSecond line.", "First line. Second line."},
		{"trailing whitespace", "  Hello.  \n  ", "Hello."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := speech.Clean(tc.in); got != tc.want {
				t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanerMatchesCleanAcrossDeltas proves the streaming cleaner holds a
// marker or rune split across deltas instead of emitting half of it.
func TestCleanerMatchesCleanAcrossDeltas(t *testing.T) {
	const text = "**Sure.**\n- Milk\n- Eggs 😊"
	c := speech.NewCleaner()
	var got strings.Builder
	for _, delta := range []string{"**Su", "re.**\n- Mi", "lk\n- Eg", "gs \xf0\x9f\x98"} {
		got.WriteString(c.Write(delta))
	}
	got.WriteString(c.Flush())
	if want := speech.Clean(text); got.String() != want {
		t.Fatalf("streamed clean = %q, want %q", got.String(), want)
	}
}
