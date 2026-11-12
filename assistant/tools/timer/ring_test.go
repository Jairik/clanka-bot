package timer_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

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
			manager.SetRing(func(fired timer.Timer) bool {
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
	manager.SetRing(func(timer.Timer) bool { return true })
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
