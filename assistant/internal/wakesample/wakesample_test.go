package wakesample

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// toneWAV builds a canonical take with a short burst of "speech" in the middle
// and room noise elsewhere, so the energy gate has something to find. Nothing
// here is a recording: it is a deterministic synthetic signal.
func toneWAV(seconds float64, speech bool) []byte {
	frames := int(seconds * SampleRate)
	samples := make([]int16, frames)
	for i := range samples {
		// A quiet, non-zero room floor.
		samples[i] = int16(math.Sin(float64(i)*0.03) * 60)
	}
	if speech {
		start := frames / 3
		end := start + int(0.45*SampleRate)
		if end > frames {
			end = frames
		}
		for i := start; i < end; i++ {
			samples[i] = int16(math.Sin(float64(i)*0.42) * 9000)
		}
	}
	return EncodeWAV(samples)
}

func TestParseWAVAcceptsTheTrainingFormat(t *testing.T) {
	data := toneWAV(1.2, true)
	samples, err := ParseWAV(data)
	if err != nil {
		t.Fatalf("ParseWAV: %v", err)
	}
	if got := DurationSeconds(len(samples)); math.Abs(got-1.2) > 0.02 {
		t.Errorf("duration = %.3f, want 1.2", got)
	}
	if got, err := Info(data); err != nil || got != len(samples) {
		t.Errorf("Info = %d, %v; want %d", got, err, len(samples))
	}
}

func TestParseWAVRejectsEveryOtherFormat(t *testing.T) {
	canonical := toneWAV(1, true)
	tests := map[string][]byte{
		"empty":     {},
		"not RIFF":  []byte("RIFFxxxxWAVExxxx"),
		"truncated": canonical[:30],
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWAV(data); err == nil {
				t.Fatal("ParseWAV accepted a bad file")
			}
		})
	}

	// Stereo, 44.1 kHz and 8-bit are each a real client bug; all must fail.
	for name, mutate := range map[string]func([]byte){
		"stereo": func(b []byte) { b[22] = 2 },
		"rate":   func(b []byte) { b[24], b[25], b[26], b[27] = 0x44, 0xac, 0, 0 },
		"bits":   func(b []byte) { b[34] = 8 },
		"format": func(b []byte) { b[20] = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			data := append([]byte(nil), canonical...)
			mutate(data)
			if _, err := ParseWAV(data); err == nil {
				t.Fatal("ParseWAV accepted a malformed header")
			}
		})
	}
}

func TestPrepareTrimsToTheTrainingClip(t *testing.T) {
	audio, err := Prepare(toneWAV(2.0, true))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !audio.FoundSpeech {
		t.Error("FoundSpeech = false, want true for a synthetic burst")
	}
	if audio.Duration() > MaxClipSeconds+0.001 {
		t.Errorf("clip = %.3f s, want at most %.1f", audio.Duration(), MaxClipSeconds)
	}
	// The burst starts a third into a 2 s take; the pre-roll must keep some of
	// the quiet lead-in rather than starting exactly at the speech.
	if audio.Duration() < 0.6 || audio.Duration() > 1.0 {
		t.Errorf("trimmed clip = %.3f s, want the burst plus the margins", audio.Duration())
	}
}

func TestPrepareKeepsASilentTakeWholeAndReportsIt(t *testing.T) {
	audio, err := Prepare(toneWAV(1.5, false))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if audio.FoundSpeech {
		t.Error("FoundSpeech = true for silence")
	}
	if got := audio.Duration(); math.Abs(got-1.5) > 0.02 {
		t.Errorf("silent take = %.3f s, want it kept whole", got)
	}
}

func TestPrepareEnforcesTheTakeBounds(t *testing.T) {
	if _, err := Prepare(toneWAV(MinTakeSeconds/2, true)); err == nil {
		t.Error("accepted a click-length take")
	}
	if _, err := Prepare(toneWAV(MaxTakeSeconds+0.2, true)); err == nil {
		t.Error("accepted a take longer than the limit")
	}
}

func newStore(t *testing.T, opts Options) *Service {
	t.Helper()
	dir := t.TempDir()
	if opts.Dir == "" {
		opts.Dir = filepath.Join(dir, "samples")
	}
	if opts.ExportDir == "" {
		opts.ExportDir = filepath.Join(dir, "exports")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC) }
	}
	service, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

var takeSeq int

func keep(t *testing.T, service *Service, slug string, kind Kind, seconds float64) Take {
	t.Helper()
	// A different duration per call keeps the synthetic samples distinct, so
	// the duplicate guard does not collapse a test set.
	takeSeq++
	take, err := service.Add(slug, kind, toneWAV(seconds+float64(takeSeq)/1000, true))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return take
}

