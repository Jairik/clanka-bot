package focus_test

import (
	"sync/atomic"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
)

func TestSnapshotReportsAllPriorities(t *testing.T) {
	reg := focus.New()
	snap := reg.Snapshot()
	for _, p := range []focus.Priority{focus.Ring, focus.Reply, focus.Music} {
		if snap[p] {
			t.Fatalf("priority %s held before any claim", p)
		}
	}

	reg.Claim(focus.Ring, &fakeHolder{intents: nil})
	snap = reg.Snapshot()
	if !snap[focus.Ring] {
		t.Fatal("Ring not held after claim")
	}
	if snap[focus.Reply] || snap[focus.Music] {
		t.Fatalf("unexpected holders: %+v", snap)
	}
}

func TestClaimNotifiesAndReleaseNotifies(t *testing.T) {
	reg := focus.New()
	var claimed, released atomic.Int32
	reg.OnClaimed = func(p focus.Priority) { claimed.Add(1) }
	reg.OnReleased = func(p focus.Priority) { released.Add(1) }

	claim := reg.Claim(focus.Reply, &fakeHolder{intents: nil})
	if claimed.Load() != 1 {
		t.Fatalf("OnClaimed calls = %d, want 1", claimed.Load())
	}
	if released.Load() != 0 {
		t.Fatalf("OnReleased calls = %d, want 0", released.Load())
	}

	claim.Release()
	if released.Load() != 1 {
		t.Fatalf("OnReleased calls = %d, want 1", released.Load())
	}
	// Releasing the same claim twice is a no-op.
	claim.Release()
	if released.Load() != 1 {
		t.Fatalf("OnReleased calls = %d, want 1 after second release", released.Load())
	}
}

func TestReleaseCallbackStillRunsWhenWrapped(t *testing.T) {
	reg := focus.New()
	var released atomic.Int32
	// main installs an OnReleased before the Dashboard wraps it; both must run.
	reg.OnReleased = func(p focus.Priority) { released.Add(1) }
	// Simulate the Dashboard's wrap (it preserves the prior callback).
	prev := reg.OnReleased
	reg.OnReleased = func(p focus.Priority) {
		prev(p)
		released.Add(10) // the wrapper's own work
	}
	claim := reg.Claim(focus.Music, &fakeHolder{intents: nil})
	claim.Release()
	if released.Load() != 11 {
		t.Fatalf("released = %d, want 11 (prior + wrapper)", released.Load())
	}
}
