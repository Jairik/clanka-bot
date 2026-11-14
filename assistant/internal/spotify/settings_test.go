package spotify_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

func TestMaxVolumeIsUnsetByDefaultAndLive(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := settings.New(ctx, st, spotify.Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	if got := spotify.MaxVolume(svc); got != 0 {
		t.Fatalf("default cap = %d, want unset (0)", got)
	}
	if _, err := svc.Set(ctx, spotify.MaxVolumeKey, 65); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := spotify.MaxVolume(svc); got != 65 {
		t.Fatalf("cap = %d, want 65 live", got)
	}
	if _, err := svc.Set(ctx, spotify.MaxVolumeKey, 150); err == nil {
		t.Fatal("accepted a cap above 100")
	}
	if got := spotify.MaxVolume(nil); got != 0 {
		t.Fatalf("nil service cap = %d", got)
	}
}
