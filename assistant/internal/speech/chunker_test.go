package speech_test

import (
	"reflect"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

func TestChunkerFlushesTheFirstChunkAtAClauseBoundary(t *testing.T) {
	c := speech.NewChunker()
	if got := c.Write("It depends on the time, and"); !reflect.DeepEqual(got, []string{"It depends on the time,"}) {
		t.Fatalf("chunks = %q, want the clause-flushed first chunk", got)
	}
	if got := c.Write(" the day."); len(got) != 0 {
		t.Fatalf("chunks = %q, want the trailing dot held", got)
	}
	if got := c.Flush(); got != "and the day." {
		t.Fatalf("flush = %q, want the remainder", got)
	}
}

func TestChunkerFlushesTheFirstChunkAfterTwelveWords(t *testing.T) {
	c := speech.NewChunker()
	if got := c.Write("one two three four five six seven eight nine ten eleven twelve"); len(got) != 0 {
		t.Fatalf("chunks = %q, want nothing until the word boundary is visible", got)
	}
	if got := c.Write(" thirteen"); !reflect.DeepEqual(got, []string{
		"one two three four five six seven eight nine ten eleven twelve",
	}) {
		t.Fatalf("chunks = %q, want the first twelve words", got)
	}
	if got := c.Flush(); got != "thirteen" {
		t.Fatalf("flush = %q, want the remainder", got)
	}
}

func TestChunkerSplitsLaterChunksAtSentenceEnds(t *testing.T) {
	c := speech.NewChunker()
	var chunks []string
	for _, delta := range []string{"Sure. ", "The forecast", " is sunny. ", "Bring a coat!"} {
		chunks = append(chunks, c.Write(delta)...)
	}
	if rest := c.Flush(); rest != "" {
		t.Fatalf("flush = %q, want nothing left", rest)
	}
	want := []string{"Sure.", "The forecast is sunny.", "Bring a coat!"}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks = %q, want %q", chunks, want)
	}
}

func TestChunkerHoldsATrailingDotUntilItKnowsTheSentenceEnded(t *testing.T) {
	c := speech.NewChunker()
	if got := c.Write("The value is 3."); len(got) != 0 {
		t.Fatalf("chunks = %q, want the decimal to stay open", got)
	}
	if got := c.Write("14 exactly. Next"); !reflect.DeepEqual(got, []string{"The value is 3.14 exactly."}) {
		t.Fatalf("chunks = %q, want one sentence", got)
	}
	if got := c.Flush(); got != "Next" {
		t.Fatalf("flush = %q, want the trailing text", got)
	}
}

func TestChunkerKeepsAbbreviationsTogether(t *testing.T) {
	c := speech.NewChunker()
	chunks := c.Write("Dr. Smith arrives at 3.30 p.m. tomorrow. Use e.g. the side door. Done.")
	want := []string{"Dr. Smith arrives at 3.30 p.m. tomorrow.", "Use e.g. the side door."}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks = %q, want %q", chunks, want)
	}
	if rest := c.Flush(); rest != "Done." {
		t.Fatalf("flush = %q, want %q", rest, "Done.")
	}
}

func TestChunkerKeepsNumberedListsWhole(t *testing.T) {
	c := speech.NewChunker()
	chunks := c.Write("You need these:\n1. Milk\n2. Eggs\n3. Bread")
	want := []string{"You need these:", "1. Milk", "2. Eggs"}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks = %q, want %q", chunks, want)
	}
	if rest := c.Flush(); rest != "3. Bread" {
		t.Fatalf("flush = %q, want %q", rest, "3. Bread")
	}
}

func TestChunkerJoinsDeltasSplitMidWord(t *testing.T) {
	c := speech.NewChunker()
	var chunks []string
	for _, delta := range []string{"The wea", "ther is ", "fine.", " Bring"} {
		chunks = append(chunks, c.Write(delta)...)
	}
	if !reflect.DeepEqual(chunks, []string{"The weather is fine."}) {
		t.Fatalf("chunks = %q, want the joined sentence", chunks)
	}
	if rest := c.Flush(); rest != "Bring" {
		t.Fatalf("flush = %q, want the remainder", rest)
	}
}

func TestChunkerFlushReturnsTheRemainderOnce(t *testing.T) {
	c := speech.NewChunker()
	if got := c.Write("hello there"); len(got) != 0 {
		t.Fatalf("chunks = %q, want nothing yet", got)
	}
	if got := c.Flush(); got != "hello there" {
		t.Fatalf("flush = %q, want the remainder", got)
	}
	if got := c.Flush(); got != "" {
		t.Fatalf("second flush = %q, want empty", got)
	}
}
