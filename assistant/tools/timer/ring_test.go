package timer_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// TestRingSeamTakesOverFromTheSpokenAlert proves the seam #52 will attach to:
// a handled Ring suppresses today's single spoken line, and an unhandled one
// falls back to it.
func TestRingSeamTakesOverFromTheSpokenAlert(t *testing.T) {
	for _, test := range []struct {
		name    string
		handled bool
		wantSay bool
	}{
		{name: "handled by ring", handled: true, wantSay: false},
		{name: "falls back to speech", handled: false, wantSay: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			said := make(chan string, 2)
			say := func(_ context.Context, text string) error {
				said <- text
				return nil
			}
			manager := timer.NewManagerText(time.Now, say, slog.New(slog.NewTextHandler(io.Discard, nil)), "Done.")
			rung := make(chan timer.Timer, 1)
			manager.SetRing(func(_ context.Context, fired timer.Timer) bool {
				rung <- fired
				return test.handled
			})
			defer manager.Close()

			created, err := manager.Start("tea", 10*time.Millisecond)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			select {
			case fired := <-rung:
				if fired.ID != created.ID || fired.Label != "tea" {
					t.Fatalf("ring saw %+v, want the tea timer %s", fired, created.ID)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Ring callback did not run")
			}

			if test.wantSay {
				select {
				case text := <-said:
					if text != "Tea timer is done." {
						t.Fatalf("spoken = %q", text)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("fallback spoken alert did not run")
				}
			} else {
				select {
				case text := <-said:
					t.Fatalf("handled Ring still spoke %q", text)
				case <-time.After(150 * time.Millisecond):
				}
			}
		})
	}
}

// TestClearRingRestoresTheSpokenAlert keeps the seam optional for every
// existing Manager caller.
func TestClearRingRestoresTheSpokenAlert(t *testing.T) {
	said := make(chan string, 1)
	manager := timer.NewManagerText(time.Now, func(_ context.Context, text string) error {
		said <- text
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), "Done.")
	manager.SetRing(func(context.Context, timer.Timer) bool { return true })
	manager.SetRing(nil)
	defer manager.Close()

	if _, err := manager.Start("", 10*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case text := <-said:
		if text != "Done." {
			t.Fatalf("spoken = %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("spoken alert did not run after clearing the Ring")
	}
}

// TestRingingRingHoldsFocusAndStopCancelsTheSound ties the #55 Ring seam to
// #44 Focus ownership: a stop cancels the ring context rather than only muting
// it. #52 attaches its looping sound here.
func TestRingingRingHoldsFocusAndStopCancelsTheSound(t *testing.T) {
	var registry focus.Registry
	ringStarted := make(chan struct{})
	ringCancelled := make(chan struct{})
	var once sync.Once
	manager := timer.NewManager(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFocus(&registry)
	manager.SetRing(func(ctx context.Context, _ timer.Timer) bool {
		once.Do(func() { close(ringStarted) })
		<-ctx.Done()
		close(ringCancelled)
		return true
	})
	defer manager.Close()
	manager.Set(10 * time.Millisecond)

	select {
	case <-ringStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring callback never started")
	}
	text, handled := registry.HandleIntent(context.Background(), focus.Stop)
	if !handled || text != timer.StopText {
		t.Fatalf("stop = %q, %v; want the ringing Ring to handle it", text, handled)
	}
	select {
	case <-ringCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring sound kept playing after stop")
	}
}

// TestOlderRingReleaseKeepsTheNewerClaim proves a finished older alert cannot
// clear the newer Ring holder, so the newest claim still receives stop.
func TestOlderRingReleaseKeepsTheNewerClaim(t *testing.T) {
	var registry focus.Registry
	firstStarted := make(chan struct{})
	firstReturned := make(chan struct{})
	firstGate := make(chan struct{})
	secondStarted := make(chan struct{})
	secondCancelled := make(chan struct{})
	var onceFirst, onceSecond sync.Once
	manager := timer.NewManager(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFocus(&registry)
	manager.SetRing(func(ctx context.Context, fired timer.Timer) bool {
		if fired.Label == "first" {
			onceFirst.Do(func() { close(firstStarted) })
			<-firstGate
			close(firstReturned)
			return true
		}
		onceSecond.Do(func() { close(secondStarted) })
		<-ctx.Done()
		close(secondCancelled)
		return true
	})
	defer manager.Close()
	if _, err := manager.Start("first", 10*time.Millisecond); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the first Ring never started")
	}
	if _, err := manager.Start("second", 10*time.Millisecond); err != nil {
		t.Fatalf("Start second: %v", err)
	}
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the second Ring never started")
	}
	close(firstGate)
	<-firstReturned
	// The older alert releases its token in a defer after its Ring returns.
	time.Sleep(20 * time.Millisecond)

	text, handled := registry.HandleIntent(context.Background(), focus.Stop)
	if !handled || text != timer.StopText {
		t.Fatalf("stop = %q, %v; want the newer Ring claim to survive", text, handled)
	}
	select {
	case <-secondCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the newer Ring sound kept playing after stop")
	}
}