func TestSlugForAndValidSlugRejectPathShapes(t *testing.T) {
	for input, want := range map[string]string{
		"Volume Up":    "volume-up",
		"clanker":      "clanker",
		"volume down!": "volume-down",
		"  hey clanka": "hey-clanka",
		"volume_up":    "volume-up",
	} {
		got, ok := SlugFor(input)
		if !ok || got != want {
			t.Errorf("SlugFor(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "../clanker", "a/b", "..", "a.b", `a\b`, "turn on the kitchen light", strings.Repeat("a", 41)} {
		if _, ok := SlugFor(input); ok {
			t.Errorf("SlugFor(%q) accepted an unsafe phrase", input)
		}
	}
	// Punctuation and case are normalised, not carried into a path.
	for input, want := range map[string]string{"-leading": "leading", "trailing-": "trailing", "two--dashes": "two-dashes", "clanker!": "clanker", "CLANKER": "clanker"} {
		if got, ok := SlugFor(input); !ok || got != want {
			t.Errorf("SlugFor(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
	for _, slug := range []string{"../clanker", "a/b", "", "Clanker", "-clanker", "clanker-", "12345", "two--dashes"} {
		if ValidSlug(slug) {
			t.Errorf("ValidSlug(%q) = true", slug)
		}
	}
}

func TestAddStoresCanonicalTakesAndRefusesDuplicates(t *testing.T) {
	service := newStore(t, Options{})
	// A repeated tap re-uploads the same bytes, so that is what the guard must
	// refuse. Re-adding the stored file instead looks identical only while the
	// silence trim is idempotent, and it is not: trim re-frames the clip, so a
	// one-frame boundary shift on a different length makes the digests differ.
	input := toneWAV(1.4, true)
	first, err := service.Add("volume-up", Positive, input)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if first.ID != "real_p_00001" {
		t.Fatalf("first id = %q", first.ID)
	}
	path := filepath.Join(service.Dir(), "volume-up", "real", "positive", "real_p_00001.wav")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stored take: %v", err)
	}
	if _, err := ParseWAV(data); err != nil {
		t.Fatalf("stored take is not canonical: %v", err)
	}
	if _, err := service.Add("volume-up", Positive, input); err == nil {
		t.Fatal("accepted an identical take")
	}
	second := keep(t, service, "volume-up", Positive, 1.2)
	if second.ID != "real_p_00002" {
		t.Fatalf("second id = %q", second.ID)
	}
}

func TestAddEnforcesThePerKindLimit(t *testing.T) {
	service := newStore(t, Options{})
	for i := 0; i < MaxTakes; i++ {
		// A distinct tone per take, so dedupe does not collapse them.
		audio := make([]int16, SampleRate)
		for j := range audio {
			audio[j] = int16(math.Sin(float64(j)*(0.01+float64(i)*0.0001)) * 5000)
		}
		if _, err := service.Add("clanker", Positive, EncodeWAV(audio)); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	if _, err := service.Add("clanker", Positive, toneWAV(1, true)); err == nil {
		t.Fatal("accepted take 101")
	}
}

func TestDeleteAndClearKeepNumberingForward(t *testing.T) {
	service := newStore(t, Options{})
	one := keep(t, service, "clanker", Positive, 1)
	two := keep(t, service, "clanker", Positive, 1.1)
	if err := service.Delete("clanker", Positive, one.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	three := keep(t, service, "clanker", Positive, 1.2)
	if three.ID != "real_p_00003" {
		t.Fatalf("after delete id = %q, want no reuse", three.ID)
	}
	if err := service.Delete("clanker", Positive, two.ID); err != nil {
		t.Fatalf("Delete second: %v", err)
	}
	if err := service.Clear("clanker", Positive); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := service.Count("clanker", Positive); got != 0 {
		t.Fatalf("count after clear = %d", got)
	}
}

func TestSubmitRefusesAnIncompleteSet(t *testing.T) {
	service := newStore(t, Options{})
	for i := 0; i < MinTakes-1; i++ {
		keep(t, service, "clanker", Positive, 1)
	}
	if _, err := service.Submit("clanker"); err == nil {
		t.Fatal("submitted fewer than the minimum")
	}
}

func TestSubmitWritesTheRecorderLayoutAndACopyCommand(t *testing.T) {
	service := newStore(t, Options{
		HostDir: "/home/rbpi/clanka-bot/exports",
		SSHUser: "rbpi",
		SSHHost: "pi",
		Desktop: "desktop",
	})
	for i := 0; i < MinTakes; i++ {
		keep(t, service, "volume-up", Positive, 1.1+float64(i)/100)
	}
	keep(t, service, "volume-up", Negative, 1.2)
	bundle, err := service.Submit("volume-up")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if bundle.Name != "volume-up-20261112-093000.tar.gz" {
		t.Errorf("bundle name = %q", bundle.Name)
	}
	if bundle.Positives != MinTakes || bundle.Negatives != 1 {
		t.Errorf("counts = %d/%d", bundle.Positives, bundle.Negatives)
	}
	if bundle.HostPath != "/home/rbpi/clanka-bot/exports/"+bundle.Name {
		t.Errorf("host path = %q", bundle.HostPath)
	}
	if want := "scp rbpi@pi:" + bundle.HostPath + " ."; bundle.SCP != want {
		t.Errorf("SCP = %q, want %q", bundle.SCP, want)
	}
	if want := "tailscale file cp " + bundle.HostPath + " desktop:"; bundle.Taildrop != want {
		t.Errorf("Taildrop = %q, want %q", bundle.Taildrop, want)
	}

	file, _, err := service.OpenBundle("volume-up", bundle.Name)
	if err != nil {
		t.Fatalf("OpenBundle: %v", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var names []string
	var meta manifest
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, header.Name)
		if header.Name == "manifest.json" {
			body, _ := io.ReadAll(reader)
			if err := json.Unmarshal(body, &meta); err != nil {
				t.Fatalf("manifest: %v", err)
			}
		}
	}
	if meta.Phrase != "volume up" || meta.Positives != MinTakes || meta.SampleRate != SampleRate || meta.Channels != 1 || meta.SampleWidth != 2 {
		t.Errorf("manifest = %+v", meta)
	}
	want := []string{"manifest.json"}
	for i := 1; i <= MinTakes; i++ {
		want = append(want, fmt.Sprintf("real/positive/real_p_%05d.wav", i))
	}
	want = append(want, "real/negative/real_n_00001.wav")
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tar entries = %v, want %v", names, want)
	}

	bundles, err := service.Bundles("volume-up", 5)
	if err != nil || len(bundles) != 1 {
		t.Fatalf("Bundles = %v, %v", bundles, err)
	}
	if bundles[0].SCP == "" || bundles[0].Positives != MinTakes {
		t.Errorf("listed bundle = %+v", bundles[0])
	}
}

func TestCopyCommandsStayHiddenWithoutConfiguration(t *testing.T) {
	service := newStore(t, Options{})
	for i := 0; i < MinTakes; i++ {
		keep(t, service, "clanker", Positive, 1)
	}
	bundle, err := service.Submit("clanker")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if bundle.SCP != "" || bundle.Taildrop != "" || bundle.HostPath != "" {
		t.Errorf("bundle leaked commands without config: %+v", bundle)
	}
}

func TestOpenBundleRejectsTraversalAndOtherPhrases(t *testing.T) {
	service := newStore(t, Options{})
	for _, name := range []string{"../assistant.db", "clanker-20261112-093000.tar.gz", "volume-up-20261112-093000.tar.gz/../x", "clanker.tar.gz"} {
		if _, _, err := service.OpenBundle("volume-up", name); err == nil {
			t.Errorf("OpenBundle accepted %q", name)
		}
	}
	// A shorter slug that is a prefix of another phrase must not borrow its
	// bundle; the date/time suffix is the boundary.
	for i := 0; i < MinTakes; i++ {
		keep(t, service, "volume-up", Positive, 1)
	}
	bundle, err := service.Submit("volume-up")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, _, err := service.OpenBundle("volume", bundle.Name); err == nil {
		t.Error("OpenBundle served another phrase's bundle")
	}
	if bundles, err := service.Bundles("volume", 5); err != nil || len(bundles) != 0 {
		t.Errorf("Bundles(volume) = %v, %v", bundles, err)
	}
}

func TestPhrasesListsBuiltinsAndStoredOnes(t *testing.T) {
	service := newStore(t, Options{})
	keep(t, service, "hey-clanka", Positive, 1)
	got := strings.Join(service.Phrases(), ",")
	if !strings.Contains(got, "clanker") || !strings.Contains(got, "volume-up") || !strings.Contains(got, "volume-down") || !strings.Contains(got, "hey-clanka") {
		t.Fatalf("Phrases = %q", got)
	}
}

func TestBundleNameAndURLs(t *testing.T) {
	if !ValidBundleName("volume-up-20261112-093000.tar.gz") {
		t.Error("ValidBundleName rejected a real name")
	}
	if ValidBundleName("../../etc/passwd") || ValidBundleName("../x-20261112-093000.tar.gz") {
		t.Error("ValidBundleName accepted a bad name")
	}
	if got := DownloadURL("volume up", "x.tar.gz"); strings.Contains(got, " ") {
		t.Errorf("DownloadURL = %q", got)
	}
}
