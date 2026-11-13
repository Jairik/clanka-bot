package voicelib_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib/voicelibtest"
)

// openLibrary builds a real SQLite-backed service over a temp folder.
func openLibrary(t *testing.T, dir string) (*voicelib.Service, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := voicelib.New(context.Background(), st, voicelib.Config{
		Dir:      dir,
		TTSDir:   "/voices",
		Default:  "alba",
		Builtins: []string{"alba", "marius"},
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	return svc, st
}

// bindSettings creates the two settings definitions and binds the service.
func bindSettings(t *testing.T, svc *voicelib.Service, st *store.Store) *settings.Service {
	t.Helper()
	settingsSvc, err := settings.New(context.Background(), st, svc.Definitions(voicelib.DefinitionConfig{Name: "alba", Rate: 1.0})...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	svc.Bind(settingsSvc)
	return settingsSvc
}

// fakeProbe records validations and returns a fixed revision. It implements
// both upload extensions: ephemeral staging validation and cache eviction for
// replacement.
type fakeProbe struct {
	revision     string
	revisionErr  error
	validateErr  error
	ephemeralErr error

	mu             sync.Mutex
	validated      []string
	ephemeral      []string
	forgotten      []string
	validatedCache []bool
	calls          map[string]int
}

func (p *fakeProbe) Revision(context.Context) (string, error) {
	return p.revision, p.revisionErr
}

func (p *fakeProbe) Validate(_ context.Context, path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[path]++
	p.validated = append(p.validated, path)
	p.validatedCache = append(p.validatedCache, true)
	if p.validateErr != nil {
		return "", p.validateErr
	}
	return p.revision, nil
}

func (p *fakeProbe) ValidateEphemeral(_ context.Context, path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[path]++
	p.validated = append(p.validated, path)
	p.ephemeral = append(p.ephemeral, path)
	p.validatedCache = append(p.validatedCache, false)
	if p.ephemeralErr != nil {
		return "", p.ephemeralErr
	}
	return p.revision, nil
}

func (p *fakeProbe) Forget(_ context.Context, path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forgotten = append(p.forgotten, path)
	return nil
}

func TestInspectStateAcceptsRealExportShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layers int
		heads  int
	}{
		{"100M", 6, 16},
		{"18M", 8, 8},
		{"7M", 8, 8},
		{"2M", 8, 3},
	} {
		for _, withPads := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pads=%v", tc.name, withPads), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "voice.safetensors")
				if err := voicelibtest.WriteValidLayout(path, 127, tc.layers, tc.heads, withPads); err != nil {
					t.Fatalf("write state: %v", err)
				}
				info, err := voicelib.InspectState(path)
				if err != nil {
					t.Fatalf("InspectState: %v", err)
				}
				wantTensors := tc.layers * 2
				if withPads {
					wantTensors = tc.layers * 3
				}
				if info.Layers != tc.layers || info.ContextLen != 127 || info.Tensors != wantTensors {
					t.Fatalf("InspectState = %+v, want layers=%d context=127 tensors=%d", info, tc.layers, wantTensors)
				}
			})
		}
	}
}

func TestInspectStateAcceptsThePinnedRealExportWhenProvided(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("VOICE_STATE_TEST_FILE"))
	if path == "" {
		t.Skip("set VOICE_STATE_TEST_FILE to a real Pocket TTS exported .safetensors to check its shape")
	}
	info, err := voicelib.InspectState(path)
	if err != nil {
		t.Fatalf("InspectState(%s): %v", path, err)
	}
	if info.Layers < 1 || info.ContextLen < 1 || info.Tensors != info.Layers*2 && info.Tensors != info.Layers*3 {
		t.Fatalf("InspectState = %+v, want a real export", info)
	}
}

