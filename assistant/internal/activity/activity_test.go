package activity

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

func completed(id string) latency.Completed {
	ms := 1400 * time.Millisecond
	return latency.Completed{
		ID:          id,
		Source:      latency.SourceVoice,
		Path:        "fast_path",
		Tool:        "secret.read",
		Transcript:  "read my secret",
		Outcome:     latency.OutcomeOK,
		Response:    ms,
		HasResponse: true,
		Stages: map[latency.Stage]time.Duration{
			latency.StageTTSFirstAudio: 120 * time.Millisecond,
			latency.StageRouting:       40 * time.Millisecond,
		},
		Checkpoints: map[string]time.Duration{
			latency.CheckpointFirstChunk: 900 * time.Millisecond,
		},
		Detail: &latency.Detail{
			Reply:     "The launch code is 1234.",
			LocalOnly: true,
			Calls: []latency.ToolCallDetail{{
				Name:      "secret.read",
				LocalOnly: true,
				Result:    "the launch code is 1234",
			}},
		},
	}
}

func TestStoreKeepsNewestEntriesWithinItsBound(t *testing.T) {
	store := New(3)
	for _, id := range []string{"one", "two", "three", "four"} {
		store.Observe(completed(id))
	}
	if got := store.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	recent := store.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("Recent len = %d, want 3", len(recent))
	}
	if recent[0].ID != "four" || recent[2].ID != "two" {
		t.Fatalf("unexpected order: %s, %s, %s", recent[0].ID, recent[1].ID, recent[2].ID)
	}
	if _, ok := store.Get("one"); ok {
		t.Fatal("evicted entry is still addressable")
	}
	if entry, ok := store.Get("three"); !ok || entry.ID != "three" {
		t.Fatalf("Get(three) = %+v, %v", entry, ok)
	}
}

func TestObservePreservesProvenanceAndOrderedTimings(t *testing.T) {
	store := New(5)
	store.Observe(completed("req-1"))
	entry, ok := store.Get("req-1")
	if !ok {
		t.Fatal("entry missing")
	}
	if !entry.LocalOnly || entry.Reply != "The launch code is 1234." {
		t.Fatalf("provenance lost: %+v", entry)
	}
	if entry.ResponseMS == nil || *entry.ResponseMS != 1400 {
		t.Fatalf("response latency = %v, want 1400", entry.ResponseMS)
	}
	if len(entry.Stages) != 2 || entry.Stages[0].Name != "routing" || entry.Stages[1].Name != "tts_first_audio" {
		t.Fatalf("stage display order = %+v", entry.Stages)
	}
	if len(entry.Checkpoints) != 1 || entry.Checkpoints[0].Name != "first_chunk" {
		t.Fatalf("checkpoints = %+v", entry.Checkpoints)
	}
	if len(entry.Calls) != 1 || !entry.Calls[0].LocalOnly {
		t.Fatalf("calls = %+v", entry.Calls)
	}
}

func TestObserveDropsEmptyWakeTurnsAndBoundsLongText(t *testing.T) {
	store := New(5)
	// A wake detection with no speech has neither a transcript nor a route.
	store.Observe(latency.Completed{ID: "silence", Source: latency.SourceVoice, Outcome: latency.OutcomeNoAudio})
	if store.Len() != 0 {
		t.Fatalf("empty wake turn was retained: %+v", store.Recent(0))
	}

	long := strings.Repeat("x", maxReplyRunes+20)
	store.Observe(latency.Completed{
		ID:         "long",
		Source:     latency.SourceDashboard,
		Path:       "agent_path",
		Transcript: long,
		Outcome:    latency.OutcomeNoAudio,
		Detail:     &latency.Detail{Reply: long},
	})
	entry, _ := store.Get("long")
	if len([]rune(entry.Reply)) != maxReplyRunes+1 || !strings.HasSuffix(entry.Reply, "…") {
		t.Fatalf("reply not bounded: %d runes", len([]rune(entry.Reply)))
	}
	if len([]rune(entry.Transcript)) != maxTranscriptRunes+1 {
		t.Fatalf("transcript not bounded: %d runes", len([]rune(entry.Transcript)))
	}
}

func TestStoreIsSafeForConcurrentObservers(t *testing.T) {
	store := New(25)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Observe(completed("req"))
			_ = store.Recent(10)
			_, _ = store.Get("req")
		}()
	}
	wg.Wait()
	if got := store.Len(); got != 25 {
		t.Fatalf("Len = %d, want the 25-entry bound", got)
	}
}

func TestNilStoreIsHarmless(t *testing.T) {
	var store *Store
	store.Observe(completed("req"))
	if store.Len() != 0 || store.Recent(5) != nil {
		t.Fatal("nil store returned data")
	}
	if _, ok := store.Get("req"); ok {
		t.Fatal("nil store returned an entry")
	}
}
