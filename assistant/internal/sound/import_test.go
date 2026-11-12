package sound_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

// stereoWAV synthesises an interleaved PCM16 WAV so upload tests exercise real
// downmix and resampling rather than pre-canonical input.
func stereoWAV(t *testing.T, rate, channels int, duration time.Duration, amplitude float64) []byte {
	t.Helper()
	frames := int(duration * time.Duration(rate) / time.Second)
	data := make([]byte, frames*channels*2)
	for frame := 0; frame < frames; frame++ {
		sample := int16(math.Round(amplitude * math.MaxInt16 * math.Sin(2*math.Pi*440*float64(frame)/float64(rate))))
		for channel := 0; channel < channels; channel++ {
			binary.LittleEndian.PutUint16(data[(frame*channels+channel)*2:], uint16(sample))
		}
	}
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(data)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(header[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(data)))
	return append(header, data...)
}

// libraryWAVs lists real .wav files, ignoring hidden spool files.
func libraryWAVs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".wav") && !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestImportCanonicalizesWAVAndIndexesDuration(t *testing.T) {
	f := newFixture(t, sound.Config{})
	source := stereoWAV(t, 8000, 2, 500*time.Millisecond, 0.3)
	entry, err := f.sounds.Import(context.Background(), bytes.NewReader(source), "My Birds!.wav")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if entry.Name != "My-Birds" {
		t.Fatalf("name = %q, want My-Birds", entry.Name)
	}
	if entry.Duration < 490*time.Millisecond || entry.Duration > 510*time.Millisecond {
		t.Fatalf("duration = %s, want about 500ms", entry.Duration)
	}
	info, err := audio.InspectWAV(entry.Path)
	if err != nil {
		t.Fatalf("InspectWAV: %v", err)
	}
	if info.Format != (audio.Format{Rate: 16000, Width: 2, Channels: 1}) {
		t.Fatalf("canonical format = %+v, want mono 16 kHz", info.Format)
	}
	if got := libraryWAVs(t, f.sounds.LibraryDir()); len(got) != 1 || got[0] != "My-Birds.wav" {
		t.Fatalf("library files = %v, want My-Birds.wav only", got)
	}
	entries, err := os.ReadDir(f.sounds.LibraryDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("temp file %q left behind", entry.Name())
		}
	}
	found, ok := f.sounds.Find("my-birds")
	if !ok || found.Duration != entry.Duration {
		t.Fatalf("Find = %+v, ok=%v", found, ok)
	}
}