func TestInspectStateRejectsUnsupportedAndMalformedFiles(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "valid.safetensors")
	if err := voicelibtest.WriteValid(valid, 2, true); err != nil {
		t.Fatalf("write valid: %v", err)
	}

	t.Run("pickle extension", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "voice.pt")
		raw, err := os.ReadFile(valid)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted .pt")
		}
	})

	t.Run("over the size limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "huge.safetensors")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(voicelib.MaxStateBytes + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted an oversized file")
		}
	})

	t.Run("unknown tensor", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unknown.safetensors")
		if err := voicelibtest.Write(path, map[string]voicelibtest.Tensor{
			"transformer.layers.0.self_attn/cache": {Dtype: "F32", Shape: []int64{2, 1, 1, 16, 64}, Data: make([]byte, 8192)},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted a one-tensor file")
		}
	})

	t.Run("cache dtype", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dtype.safetensors")
		if err := voicelibtest.WriteValid(path, 2, true); err != nil {
			t.Fatal(err)
		}
		rewriteTensor(t, path, "transformer.layers.0.self_attn/cache", "F16", []int64{2, 1, 2, 16, 64}, 4096)
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted an F16 cache")
		}
	})

	t.Run("context too long", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "long.safetensors")
		if err := voicelibtest.WriteValid(path, 401, false); err != nil {
			t.Fatal(err)
		}
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted N > 400")
		}
	})

	t.Run("inconsistent head count", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "heads.safetensors")
		if err := voicelibtest.WriteValidLayout(path, 2, 8, 8, true); err != nil {
			t.Fatal(err)
		}
		rewriteTensor(t, path, "transformer.layers.0.self_attn/cache", "F32", []int64{2, 1, 2, 3, 64}, 0)
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted inconsistent head counts")
		}
	})

	t.Run("offset dtype", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "offset.safetensors")
		if err := voicelibtest.WriteValid(path, 2, true); err != nil {
			t.Fatal(err)
		}
		rewriteTensor(t, path, "transformer.layers.0.self_attn/offset", "I32", []int64{1}, 4)
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted an I32 offset")
		}
	})

	t.Run("truncated data", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "truncated.safetensors")
		if err := voicelibtest.WriteValid(path, 2, true); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, info.Size()-1); err != nil {
			t.Fatal(err)
		}
		if _, err := voicelib.InspectState(path); err == nil {
			t.Fatal("InspectState accepted truncated data")
		}
	})
}

func TestRescanIndexesValidStatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, st := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if len(probe.validated) != 1 || probe.validated[0] != "/voices/jarvis.safetensors" {
		t.Fatalf("validated = %v", probe.validated)
	}
	voice, ok := svc.Get("jarvis")
	if !ok || !voice.Ready() || voice.ValidatedRevision != "rev-1" {
		t.Fatalf("jarvis = %+v ok=%v", voice, ok)
	}
	if ref, err := svc.SpeechRef("jarvis"); err != nil || ref != "/voices/jarvis.safetensors" {
		t.Fatalf("SpeechRef = %q, %v", ref, err)
	}
	candidates := svc.Candidates()
	if !contains(candidates, "jarvis") || !contains(candidates, "alba") {
		t.Fatalf("candidates = %v", candidates)
	}

	// Reopen the database and bind a fresh settings service; the row, revision
	// and added time survive.
	reopened, err := voicelib.New(context.Background(), st, voicelib.Config{
		Dir: dir, TTSDir: "/voices", Default: "alba", Builtins: []string{"alba"},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	again, ok := reopened.Get("jarvis")
	if !ok || again.ValidatedRevision != "rev-1" || again.AddedAt.IsZero() {
		t.Fatalf("reopened jarvis = %+v", again)
	}
}

func TestRescanSkipsUnsupportedInvalidAndEscapingFiles(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.safetensors")
	if err := voicelibtest.WriteValid(valid, 2, true); err != nil {
		t.Fatal(err)
	}
	// A custom file may not shadow the built-in alba.
	if err := voicelibtest.WriteValid(filepath.Join(dir, "alba.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	// A .pt file with valid safetensors bytes is still refused by extension.
	raw, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pickle.pt"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.safetensors"), []byte("not safetensors"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.safetensors")
	if err := voicelibtest.WriteValid(outside, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.safetensors")); err != nil {
		t.Fatal(err)
	}

	svc, _ := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if len(probe.validated) != 1 || probe.validated[0] != "/voices/valid.safetensors" {
		t.Fatalf("validated = %v", probe.validated)
	}
	for _, name := range []string{"pickle", "broken", "escape"} {
		if _, ok := svc.Get(name); ok {
			t.Fatalf("%s was indexed", name)
		}
	}
	if got, _ := svc.Get("alba"); got.Builtin != true {
		t.Fatalf("alba resolved to a custom row: %+v", got)
	}
}

func TestRescanMarksUnchangedVoiceStaleOnNewRevision(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	first := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), first); err != nil {
		t.Fatalf("first Rescan: %v", err)
	}
	second := &fakeProbe{revision: "rev-2"}
	if err := svc.Rescan(context.Background(), second); err != nil {
		t.Fatalf("second Rescan: %v", err)
	}
	if len(second.validated) != 0 {
		t.Fatalf("unchanged voice was smoke-tested again: %v", second.validated)
	}
	voice, _ := svc.Get("jarvis")
	if !voice.Stale || voice.Ready() || voice.ValidatedRevision != "rev-1" {
		t.Fatalf("jarvis after revision change = %+v", voice)
	}
	if _, err := svc.SpeechRef("jarvis"); !errors.Is(err, voicelib.ErrStaleVoice) {
		t.Fatalf("SpeechRef stale error = %v", err)
	}

	// A TTS rollback to the recorded revision makes the same unchanged file
	// usable again; stale is not a permanent label.
	back := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), back); err != nil {
		t.Fatalf("rollback Rescan: %v", err)
	}
	if voice, _ := svc.Get("jarvis"); voice.Stale || !voice.Ready() {
		t.Fatalf("jarvis after rollback = %+v", voice)
	}
}

func TestNewCleansCrashStagingFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".upload-123.safetensors", ".replace-456.bak"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("left by a crash"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	visible := filepath.Join(dir, "jarvis.safetensors")
	if err := voicelibtest.WriteValid(visible, 2, true); err != nil {
		t.Fatal(err)
	}
	openLibrary(t, dir)
	for _, name := range []string{".upload-123.safetensors", ".replace-456.bak"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging file %q survived New: %v", name, err)
		}
	}
	if _, err := os.Stat(visible); err != nil {
		t.Fatalf("visible state was removed: %v", err)
	}
}

func TestRescanSkipsHiddenStagingAndBackupFiles(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	// An upload in flight and a replacement backup are both hidden; a scan
	// must never index either as a selectable voice.
	if err := voicelibtest.WriteValid(filepath.Join(dir, ".upload-123.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	if err := voicelibtest.WriteValid(filepath.Join(dir, ".replace-456.bak"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	probe := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, ok := svc.Get(".upload-123"); ok {
		t.Fatal("rescan indexed an upload staging file")
	}
	if _, ok := svc.Get(".replace-456"); ok {
		t.Fatal("rescan indexed a replacement backup")
	}
	if voice, ok := svc.Get("jarvis"); !ok || !voice.Ready() {
		t.Fatalf("jarvis = %+v, %v", voice, ok)
	}
	if len(probe.validated) != 1 {
		t.Fatalf("validations = %v, want only jarvis", probe.validated)
	}
}

func TestRescanRevalidatesAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jarvis.safetensors")
	if err := voicelibtest.WriteValid(path, 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	first := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), first); err != nil {
		t.Fatalf("first Rescan: %v", err)
	}
	// A new export with a different context length is a different file.
	if err := voicelibtest.WriteValid(path, 3, true); err != nil {
		t.Fatal(err)
	}
	second := &fakeProbe{revision: "rev-1"}
	if err := svc.Rescan(context.Background(), second); err != nil {
		t.Fatalf("second Rescan: %v", err)
	}
	if len(second.validated) != 1 {
		t.Fatalf("replaced file was not revalidated: %v", second.validated)
	}
	if voice, _ := svc.Get("jarvis"); !voice.Ready() {
		t.Fatalf("replaced jarvis is not ready: %+v", voice)
	}
}

func TestRescanWithoutProbeLeavesNewFilesUnvalidated(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, st := openLibrary(t, dir)
	bindSettings(t, svc, st)
	if err := svc.Rescan(context.Background(), nil); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	voice, ok := svc.Get("jarvis")
	if !ok || voice.Ready() || voice.Status() != "unvalidated" {
		t.Fatalf("jarvis = %+v", voice)
	}
	if _, err := svc.SetActive(context.Background(), "jarvis"); !errors.Is(err, voicelib.ErrNotValidated) {
		t.Fatalf("SetActive error = %v", err)
	}
}

