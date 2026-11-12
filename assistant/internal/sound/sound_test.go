package sound_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/settingsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

var cueFormat = audio.Format{Rate: 16000, Width: 2, Channels: 1}

// recordingSink captures every Cue write so tests can assert order and format
// without a speaker.
type recordingSink struct {
	mu      sync.Mutex
	formats []audio.Format
	writes  [][]byte
	closes  int
}

func (s *recordingSink) Open(_ context.Context, format audio.Format) (io.WriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.formats = append(s.formats, format)
	return &recordingWriter{sink: s}, nil
}

func (s *recordingSink) snapshot() ([][]byte, []audio.Format) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writes := make([][]byte, len(s.writes))
	for i, write := range s.writes {
		writes[i] = append([]byte(nil), write...)
	}
	return writes, append([]audio.Format(nil), s.formats...)
}

type recordingWriter struct{ sink *recordingSink }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()
	w.sink.writes = append(w.sink.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (w *recordingWriter) Close() error {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()
	w.sink.closes++
	return nil
}

// wavBytes builds a mono PCM16 WAV of the requested length.
func wavBytes(duration time.Duration) []byte {
	frames := int(duration * time.Duration(cueFormat.Rate) / time.Second)
	data := make([]byte, frames*2)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(1200)))
	}
	out := make([]byte, 44+len(data))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(data)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 1)
	binary.LittleEndian.PutUint32(out[24:28], uint32(cueFormat.Rate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(cueFormat.Rate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(data)))
	copy(out[44:], data)
	return out
}

