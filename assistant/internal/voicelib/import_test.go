package voicelib_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib/voicelibtest"
)

// stateBytes builds one structurally valid state for an upload reader.
func stateBytes(t *testing.T, contextLen int, withPads bool) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.safetensors")
	if err := voicelibtest.WriteValid(path, contextLen, withPads); err != nil {
		t.Fatalf("WriteValid: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return raw
}

func TestStateNameConfinesTheFilename(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		ok   bool
	}{
		{"jarvis.safetensors", "jarvis", true},
		{"My Voice.SAFETENSORS", "My Voice", true},
		{"../jarvis.safetensors", "jarvis", true},
		{"bad/name.safetensors", "name", true},
		{"jarvis.pt", "", false},
		{"jarvis.pth", "", false},
		{"jarvis.wav", "", false},
		{"jarvis", "", false},
		{".hidden.safetensors", "", false},
		{".safetensors", "", false},
		{`bad\name.safetensors`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := voicelib.StateName(tc.name)
			if tc.ok {
				if err != nil || got != tc.want {
					t.Fatalf("StateName(%q) = %q, %v; want %q", tc.name, got, err, tc.want)
				}
				return
			}
			if err == nil || !errors.Is(err, voicelib.ErrUnsupportedState) {
				t.Fatalf("StateName(%q) = %q, %v; want ErrUnsupportedState", tc.name, got, err)
			}
		})
	}
}

func TestImportRejectsBadFilesBeforeTTS(t *testing.T) {
	dir := t.TempDir()
	svc, _ := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1"}

	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"voice.pt", stateBytes(t, 2, true), voicelib.ErrUnsupportedState},
		{"voice.pth", stateBytes(t, 2, true), voicelib.ErrUnsupportedState},
		{"voice.wav", stateBytes(t, 2, true), voicelib.ErrUnsupportedState},
		{"voice.safetensors", []byte("not a safetensors header"), voicelib.ErrInvalidState},
		{"empty.safetensors", nil, voicelib.ErrInvalidState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Import(context.Background(), bytes.NewReader(tc.data), tc.name, voicelib.ImportOptions{Probe: probe})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Import error = %v, want %v", err, tc.want)
			}
		})
	}
	if len(probe.validated) != 0 {
		t.Fatalf("TTS was called for rejected files: %v", probe.validated)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("rejected upload left staging %q", entry.Name())
		}
	}
}

func TestImportRejectsOversizeBeforeTTS(t *testing.T) {
	dir := t.TempDir()
	svc, _ := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1"}
	oversize := make([]byte, voicelib.MaxStateBytes+1)
	_, err := svc.Import(context.Background(), bytes.NewReader(oversize), "huge.safetensors", voicelib.ImportOptions{Probe: probe})
	if !errors.Is(err, voicelib.ErrStateTooLarge) {
		t.Fatalf("Import error = %v, want ErrStateTooLarge", err)
	}
	if len(probe.validated) != 0 {
		t.Fatalf("TTS was called for an oversize file: %v", probe.validated)
	}
}

func TestImportPublishesAValidatedStateAndRecordsItsRevision(t *testing.T) {
	for _, withPads := range []bool{false, true} {
		name := "twelve"
		if withPads {
			name = "eighteen"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			svc, _ := openLibrary(t, dir)
			probe := &fakeProbe{revision: "pocket-tts 4.0.1 english abc123"}

			voice, err := svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 3, withPads)), "Jarvis.safetensors", voicelib.ImportOptions{Probe: probe})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if voice.Name != "Jarvis" || !voice.Ready() || voice.ValidatedRevision != probe.revision {
				t.Fatalf("voice = %+v", voice)
			}
			if _, err := os.Stat(filepath.Join(dir, "Jarvis.safetensors")); err != nil {
				t.Fatalf("published file: %v", err)
			}
			// One ephemeral validation of the staging path, then one final-path
			// validation through Rescan; the final path is explicitly forgotten
			// first so no stale cache bytes can be reused.
			if len(probe.ephemeral) != 1 || !strings.HasPrefix(filepath.Base(probe.ephemeral[0]), ".upload-") {
				t.Fatalf("ephemeral validations = %v", probe.ephemeral)
			}
			if len(probe.validated) != 2 || probe.validated[1] != "/voices/Jarvis.safetensors" {
				t.Fatalf("validations = %v", probe.validated)
			}
			if len(probe.forgotten) != 1 || probe.forgotten[0] != "/voices/Jarvis.safetensors" {
				t.Fatalf("forgotten = %v", probe.forgotten)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") {
					t.Fatalf("upload left staging %q", entry.Name())
				}
			}
		})
	}
}

func TestImportRejectsAnExistingNameWithoutReplace(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "jarvis.safetensors")
	if err := voicelibtest.WriteValid(old, 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	before, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 3, true)), "jarvis.safetensors", voicelib.ImportOptions{Probe: probe})
	if !errors.Is(err, voicelib.ErrVoiceExists) {
		t.Fatalf("Import error = %v, want ErrVoiceExists", err)
	}
	after, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected replacement changed the existing file")
	}
	if len(probe.ephemeral) != 0 {
		t.Fatalf("rejected replacement reached TTS: %v", probe.ephemeral)
	}
}