func TestEnsureActiveFallsBackOnceAndPersistsTheDefault(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, st := openLibrary(t, dir)
	settingsSvc := bindSettings(t, svc, st)
	if err := svc.Rescan(context.Background(), &fakeProbe{revision: "rev-1"}); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, err := svc.SetActive(context.Background(), "jarvis"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	// A new process sees the pinned model revision move. The unchanged voice is
	// stale, so the persisted active voice falls back exactly once.
	reopened, err := voicelib.New(context.Background(), st, voicelib.Config{
		Dir: dir, TTSDir: "/voices", Default: "alba", Builtins: []string{"alba"},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopenedSettings, err := settings.New(context.Background(), st, reopened.Definitions(voicelib.DefinitionConfig{Name: "alba", Rate: 1.0})...)
	if err != nil {
		t.Fatalf("reopened settings: %v", err)
	}
	reopened.Bind(reopenedSettings)
	if err := reopened.Rescan(context.Background(), &fakeProbe{revision: "rev-2"}); err != nil {
		t.Fatalf("reopened Rescan: %v", err)
	}
	fallback, err := reopened.EnsureActive(context.Background())
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}
	if fallback == nil || fallback.From != "jarvis" || fallback.To != "alba" {
		t.Fatalf("fallback = %+v", fallback)
	}
	if got := reopenedSettings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("stored voice = %q, want alba", got)
	}
	again, err := reopened.EnsureActive(context.Background())
	if err != nil || again != nil {
		t.Fatalf("second EnsureActive = %+v, %v; want nil, nil", again, err)
	}
	_ = settingsSvc
}

func TestDefinitionsRateAndNameValidation(t *testing.T) {
	dir := t.TempDir()
	svc, st := openLibrary(t, dir)
	settingsSvc := bindSettings(t, svc, st)
	if got := settingsSvc.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("default name = %q", got)
	}
	if got := settingsSvc.Float(voicelib.KeyRate); got != 1.0 {
		t.Fatalf("default rate = %v", got)
	}
	if _, err := settingsSvc.Set(context.Background(), voicelib.KeyRate, 2.5); err == nil {
		t.Fatal("rate 2.5 was accepted")
	}
	if _, err := settingsSvc.Set(context.Background(), voicelib.KeyRate, 0.4); err == nil {
		t.Fatal("rate 0.4 was accepted")
	}
	if _, err := settingsSvc.Set(context.Background(), voicelib.KeyName, " "); err == nil {
		t.Fatal("blank voice name was accepted")
	}
	rate, err := svc.SetRate(context.Background(), 1.234)
	if err != nil || rate != 1.23 {
		t.Fatalf("SetRate = %v, %v", rate, err)
	}
	rate, err = svc.SetRate(context.Background(), 9)
	if err != nil || rate != 2.0 {
		t.Fatalf("SetRate clamps = %v, %v", rate, err)
	}
}

func TestActiveSpeechAndNote(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, st := openLibrary(t, dir)
	bindSettings(t, svc, st)
	if err := svc.Rescan(context.Background(), &fakeProbe{revision: "rev-1"}); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, err := svc.SetActive(context.Background(), "jarvis"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if _, err := svc.SetRate(context.Background(), 1.2); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	ref, rate, ok := svc.ActiveSpeech()
	if !ok || ref != "/voices/jarvis.safetensors" || rate != 1.2 {
		t.Fatalf("ActiveSpeech = %q, %v, %v", ref, rate, ok)
	}
	if err := svc.SetNote(context.Background(), "jarvis", "recorded with permission, 2026-11-12"); err != nil {
		t.Fatalf("SetNote: %v", err)
	}
	voice, _ := svc.Get("jarvis")
	if voice.Note != "recorded with permission, 2026-11-12" {
		t.Fatalf("note = %q", voice.Note)
	}
}

func TestConcurrentListAndRescan(t *testing.T) {
	dir := t.TempDir()
	if err := voicelibtest.WriteValid(filepath.Join(dir, "jarvis.safetensors"), 2, true); err != nil {
		t.Fatal(err)
	}
	svc, _ := openLibrary(t, dir)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = svc.List()
				_ = svc.Candidates()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = svc.Rescan(context.Background(), &fakeProbe{revision: "rev-1"})
			}
		}()
	}
	wg.Wait()
}

// rewriteTensor replaces one header entry so a malformed-shape test can keep
// the rest of the fixture valid.
func rewriteTensor(t *testing.T, path, key, dtype string, shape []int64, size int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	headerLen := int(uint64(raw[0]) | uint64(raw[1])<<8 | uint64(raw[2])<<16 | uint64(raw[3])<<24 |
		uint64(raw[4])<<32 | uint64(raw[5])<<40 | uint64(raw[6])<<48 | uint64(raw[7])<<56)
	var header map[string]map[string]any
	if err := json.Unmarshal(raw[8:8+headerLen], &header); err != nil {
		t.Fatal(err)
	}
	entry := header[key]
	entry["dtype"] = dtype
	entry["shape"] = shape
	// Keep the original data range; the validator must reject the mismatch.
	_ = size
	updated, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != headerLen {
		t.Fatalf("rewritten header length %d != %d", len(updated), headerLen)
	}
	copy(raw[8:8+headerLen], updated)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