func writeLibraryWAV(t *testing.T, dir, name string, duration time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, wavBytes(duration), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fixture is one real store, library and settings service over a temp volume.
type fixture struct {
	dir      string
	store    *store.Store
	sounds   *sound.Service
	settings *settings.Service
	sink     *recordingSink
}

func newFixture(t *testing.T, cfg sound.Config) *fixture {
	t.Helper()
	return newFixtureAt(t, t.TempDir(), cfg)
}

func newFixtureAt(t *testing.T, dir string, cfg sound.Config) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(dir, "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sounds, err := sound.New(context.Background(), st, dir, cueFormat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	sett, err := settings.New(context.Background(), st, sounds.Definitions(cfg)...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	sink := &recordingSink{}
	sounds.Bind(sett, sink)
	return &fixture{dir: dir, store: st, sounds: sounds, settings: sett, sink: sink}
}

func TestLibraryIndexesBuiltinAndVolumeWav(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 500*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	list := f.sounds.List()
	byName := map[string]sound.Sound{}
	for _, entry := range list {
		byName[entry.Name] = entry
	}
	chime, ok := byName[sound.BuiltinChime]
	if !ok || !chime.Builtin || chime.Duration <= 0 || chime.AddedAt.IsZero() {
		t.Fatalf("builtin chime = %+v, ok=%v", chime, ok)
	}
	birds, ok := byName["birds"]
	if !ok || birds.Builtin || birds.Duration != 500*time.Millisecond || birds.AddedAt.IsZero() {
		t.Fatalf("birds = %+v, ok=%v", birds, ok)
	}
	if filepath.Base(birds.Path) != "birds.wav" {
		t.Fatalf("birds path = %q", birds.Path)
	}

	var (
		path       string
		durationMS int64
		added      string
	)
	if err := f.store.DB().QueryRow(
		`SELECT path, duration_ms, added_at FROM sounds WHERE name = 'birds'`).Scan(&path, &durationMS, &added); err != nil {
		t.Fatalf("read sound index: %v", err)
	}
	if path != "birds.wav" || durationMS != 500 || added == "" {
		t.Fatalf("index row = %q, %d, %q", path, durationMS, added)
	}
}

func TestRescanKeepsAddedAtAndDropsRemovedFile(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 300*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("first Rescan: %v", err)
	}
	first := findSound(t, f.sounds.List(), "birds")
	// A rescan must not treat the same file as newly added.
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("second Rescan: %v", err)
	}
	second := findSound(t, f.sounds.List(), "birds")
	if !second.AddedAt.Equal(first.AddedAt) {
		t.Fatalf("added_at changed from %v to %v", first.AddedAt, second.AddedAt)
	}

	if err := os.Remove(first.Path); err != nil {
		t.Fatalf("remove sound: %v", err)
	}
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("rescan after remove: %v", err)
	}
	for _, entry := range f.sounds.List() {
		if entry.Name == "birds" {
			t.Fatalf("removed sound is still indexed: %+v", entry)
		}
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM sounds WHERE name = 'birds'`).Scan(&count); err != nil {
		t.Fatalf("count removed sound: %v", err)
	}
	if count != 0 {
		t.Fatalf("removed sound rows = %d", count)
	}
}

func TestInvalidLibraryWavIsSkipped(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeFile(t, filepath.Join(f.sounds.LibraryDir(), "bad.wav"), []byte("this is not a wav"))
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	for _, entry := range f.sounds.List() {
		if entry.Name == "bad" {
			t.Fatalf("invalid WAV was indexed: %+v", entry)
		}
	}
	if _, _, err := f.sounds.Resolve("bad"); err == nil {
		t.Fatal("Resolve accepted an invalid library name")
	}
}

func TestRoleCapsRejectRatherThanTrim(t *testing.T) {
	f := newFixture(t, sound.Config{})
	dir := f.sounds.LibraryDir()
	long := writeLibraryWAV(t, dir, "long.wav", 1500*time.Millisecond)
	just := writeLibraryWAV(t, dir, "just.wav", 1001*time.Millisecond)
	exact := writeLibraryWAV(t, dir, "exact.wav", time.Second)
	long3 := writeLibraryWAV(t, dir, "long3.wav", 4*time.Second)
	ring := writeLibraryWAV(t, dir, "ring.wav", 61*time.Second)
	edge := writeLibraryWAV(t, dir, "edge.wav", 59*time.Second)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	ctx := context.Background()
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleWake), "long"); err == nil ||
		!strings.Contains(err.Error(), "over the 1s cap") {
		t.Fatalf("wake assignment error = %v, want the 1s cap error", err)
	}
	// A millisecond over the cap is still over, even though the index column
	// could truncate it to exactly 1000 ms; exactly 1 s is accepted.
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleWake), "just"); err == nil {
		t.Fatal("1001 ms wake sound was accepted")
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleWake), "exact"); err != nil {
		t.Fatalf("exactly 1 s wake sound was rejected: %v", err)
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleReminder), "long3"); err == nil ||
		!strings.Contains(err.Error(), "over the 3s cap") {
		t.Fatalf("reminder assignment error = %v, want the 3s cap error", err)
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleError), "long3"); err == nil ||
		!strings.Contains(err.Error(), "over the 3s cap") {
		t.Fatalf("error assignment error = %v, want the 3s cap error", err)
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleTimerRing), "ring"); err == nil ||
		!strings.Contains(err.Error(), "over the 1m0s cap") {
		t.Fatalf("ring assignment error = %v, want the 1m0s cap error", err)
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleTimerRing), "edge"); err != nil {
		t.Fatalf("59 s Ring assignment was rejected: %v", err)
	}

	// Rejection never trims the source: the file still holds its full length.
	for _, path := range []string{long, just, exact, long3, ring, edge} {
		info, err := audio.InspectWAV(path)
		if err != nil {
			t.Fatalf("InspectWAV %s: %v", path, err)
		}
		if path == long && info.Duration != 1500*time.Millisecond {
			t.Fatalf("long.wav duration = %v after rejection", info.Duration)
		}
	}
}

func TestAssignmentNoneDisablesPlayback(t *testing.T) {
	f := newFixture(t, sound.Config{})
	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleError), sound.None); err != nil {
		t.Fatalf("Set none: %v", err)
	}
	if clip, ok := f.sounds.Clip(sound.RoleError); ok || len(clip.PCM) != 0 {
		t.Fatalf("none Clip = %+v, %v", clip, ok)
	}
	if err := f.sounds.Play(context.Background(), sound.RoleError); err != nil {
		t.Fatalf("Play none: %v", err)
	}
	if writes, _ := f.sink.snapshot(); len(writes) != 0 {
		t.Fatalf("none played %d writes", len(writes))
	}
	if got := f.settings.String(sound.Key(sound.RoleError)); got != sound.None {
		t.Fatalf("effective assignment = %q", got)
	}
}

func TestReminderAndRingCueSeamsResolveAndPlay(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 300*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	ctx := context.Background()
	for _, role := range []sound.Role{sound.RoleReminder, sound.RoleTimerRing, sound.RoleAlarmRing} {
		if _, err := f.settings.Set(ctx, sound.Key(role), "birds"); err != nil {
			t.Fatalf("Set %s: %v", role, err)
		}
		clip, ok := f.sounds.Clip(role)
		if !ok || clip.Duration != 300*time.Millisecond {
			t.Fatalf("%s clip = %+v, %v", role, clip.Duration, ok)
		}
	}
	// #54 and #52 consume these roles later; the playback seam is usable now.
	if err := f.sounds.Play(ctx, sound.RoleReminder); err != nil {
		t.Fatalf("Play reminder: %v", err)
	}
	if err := f.sounds.Play(ctx, sound.RoleTimerRing); err != nil {
		t.Fatalf("Play timer ring: %v", err)
	}
	if writes, _ := f.sink.snapshot(); len(writes) != 2 {
		t.Fatalf("reminder/ring writes = %d, want 2", len(writes))
	}
	if _, err := f.settings.Set(ctx, sound.Key(sound.RoleAlarmRing), sound.None); err != nil {
		t.Fatalf("Set alarm none: %v", err)
	}
	if err := f.sounds.Play(ctx, sound.RoleAlarmRing); err != nil {
		t.Fatalf("Play none alarm: %v", err)
	}
	if writes, _ := f.sink.snapshot(); len(writes) != 2 {
		t.Fatalf("none alarm added a write: %d", len(writes))
	}
}

func TestLiveAssignmentChangesTheNextClip(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 400*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	before, ok := f.sounds.Clip(sound.RoleWake)
	if !ok || !bytes.Equal(before.PCM, audio.Chime(cueFormat.Rate)) {
		t.Fatal("wake did not start on the built-in chime")
	}
	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleWake), "birds"); err != nil {
		t.Fatalf("Set birds: %v", err)
	}
	after, ok := f.sounds.Clip(sound.RoleWake)
	if !ok || bytes.Equal(after.PCM, before.PCM) || after.Duration != 400*time.Millisecond {
		t.Fatalf("wake clip after override = %+v, %v", after.Duration, ok)
	}
	if _, err := f.settings.Reset(context.Background(), sound.Key(sound.RoleWake)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	reset, ok := f.sounds.Clip(sound.RoleWake)
	if !ok || !bytes.Equal(reset.PCM, before.PCM) {
		t.Fatal("wake did not return to the built-in chime after reset")
	}
}

func TestDefaultClipResolvesConfigBeforeBind(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 400*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	// An empty config value uses the built-in chime, exactly like the setting.
	chime, ok := f.sounds.DefaultClip(sound.Config{}, sound.RoleTimerRing)
	if !ok || !bytes.Equal(chime.PCM, audio.Chime(cueFormat.Rate)) {
		t.Fatal("default Timer Ring did not resolve the built-in chime")
	}
	birds, ok := f.sounds.DefaultClip(sound.Config{AlarmRing: "birds"}, sound.RoleAlarmRing)
	if !ok || birds.Duration != 400*time.Millisecond {
		t.Fatalf("config Alarm Ring = %+v, %v", birds.Duration, ok)
	}
	if _, ok := f.sounds.DefaultClip(sound.Config{TimerRing: sound.None}, sound.RoleTimerRing); ok {
		t.Fatal("an explicit none resolved a clip")
	}
	if _, ok := f.sounds.DefaultClip(sound.Config{TimerRing: "missing"}, sound.RoleTimerRing); ok {
		t.Fatal("an unknown config sound resolved a clip")
	}
}

func TestConfigLayerAcceptsBuiltinAndVolumePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sounds"), 0o700); err != nil {
		t.Fatalf("create sounds: %v", err)
	}
	path := writeLibraryWAV(t, filepath.Join(dir, "sounds"), "birds.wav", 400*time.Millisecond)
	f := newFixtureAt(t, dir, sound.Config{Wake: sound.BuiltinChime, Error: path})
	if got := f.settings.String(sound.Key(sound.RoleWake)); got != sound.BuiltinChime {
		t.Fatalf("config wake = %q", got)
	}
	value, ok := f.settings.Get(sound.Key(sound.RoleError))
	if !ok || value.Source != settings.SourceConfig {
		t.Fatalf("error source = %+v", value)
	}
	clip, ok := f.sounds.Clip(sound.RoleError)
	if !ok || clip.Duration != 400*time.Millisecond {
		t.Fatalf("config path clip = %+v, %v", clip.Duration, ok)
	}
	if err := f.sounds.Play(context.Background(), sound.RoleError); err != nil {
		t.Fatalf("Play configured path: %v", err)
	}
	if writes, formats := f.sink.snapshot(); len(writes) == 0 || len(formats) == 0 || formats[0] != cueFormat {
		t.Fatalf("configured path playback = %d writes, formats %v", len(writes), formats)
	}
}

func TestAssignmentRejectsPathsOutsideTheVolumeAndMissingFiles(t *testing.T) {
	f := newFixture(t, sound.Config{})
	outside := filepath.Join(t.TempDir(), "outside.wav")
	writeFile(t, outside, wavBytes(200*time.Millisecond))

	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleWake), outside); err == nil ||
		!strings.Contains(err.Error(), "outside the data volume") {
		t.Fatalf("outside assignment error = %v", err)
	}
	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleWake), "missing.wav"); err == nil ||
		!strings.Contains(err.Error(), "not a readable file") {
		t.Fatalf("missing assignment error = %v", err)
	}
	invalid := filepath.Join(f.dir, "broken.wav")
	writeFile(t, invalid, []byte("not a riff"))
	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleError), invalid); err == nil {
		t.Fatal("invalid WAV path was accepted")
	}
}

func TestSymlinkEscapeIsRejected(t *testing.T) {
	f := newFixture(t, sound.Config{})
	outside := filepath.Join(t.TempDir(), "outside.wav")
	writeFile(t, outside, wavBytes(200*time.Millisecond))
	link := filepath.Join(f.sounds.LibraryDir(), "linked.wav")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	for _, entry := range f.sounds.List() {
		if entry.Name == "linked" {
			t.Fatalf("escaping symlink was indexed: %+v", entry)
		}
	}
	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleWake), link); err == nil ||
		!strings.Contains(err.Error(), "outside the data volume") {
		t.Fatalf("symlink assignment error = %v", err)
	}
}

func TestRoleAssignmentsAndIndexSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "assistant.db")
	ctx := context.Background()

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	firstSounds, err := sound.New(ctx, first, dir, cueFormat, discardLogger())
	if err != nil {
		t.Fatalf("first sound.New: %v", err)
	}
	writeLibraryWAV(t, firstSounds.LibraryDir(), "birds.wav", 600*time.Millisecond)
	if err := firstSounds.Rescan(ctx); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	firstSettings, err := settings.New(ctx, first, firstSounds.Definitions(sound.Config{})...)
	if err != nil {
		t.Fatalf("first settings.New: %v", err)
	}
	firstSounds.Bind(firstSettings, nil)
	if _, err := firstSettings.Set(ctx, sound.Key(sound.RoleReminder), "birds"); err != nil {
		t.Fatalf("Set birds: %v", err)
	}
	before := findSound(t, firstSounds.List(), "birds")
	if err := first.Close(); err != nil {
		t.Fatalf("first store close: %v", err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	defer second.Close()
	secondSounds, err := sound.New(ctx, second, dir, cueFormat, discardLogger())
	if err != nil {
		t.Fatalf("second sound.New: %v", err)
	}
	secondSettings, err := settings.New(ctx, second, secondSounds.Definitions(sound.Config{})...)
	if err != nil {
		t.Fatalf("second settings.New: %v", err)
	}
	secondSounds.Bind(secondSettings, nil)
	if got := secondSettings.String(sound.Key(sound.RoleReminder)); got != "birds" {
		t.Fatalf("reminder assignment after reopen = %q", got)
	}
	after := findSound(t, secondSounds.List(), "birds")
	if !after.AddedAt.Equal(before.AddedAt) || after.Duration != before.Duration {
		t.Fatalf("reopened index = %+v, want %+v", after, before)
	}
	if clip, ok := secondSounds.Clip(sound.RoleReminder); !ok || clip.Duration != 600*time.Millisecond {
		t.Fatalf("reopened clip = %+v, %v", clip.Duration, ok)
	}

	// Role assignments are settings rows in the same database, not a second
	// store or a YAML write.
	var key string
	if err := second.DB().QueryRow(`SELECT key FROM settings WHERE key = ?`, sound.Key(sound.RoleReminder)).Scan(&key); err != nil {
		t.Fatalf("role assignment row: %v", err)
	}
	if key != "sounds.reminder" {
		t.Fatalf("role key = %q", key)
	}
}

func TestPlaybackThroughPacatWritesDecodedPCM(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "pcm.raw")
	binary := filepath.Join(dir, "pacat")
	script := "#!/bin/sh\ncat > '" + pcmPath + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}

	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 100*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, err := f.settings.Set(context.Background(), sound.Key(sound.RoleError), "birds"); err != nil {
		t.Fatalf("Set birds: %v", err)
	}
	f.sounds.Bind(f.settings, audio.NewPulseSink(binary, ""))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.sounds.Play(ctx, sound.RoleError); err != nil {
		t.Fatalf("Play: %v", err)
	}
	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		t.Fatalf("read pcm: %v", err)
	}
	if len(pcm) != cueFormat.Rate/10*2 {
		t.Fatalf("played %d bytes, want %d", len(pcm), cueFormat.Rate/10*2)
	}
}

func TestConcurrentAssignmentAndPlaybackAreRaceFree(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 100*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		values := []string{sound.None, sound.BuiltinChime, "birds"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.settings.Set(ctx, sound.Key(sound.RoleError), values[i%len(values)]); err != nil {
				errs <- err
				return
			}
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				f.sounds.Clip(sound.RoleError)
				_ = f.sounds.Play(ctx, sound.RoleError)
			}
		}()
	}
	// Let the writers and readers overlap, then stop the writer.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent assignment: %v", err)
	}
}

func findSound(t *testing.T, list []sound.Sound, name string) sound.Sound {
	t.Helper()
	for _, entry := range list {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("sound %q not in %v", name, list)
	return sound.Sound{}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSettingsAPIRejectsAnOverCapCue is the Dashboard/API seam: the same live
// service the page writes rejects an oversized assignment with the cap in the
// response, and accepts a valid one that the next Clip resolves.
func TestSettingsAPIRejectsAnOverCapCue(t *testing.T) {
	f := newFixture(t, sound.Config{})
	writeLibraryWAV(t, f.sounds.LibraryDir(), "long.wav", 1500*time.Millisecond)
	writeLibraryWAV(t, f.sounds.LibraryDir(), "birds.wav", 400*time.Millisecond)
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	handler := settingsapi.New(f.settings, nil, discardLogger())
	server := httptest.NewServer(handler)
	defer server.Close()

	put := func(key, value string) (int, string) {
		t.Helper()
		body := strings.NewReader(`{"value":"` + value + `"}`)
		req, err := http.NewRequest(http.MethodPut, server.URL+"/settings/"+key, body)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", key, err)
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return response.StatusCode, string(payload)
	}

	status, body := put("sounds.wake", "long")
	if status != http.StatusBadRequest || !strings.Contains(body, "over the 1s cap") {
		t.Fatalf("oversized PUT = %d %s", status, body)
	}
	status, body = put("sounds.wake", "birds")
	if status != http.StatusOK || !strings.Contains(body, `"source":"override"`) {
		t.Fatalf("valid PUT = %d %s", status, body)
	}
	clip, ok := f.sounds.Clip(sound.RoleWake)
	if !ok || clip.Duration != 400*time.Millisecond {
		t.Fatalf("clip after PUT = %+v, %v", clip.Duration, ok)
	}
}
