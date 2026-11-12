package lastreply_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
)

func TestRecordAndLastKeepProvenance(t *testing.T) {
	store := lastreply.New()
	if _, ok := store.Last(); ok {
		t.Fatal("a new store has a reply")
	}

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
		t.Fatal("Last returned no reply after Record")
	}
	if got.Text != "It is 9 o'clock." || got.Tool != "time.now" || got.Path != "fast_path" || got.LocalOnly {
		t.Fatalf("Last = %+v", got)
	}
	if !got.At.Equal(at) {
		t.Errorf("At = %s, want %s", got.At, at)
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var store *lastreply.Store
	store.Record(lastreply.Reply{Text: "ignored"})
	if _, ok := store.Last(); ok {
		t.Fatal("nil store returned a reply")
	}
}

func TestBlankRepliesDoNotEraseTheLastOne(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "The first reply."})
	store.Record(lastreply.Reply{Text: "   \n"})
	store.Record(lastreply.Reply{})

	got, ok := store.Last()
	if !ok || got.Text != "The first reply." {
		t.Fatalf("Last = %+v/%v, want the first reply", got, ok)
	}
}

func TestZeroTimeGetsARecordTimestamp(t *testing.T) {
	store := lastreply.New()
	before := time.Now().Add(-time.Second)
	store.Record(lastreply.Reply{Text: "Recorded."})
	got, _ := store.Last()
	if got.At.IsZero() || got.At.Before(before) {
		t.Fatalf("At = %s, want a current timestamp", got.At)
	}
}

// TestConcurrentRecordAndLastAreRaceFree exercises the two public methods
// together; run with -race to prove the mutex covers both.
func TestConcurrentRecordAndLastAreRaceFree(t *testing.T) {
	store := lastreply.New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				store.Record(lastreply.Reply{Text: fmt.Sprintf("reply %d-%d", i, j)})
				if reply, ok := store.Last(); ok && reply.Text == "" {
					t.Errorf("Last returned a blank reply")
				}
			}
		}(i)
	}
	wg.Wait()
	if reply, ok := store.Last(); !ok || reply.Text == "" {
		t.Fatalf("final Last = %+v/%v, want a real reply", reply, ok)
	}
}
