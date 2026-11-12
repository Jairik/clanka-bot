package lastreply_test

import (
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
)

func TestRecordAndLast(t *testing.T) {
	store := lastreply.New()
	if _, ok := store.Last(); ok {
		t.Fatal("an empty store reported a reply")
	}
	if _, ok := (*lastreply.Store)(nil).Last(); ok {
		t.Fatal("a nil store reported a reply")
	}
	// A nil store must accept Record without panicking, so optional wiring is
	// safe in tests.
	(*lastreply.Store)(nil).Record(lastreply.Reply{Text: "ignored"})

	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	store.Record(lastreply.Reply{
		Text:      "It is 9 o'clock.",
		Tool:      "time.now",
		Path:      "fast_path",
		LocalOnly: false,
		At:        at,
	})
	got, ok := store.Last()
	if !ok {
		t.Fatal("Last reported no reply after Record")
	}
	if got.Text != "It is 9 o'clock." || got.Tool != "time.now" || got.Path != "fast_path" || got.LocalOnly {
		t.Fatalf("Last = %+v", got)
	}
	if !got.At.Equal(at) {
		t.Fatalf("At = %v, want %v", got.At, at)
	}
}

func TestBlankRepliesDoNotReplaceTheLastOne(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "Keep me."})
	store.Record(lastreply.Reply{Text: "   \n"})
	got, ok := store.Last()
	if !ok || got.Text != "Keep me." {
		t.Fatalf("Last = %+v, %v; want the previous reply", got, ok)
	}
}

func TestRecordDefaultsTheTimestamp(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "Hello."})
	got, _ := store.Last()
	if got.At.IsZero() {
		t.Fatal("Record left the timestamp zero")
	}
}

func TestConcurrentRecordAndLast(t *testing.T) {
	store := lastreply.New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			store.Record(lastreply.Reply{Text: "reply"})
		}()
		go func() {
			defer wg.Done()
			if reply, ok := store.Last(); ok && reply.Text == "" {
				t.Error("Last returned an empty text")
			}
		}()
	}
	wg.Wait()
}