func TestImportMP3(t *testing.T) {
	f := newFixture(t, sound.Config{})
	file, err := os.Open(filepath.Join("..", "audio", "testdata", "tone.mp3"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	entry, err := f.sounds.Import(context.Background(), file, "tone.mp3")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if entry.Name != "tone" {
		t.Fatalf("name = %q", entry.Name)
	}
	if entry.Duration < 550*time.Millisecond || entry.Duration > 660*time.Millisecond {
		t.Fatalf("duration = %s, want about 600ms", entry.Duration)
	}
	info, err := audio.InspectWAV(entry.Path)
	if err != nil {
		t.Fatalf("InspectWAV: %v", err)
	}
	if info.Format != (audio.Format{Rate: 16000, Width: 2, Channels: 1}) {
		t.Fatalf("canonical format = %+v", info.Format)
	}
}

func TestImportRejectsOversizeWithoutPublishing(t *testing.T) {
	f := newFixture(t, sound.Config{})
	oversize := io.LimitReader(zeroReader{}, audio.MaxUploadBytes+1)
	if _, err := f.sounds.Import(context.Background(), oversize, "huge.wav"); !errors.Is(err, sound.ErrUploadTooLarge) {
		t.Fatalf("Import error = %v, want ErrUploadTooLarge", err)
	}
	if got := libraryWAVs(t, f.sounds.LibraryDir()); len(got) != 0 {
		t.Fatalf("library files after rejected upload = %v", got)
	}
}

func TestImportRejectsUndecodableAndLeavesNoPartialFile(t *testing.T) {
	f := newFixture(t, sound.Config{})
	if _, err := f.sounds.Import(context.Background(), strings.NewReader("this is not audio"), "notes.wav"); err == nil {
		t.Fatal("Import accepted text")
	}
	if got := libraryWAVs(t, f.sounds.LibraryDir()); len(got) != 0 {
		t.Fatalf("library files after failed import = %v", got)
	}
	entries, err := os.ReadDir(f.sounds.LibraryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("temp file %q left behind", entry.Name())
		}
	}
}

func TestImportUsesUniqueNamesAndAvoidsReservedOnes(t *testing.T) {
	f := newFixture(t, sound.Config{})
	source := stereoWAV(t, 16000, 1, 100*time.Millisecond, 0.2)
	for _, filename := range []string{"chime.wav", "none.wav", "birds.wav", "birds.wav"} {
		if _, err := f.sounds.Import(context.Background(), bytes.NewReader(source), filename); err != nil {
			t.Fatalf("Import %s: %v", filename, err)
		}
	}
	got := libraryWAVs(t, f.sounds.LibraryDir())
	want := []string{"birds-2.wav", "birds.wav", "chime-2.wav", "none-2.wav"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("library files = %v, want %v", got, want)
	}
}

func TestImportRemovesThePublishedFileWhenIndexingFails(t *testing.T) {
	f := newFixture(t, sound.Config{})
	if err := f.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	source := stereoWAV(t, 16000, 1, 100*time.Millisecond, 0.2)
	if _, err := f.sounds.Import(context.Background(), bytes.NewReader(source), "birds.wav"); err == nil {
		t.Fatal("Import succeeded with a closed store")
	}
	if got := libraryWAVs(t, f.sounds.LibraryDir()); len(got) != 0 {
		t.Fatalf("published file survived a failed index: %v", got)
	}
}

func TestFindAudioAndPlayNameUseLibraryNamesOnly(t *testing.T) {
	f := newFixture(t, sound.Config{})
	source := stereoWAV(t, 16000, 1, 100*time.Millisecond, 0.2)
	if _, err := f.sounds.Import(context.Background(), bytes.NewReader(source), "birds.wav"); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, ok := f.sounds.Find("../assistant.db"); ok {
		t.Fatal("Find accepted a path")
	}
	if _, ok := f.sounds.Find("sounds/birds.wav"); ok {
		t.Fatal("Find accepted a volume path")
	}
	audioBytes, entry, err := f.sounds.Audio("BIRDS")
	if err != nil {
		t.Fatalf("Audio: %v", err)
	}
	if len(audioBytes) < 44 || string(audioBytes[0:4]) != "RIFF" {
		t.Fatalf("Audio returned %d bytes without a RIFF header", len(audioBytes))
	}
	if entry.Name != "birds" {
		t.Fatalf("entry = %+v", entry)
	}
	if path, modTime, ok := f.sounds.File("birds"); !ok || path != entry.Path || modTime.IsZero() {
		t.Fatalf("File = %q, %v, %v", path, modTime, ok)
	}
	if _, _, ok := f.sounds.File("chime"); ok {
		t.Fatal("File returned a disk path for the built-in chime")
	}
	if err := f.sounds.PlayName(context.Background(), "birds"); err != nil {
		t.Fatalf("PlayName: %v", err)
	}
	writes, formats := f.sink.snapshot()
	if len(writes) == 0 || len(writes[0]) == 0 {
		t.Fatalf("sink writes = %d", len(writes))
	}
	if formats[0] != cueFormat {
		t.Fatalf("played format = %+v, want %+v", formats[0], cueFormat)
	}
	if err := f.sounds.PlayName(context.Background(), "missing"); err == nil {
		t.Fatal("PlayName accepted an unknown name")
	}
}

func TestImportedSoundIsNotCappedUntilAssignment(t *testing.T) {
	f := newFixture(t, sound.Config{})
	source := stereoWAV(t, 16000, 1, 1500*time.Millisecond, 0.2)
	entry, err := f.sounds.Import(context.Background(), bytes.NewReader(source), "long.wav")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if entry.Duration < 1490*time.Millisecond {
		t.Fatalf("duration = %s, want the full upload", entry.Duration)
	}
	if err := f.sounds.Validate(sound.RoleTimerRing, "long"); err != nil {
		t.Fatalf("Timer Ring rejected a 1.5 s sound: %v", err)
	}
	err = f.sounds.Validate(sound.RoleWake, "long")
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("wake assignment error = %v, want a cap error", err)
	}
	// Rejection must not trim or rewrite the published file.
	after, ok := f.sounds.Find("long")
	if !ok || after.Duration != entry.Duration {
		t.Fatalf("sound changed after rejected assignment: %+v", after)
	}
}

// zeroReader yields zeros without allocating a 10 MB slice in the test.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = 0
	}
	return len(p), nil
}
