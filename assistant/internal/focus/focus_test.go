package focus_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
)

// fakeHolder records the intents it was offered and answers the ones it
// declares. A nil handled map means every declared intent is handled.
type fakeHolder struct {
	name    string
	intents []focus.Intent
	handled map[focus.Intent]bool

	mu    sync.Mutex
	calls []focus.Intent
}

func (h *fakeHolder) Intents() []focus.Intent { return h.intents }

func (h *fakeHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	h.mu.Lock()
	h.calls = append(h.calls, intent)
	h.mu.Unlock()
	if h.handled != nil {
		if ok, exists := h.handled[intent]; exists {
			return h.name + ":" + string(intent), ok
		}
	}
	for _, declared := range h.intents {
		if declared == intent {
			return h.name + ":" + string(intent), true
		}
	}
	return "", false
}

func (h *fakeHolder) called() []focus.Intent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]focus.Intent(nil), h.calls...)
}

// TestIntentsAreTheFixedVocabulary pins the six shared intents and their order.
func TestIntentsAreTheFixedVocabulary(t *testing.T) {
	want := []focus.Intent{focus.Stop, focus.Pause, focus.Resume, focus.Next, focus.Cancel, focus.Snooze}
	got := focus.Intents()
	if len(got) != len(want) {
		t.Fatalf("Intents() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Intents()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseIntentAcceptsOnlyTheVocabulary keeps config validation honest: a
// typo must be an error, not an unknown intent that silently never fires.
func TestParseIntentAcceptsOnlyTheVocabulary(t *testing.T) {
	for _, want := range focus.Intents() {
		for _, text := range []string{string(want), " " + string(want) + " ", upper(string(want))} {
			got, ok := focus.ParseIntent(text)
			if !ok || got != want {
				t.Errorf("ParseIntent(%q) = %q, %v; want %q, true", text, got, ok, want)
			}
		}
	}
	for _, text := range []string{"", "stopp", "pause the music", "audio.stop", "next song"} {
		if got, ok := focus.ParseIntent(text); ok {
			t.Errorf("ParseIntent(%q) = %q, true; want rejected", text, got)
		}
	}
}

// TestIntentTargetsAreUndotted keeps the Control phrase table readable: a bare
// word is an intent, and a canonical Tool name is always dotted.
func TestIntentTargetsAreUndotted(t *testing.T) {
	for _, text := range []string{"stop", " pause ", "snooze"} {
		if !focus.IsIntentTarget(text) {
			t.Errorf("IsIntentTarget(%q) = false, want true", text)
		}
	}
	for _, text := range []string{"audio.stop", "timer.cancel", " audio.volume_set "} {
		if focus.IsIntentTarget(text) {
			t.Errorf("IsIntentTarget(%q) = true, want false", text)
		}
	}
}

// TestHandleIntentUsesTheFixedPriority proves the code order: a ringing alert
// wins over the reply, and the reply wins over music.
func TestHandleIntentUsesTheFixedPriority(t *testing.T) {
	var registry focus.Registry
	ring := &fakeHolder{name: "ring", intents: []focus.Intent{focus.Stop}}
	reply := &fakeHolder{name: "reply", intents: []focus.Intent{focus.Stop}}
	music := &fakeHolder{name: "music", intents: []focus.Intent{focus.Stop}}

	ringClaim := registry.Claim(focus.Ring, ring)
	replyClaim := registry.Claim(focus.Reply, reply)
	musicClaim := registry.Claim(focus.Music, music)
	defer musicClaim.Release()

	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok || text != "ring:stop" {
		t.Fatalf("HandleIntent = %q, %v; want the ringing alert first", text, ok)
	}
	ringClaim.Release()
	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok || text != "reply:stop" {
		t.Fatalf("HandleIntent = %q, %v; want the current reply second", text, ok)
	}
	replyClaim.Release()
	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok || text != "music:stop" {
		t.Fatalf("HandleIntent = %q, %v; want music third", text, ok)
	}
	musicClaim.Release()
	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); ok {
		t.Fatalf("HandleIntent = %q, true; want nothing to hold Focus", text)
	}
	if got := ring.called(); len(got) != 1 {
		t.Errorf("ring was offered %v, want only the first stop", got)
	}
}

// TestHandleIntentFallsToALowerHolderThatDeclaresIt covers "pause" while a
// reply holds Focus: the reply does not pause, so music gets the intent.
func TestHandleIntentFallsToALowerHolderThatDeclaresIt(t *testing.T) {
	var registry focus.Registry
	reply := &fakeHolder{name: "reply", intents: []focus.Intent{focus.Stop}}
	music := &fakeHolder{name: "music", intents: []focus.Intent{focus.Pause}}
	registry.Claim(focus.Reply, reply)
	registry.Claim(focus.Music, music)

	if text, ok := registry.HandleIntent(context.Background(), focus.Pause); !ok || text != "music:pause" {
		t.Fatalf("HandleIntent(pause) = %q, %v; want music", text, ok)
	}
	if got := reply.called(); len(got) != 0 {
		t.Fatalf("reply was offered %v, want no call for an undeclared intent", got)
	}
}

// TestHandleIntentSkipsAHolderThatDeclines lets a holder finish between the
// claim and the intent (a race the caller must tolerate).
func TestHandleIntentSkipsAHolderThatDeclines(t *testing.T) {
	var registry focus.Registry
	ring := &fakeHolder{name: "ring", intents: []focus.Intent{focus.Stop}, handled: map[focus.Intent]bool{focus.Stop: false}}
	reply := &fakeHolder{name: "reply", intents: []focus.Intent{focus.Stop}}
	registry.Claim(focus.Ring, ring)
	registry.Claim(focus.Reply, reply)

	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok || text != "reply:stop" {
		t.Fatalf("HandleIntent = %q, %v; want the next holder after a decline", text, ok)
	}
}

// TestReleasingAnOldClaimKeepsANewerHolder is the ownership rule: a turn that
// finishes late must not clear the holder that replaced it.
func TestReleasingAnOldClaimKeepsANewerHolder(t *testing.T) {
	var registry focus.Registry
	first := &fakeHolder{name: "first", intents: []focus.Intent{focus.Stop}}
	second := &fakeHolder{name: "second", intents: []focus.Intent{focus.Stop}}

	old := registry.Claim(focus.Reply, first)
	current := registry.Claim(focus.Reply, second)
	old.Release()
	old.Release() // releasing twice is harmless

	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok || text != "second:stop" {
		t.Fatalf("HandleIntent after the old release = %q, %v; want the newer holder kept", text, ok)
	}
	current.Release()
	if _, ok := registry.HandleIntent(context.Background(), focus.Stop); ok {
		t.Fatal("HandleIntent after the current release succeeded, want nothing held")
	}
}

// TestNilRegistryAndClaimAreSafe keeps the optional seam cheap: a deployment
// without Focus wiring must not panic.
func TestNilRegistryAndClaimAreSafe(t *testing.T) {
	var registry *focus.Registry
	if text, ok := registry.HandleIntent(context.Background(), focus.Stop); ok {
		t.Fatalf("nil registry handled %q", text)
	}
	var claim *focus.Claim
	claim.Release()

	var zero focus.Registry
	claim = zero.Claim(focus.Reply, nil)
	claim.Release()
	if _, ok := zero.HandleIntent(context.Background(), focus.Stop); ok {
		t.Fatal("a nil holder took Focus")
	}
}

// TestConcurrentClaimsAndIntents exercises the race the reply lifecycle creates:
// turns claim and release while a shared phrase is routed. Run with -race.
func TestConcurrentClaimsAndIntents(t *testing.T) {
	var registry focus.Registry
	holder := &fakeHolder{name: "reply", intents: []focus.Intent{focus.Stop}}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				claim := registry.Claim(focus.Reply, holder)
				registry.HandleIntent(context.Background(), focus.Stop)
				claim.Release()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				registry.HandleIntent(context.Background(), focus.Stop)
			}
		}()
	}
	wg.Wait()
	if _, ok := registry.HandleIntent(context.Background(), focus.Stop); ok {
		t.Fatal("a holder survived every release")
	}
}

// upper is a tiny helper so the parse test does not import strings just for one
// ASCII case fold.
func upper(text string) string {
	out := []byte(text)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - ('a' - 'A')
		}
	}
	return string(out)
}
