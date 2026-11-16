package spotify_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	spotifytools "github.com/Jairik/clanka-bot/assistant/tools/spotify"
)

// TestSpotifyToolTimeoutsAreRegistered proves the playback Tools report their
// explicit Timeout from the registry, not the 2s default (#207). It registers
// the real Tools and reads their Info, so a regression that drops the field
// (or a Tool accidentally left on the default) fails here.
func TestSpotifyToolTimeoutsAreRegistered(t *testing.T) {
	const registryDefault = 2 * time.Second
	r := tool.NewRegistry(registryDefault)
	if err := spotifytools.RegisterTools(r, newRig(t, 0).svc); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	want := map[string]time.Duration{
		spotifytools.PlayName:         6 * time.Second,
		spotifytools.PlayPlaylistName: 6 * time.Second,
		spotifytools.QueueAddName:     6 * time.Second,
		spotifytools.LikeName:         6 * time.Second,
		spotifytools.VolumeName:       6 * time.Second,
		spotifytools.PauseName:        4 * time.Second,
		spotifytools.NextName:         4 * time.Second,
		spotifytools.NowPlayingName:   4 * time.Second,
	}
	for name, wantTO := range want {
		info, ok := r.Info(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if info.Timeout != wantTO {
			t.Errorf("%s timeout = %s, want %s", name, info.Timeout, wantTO)
		}
	}
	// The single-call Tools were deliberately left on the registry default.
	for _, name := range []string{
		spotifytools.SleepName,
		spotifytools.ShuffleName,
		spotifytools.RepeatName,
		spotifytools.PreviousName,
	} {
		info, ok := r.Info(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if info.Timeout != registryDefault {
			t.Errorf("%s timeout = %s, want registry default %s", name, info.Timeout, registryDefault)
		}
	}
}

// TestPlaySucceedsWithinTheTimeoutEnvelope drives spotify.play through the
// registry (so the 6s Tool envelope is really applied) against a fake API that
// adds 500ms per call. A search plus a volume cap is four round trips
// (devices, search, cap, play) = 2s, well inside the envelope (#207).
func TestPlaySucceedsWithinTheTimeoutEnvelope(t *testing.T) {
	r := newRig(t, 55) // cap 55 so the requested 90 is clamped (a fourth call)
	r.api.search = radiohead
	r.api.delay = 500 * time.Millisecond

	reg := tool.NewRegistry(time.Second) // Tool timeout overrides this default
	if err := spotifytools.RegisterTools(reg, r.svc); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	ninety := 90
	args, _ := json.Marshal(spotifytools.PlayIn{Query: "Radiohead", Volume: &ninety})

	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := reg.Execute(ctx, spotifytools.PlayName, args); err != nil {
		t.Fatalf("play within envelope: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("play took %s, outside the 6s envelope", elapsed)
	}
}

// TestDeadlineReturnsTimeoutLineNotUnreachable proves a context deadline maps
// to its own spoken line ("Spotify is taking too long to answer.") and not the
// generic "I couldn't reach Spotify." The hang fake forces the registry's 4s
// pause envelope to expire mid-chain (#207).
func TestDeadlineReturnsTimeoutLineNotUnreachable(t *testing.T) {
	r := newRig(t, 0)
	r.api.hang = true

	reg := tool.NewRegistry(time.Second)
	if err := spotifytools.RegisterTools(reg, r.svc); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	_, err := reg.Execute(t.Context(), spotifytools.PauseName, nil)
	if err == nil {
		t.Fatal("expected an error from the deadline")
	}
	msg := tool.FailureMessage(err)
	if strings.Contains(msg, "couldn't reach") {
		t.Fatalf("deadline mapped to the unreachable line: %q", msg)
	}
	if msg != "Spotify is taking too long to answer." {
		t.Fatalf("timeout line = %q, want %q", msg, "Spotify is taking too long to answer.")
	}
}
