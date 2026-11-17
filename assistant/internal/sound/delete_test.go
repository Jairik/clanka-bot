package sound_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

func TestDeleteRemovesFileAndIndexRow(t *testing.T) {
	f := newFixture(t, sound.Config{})
	path := writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 200*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if err := f.sounds.Delete(context.Background(), "birds"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := f.sounds.Find("birds"); ok {
		t.Fatal("deleted sound is still in the library")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists: %v", err)
	}
	if err := f.sounds.Delete(context.Background(), "birds"); !errors.Is(err, sound.ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestDeleteRefusesBuiltinAndAssignedSounds(t *testing.T) {
	f := newFixture(t, sound.Config{})
	path := writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 200*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	ctx := context.Background()
	if err := f.sounds.Delete(ctx, sound.BuiltinChime); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("Delete(chime) = %v, want a built-in refusal", err)
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleWake), "birds"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := f.sounds.Delete(ctx, "birds"); err == nil || !strings.Contains(err.Error(), "wake") {
		t.Fatalf("Delete(assigned) = %v, want the role named", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refused delete removed the file: %v", err)
	}
	if _, ok := f.sounds.Find("birds"); !ok {
		t.Fatal("refused delete dropped the index row")
	}
}