func TestImportReplacesOnlyAfterValidationAndEvictsTheCache(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "jarvis.safetensors")
	if err := voicelibtest.WriteValid(old, 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	first := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), first); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	before, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}

	replacement := &fakeProbe{revision: "rev-1"}
	voice, err := svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 4, false)), "jarvis.safetensors", voicelib.ImportOptions{Probe: replacement, Replace: true})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !voice.Ready() {
		t.Fatalf("replaced voice = %+v", voice)
	}
	after, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("replacement did not change the published file")
	}
	if len(replacement.forgotten) != 1 || replacement.forgotten[0] != "/voices/jarvis.safetensors" {
		t.Fatalf("forgotten = %v", replacement.forgotten)
	}
	if len(replacement.validated) != 2 || replacement.validated[0] == replacement.validated[1] {
		t.Fatalf("validations = %v", replacement.validated)
	}
}

func TestImportRollsBackAReplacementThatFailsAtTheFinalPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "jarvis.safetensors")
	if err := voicelibtest.WriteValid(target, 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	first := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), first); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := svc.Get("jarvis")

	// The staging smoke test passes, but the final-path validation rejects the
	// bytes. Rescan records the failure and Import must restore file and row.
	failing := &fakeProbe{revision: "rev-1", validateErr: errors.New("model rejected the state")}
	_, err = svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 4, false)), "jarvis.safetensors", voicelib.ImportOptions{Probe: failing, Replace: true})
	if !errors.Is(err, voicelib.ErrSmokeFailed) {
		t.Fatalf("Import error = %v, want ErrSmokeFailed", err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rollback did not restore the previous bytes")
	}
	got, ok := svc.Get("jarvis")
	if !ok || !got.Ready() || got.ValidatedRevision != old.ValidatedRevision || got.Size != old.Size {
		t.Fatalf("rollback index = %+v, want %+v", got, old)
	}
	if len(failing.forgotten) < 2 || failing.forgotten[len(failing.forgotten)-1] != "/voices/jarvis.safetensors" {
		t.Fatalf("rollback did not evict the failed final path cache: %v", failing.forgotten)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("failed replacement left %q", entry.Name())
		}
	}
}

func TestImportCleansUpWhenTheSmokeTestFails(t *testing.T) {
	dir := t.TempDir()
	svc, _ := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1", ephemeralErr: errors.New("model could not load the state")}
	_, err := svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 2, true)), "jarvis.safetensors", voicelib.ImportOptions{Probe: probe})
	if !errors.Is(err, voicelib.ErrSmokeFailed) {
		t.Fatalf("Import error = %v, want ErrSmokeFailed", err)
	}
	if _, ok := svc.Get("jarvis"); ok {
		t.Fatal("failed upload entered the index")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Fatalf("failed upload left %q", entry.Name())
	}
}

func TestImportCleansUpWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	svc, _ := openLibrary(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe := &fakeProbe{revision: "rev-1"}
	_, err := svc.Import(ctx, bytes.NewReader(stateBytes(t, 2, true)), "jarvis.safetensors", voicelib.ImportOptions{Probe: probe})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Import error = %v, want context.Canceled", err)
	}
	if len(probe.validated) != 0 {
		t.Fatalf("cancelled upload reached TTS: %v", probe.validated)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Fatalf("cancelled upload left %q", entry.Name())
	}
}

func TestImportRequiresStagingAndReplacementExtensions(t *testing.T) {
	dir := t.TempDir()
	svc, _ := openLibrary(t, dir)

	// A legacy probe that only implements Revision/Validate cannot prove a
	// staging path will not be cached, so an upload is refused.
	legacy := &legacyProbe{revision: "rev-1"}
	_, err := svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 2, true)), "jarvis.safetensors", voicelib.ImportOptions{Probe: legacy})
	if !errors.Is(err, voicelib.ErrStagingUnsupported) {
		t.Fatalf("Import error = %v, want ErrStagingUnsupported", err)
	}

	// A probe without Forget cannot evict path-keyed cache bytes, so replacing
	// an existing file is refused rather than silently serving the old state.
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	noForget := &noForgetProbe{revision: "rev-1"}
	_, err = svc.Import(context.Background(), bytes.NewReader(stateBytes(t, 3, true)), "jarvis.safetensors", voicelib.ImportOptions{Probe: noForget, Replace: true})
	if !errors.Is(err, voicelib.ErrReplaceUnsupported) {
		t.Fatalf("Import error = %v, want ErrReplaceUnsupported", err)
	}
}

// legacyProbe implements only the base interface, like an old test double.
type legacyProbe struct{ revision string }

func (p *legacyProbe) Revision(context.Context) (string, error) { return p.revision, nil }
func (p *legacyProbe) Validate(context.Context, string) (string, error) {
	return p.revision, nil
}

// noForgetProbe adds ephemeral validation but deliberately no Forget.
type noForgetProbe struct{ revision string }

func (p *noForgetProbe) Revision(context.Context) (string, error) { return p.revision, nil }
func (p *noForgetProbe) Validate(context.Context, string) (string, error) {
	return p.revision, nil
}
func (p *noForgetProbe) ValidateEphemeral(context.Context, string) (string, error) {
	return p.revision, nil
}
