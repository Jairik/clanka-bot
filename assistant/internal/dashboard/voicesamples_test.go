package dashboard_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/wakesample"
)

// voiceSamplesFixture is a real store on a temp directory and a handler that
// serves it. Nothing is shared with another test.
type voiceSamplesFixture struct {
	t       *testing.T
	dir     string
	handler *dashboard.Handler
	store   *wakesample.Service
}

func newVoiceSamplesFixture(t *testing.T, opts wakesample.Options) *voiceSamplesFixture {
	t.Helper()
	dir := t.TempDir()
	if opts.Dir == "" {
		opts.Dir = filepath.Join(dir, "samples")
	}
	if opts.ExportDir == "" {
		opts.ExportDir = filepath.Join(dir, "exports")
	}
	store, err := wakesample.New(opts)
	if err != nil {
		t.Fatalf("wakesample.New: %v", err)
	}
	return &voiceSamplesFixture{
		t:       t,
		dir:     dir,
		store:   store,
		handler: newHandler(t, dashboard.Options{VoiceSamples: store}),
	}
}

// take builds a deterministic synthetic take. The frequency varies so a set
// is not collapsed by the duplicate guard.
func take(seconds float64, seed int) []byte {
	samples := make([]int16, int(seconds*wakesample.SampleRate))
	for i := range samples {
		samples[i] = int16(math.Sin(float64(i)*(0.04+float64(seed)*0.001)) * 7000)
	}
	return wakesample.EncodeWAV(samples)
}

func (f *voiceSamplesFixture) post(target string, body []byte, contentType string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *voiceSamplesFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := serve(f.handler, request(http.MethodGet, target, owner))
	return rec
}

type voiceSamplesView struct {
	Owner, Page, Flash, Error string
	HasStore                  bool
	RecordingAvailable        bool
	Phrases                   []struct {
		Slug, Text string
		Selected   bool
	}
	Slug, Phrase                 string
	PositiveCount, NegativeCount int
	MinTakes, MaxTakes           int
	MaxSeconds, MaxBytes         string
	Positives, Negatives         []struct {
		ID, Duration, Size, Added, AudioURL, DeleteAction string
	}
	Prompts                                    []string
	NextPrompt, NextNegativePrompt             string
	UploadPositive, UploadNegative             string
	SubmitAction, ClearPositive, ClearNegative string
	CanSubmit                                  bool
	Bundles                                    []struct {
		Name, Size, Created, DownloadURL, HostPath, SCP, Taildrop string
		Positives, Negatives                                      int
	}
}

func TestVoiceSamplesPageRendersTheRecorder(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	var view voiceSamplesView
	rec := getJSON(t, f.handler, "/api/ui/voice-samples", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if view.Owner != owner || view.Page != "voice-samples" {
		t.Errorf("owner/page = %q/%q", view.Owner, view.Page)
	}
	if !view.HasStore || view.Slug != "clanker" || view.Phrase != "clanker" {
		t.Errorf("store/phrase = %v/%q/%q", view.HasStore, view.Slug, view.Phrase)
	}
	if view.MinTakes != wakesample.MinTakes || view.MaxTakes != wakesample.MaxTakes {
		t.Errorf("limits = %d..%d", view.MinTakes, view.MaxTakes)
	}
	if view.MaxSeconds != "3 s" || view.MaxBytes != "512 KB" {
		t.Errorf("caps = %q/%q", view.MaxSeconds, view.MaxBytes)
	}
	if view.CanSubmit {
		t.Error("CanSubmit = true with no takes")
	}
	if view.UploadPositive != "/voice-samples/takes/clanker/positive" || view.SubmitAction != "/voice-samples/submit/clanker" {
		t.Errorf("actions = %q / %q", view.UploadPositive, view.SubmitAction)
	}
	if len(view.Phrases) != 3 {
		t.Fatalf("phrases = %+v", view.Phrases)
	}
	if refs := fmt.Sprint(view.Phrases); !strings.Contains(refs, "volume-up") || !strings.Contains(refs, "volume-down") {
		t.Errorf("phrases lack the command phrases: %s", refs)
	}
	if view.NextPrompt == "" || !strings.Contains(view.NextPrompt, "normal voice") {
		t.Errorf("next prompt = %q", view.NextPrompt)
	}
	if rec := f.get("/api/ui/voice-samples?phrase=volume-up"); rec.Code != http.StatusOK {
		t.Errorf("volume-up page = %d", rec.Code)
	}
	var volume voiceSamplesView
	decodeJSON(t, f.get("/api/ui/voice-samples?phrase=volume-up"), &volume)
	if volume.Slug != "volume-up" || volume.Phrase != "volume up" {
		t.Errorf("volume phrase = %q/%q", volume.Slug, volume.Phrase)
	}
}

func TestVoiceSamplesPhraseIsSanitised(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	var view voiceSamplesView
	rec := getJSON(t, f.handler, "/api/ui/voice-samples?phrase=../assistant.db", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if view.Slug != "clanker" || view.Error == "" {
		t.Errorf("bad phrase fell back to %q with error %q", view.Slug, view.Error)
	}
}

func TestVoiceSamplesUploadPlaybackDeleteAndClear(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	var view voiceSamplesView
	decodeJSON(t, f.post("/voice-samples/takes/clanker/positive", take(1.2, 1), "audio/wav"), &view)
	if view.PositiveCount != 1 || len(view.Positives) != 1 {
		t.Fatalf("after upload = %d takes (%+v)", view.PositiveCount, view.Positives)
	}
	if !strings.HasPrefix(view.Positives[0].AudioURL, "/voice-samples/take/clanker/positive/real_p_") {
		t.Errorf("audio URL = %q", view.Positives[0].AudioURL)
	}
	if view.Flash == "" {
		t.Error("no flash after a kept take")
	}

	audio := f.get(view.Positives[0].AudioURL)
	if audio.Code != http.StatusOK {
		t.Fatalf("audio status = %d", audio.Code)
	}
	if got := audio.Header().Get("Content-Type"); got != "audio/wav" {
		t.Errorf("audio Content-Type = %q", got)
	}
	if _, err := wakesample.ParseWAV(audio.Body.Bytes()); err != nil {
		t.Errorf("served audio is not canonical: %v", err)
	}

	// A stereo clip is refused; the count stays at one.
	stereo := take(1.2, 2)
	stereo[22] = 2
	req := httptest.NewRequest(http.MethodPost, "/voice-samples/takes/clanker/positive", bytes.NewReader(stereo))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(f.handler, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stereo status = %d, want 400", rec.Code)
	}
	decodeJSON(t, rec, &view)
	if view.PositiveCount != 1 || view.Error == "" {
		t.Errorf("stereo upload changed the set: %d, %q", view.PositiveCount, view.Error)
	}

	decodeJSON(t, f.post(view.Positives[0].DeleteAction, nil, ""), &view)
	if view.PositiveCount != 0 {
		t.Errorf("after delete = %d", view.PositiveCount)
	}
	decodeJSON(t, f.post("/voice-samples/takes/clanker/positive", take(1.1, 3), "audio/wav"), &view)
	decodeJSON(t, f.post("/voice-samples/clear/clanker/positive", nil, ""), &view)
	if view.PositiveCount != 0 {
		t.Errorf("after clear = %d", view.PositiveCount)
	}
}

func TestVoiceSamplesUploadEnforcesTheLimitAndSize(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{MaxTakes: 6})
	for i := 0; i < 6; i++ {
		rec := f.post("/voice-samples/takes/clanker/positive", take(1.0+float64(i)/100, i), "audio/wav")
		if rec.Code != http.StatusOK {
			t.Fatalf("take %d = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := f.post("/voice-samples/takes/clanker/positive", take(1.5, 99), "audio/wav")
	if rec.Code != http.StatusConflict {
		t.Fatalf("over-limit status = %d, want 409", rec.Code)
	}
	var view voiceSamplesView
	decodeJSON(t, rec, &view)
	if view.PositiveCount != 6 || view.MaxTakes != 6 || !strings.Contains(view.Error, "already has 6") {
		t.Errorf("over-limit view = %d/%d, %q", view.PositiveCount, view.MaxTakes, view.Error)
	}
	big := make([]byte, wakesample.MaxTakeBytes+10)
	rec = f.post("/voice-samples/takes/clanker/positive", big, "audio/wav")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", rec.Code)
	}
}

func TestVoiceSamplesSubmitWritesABundleAndServesTheDownload(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{
		HostDir: "/home/rbpi/clanka-bot/exports",
		SSHUser: "rbpi",
		SSHHost: "pi",
		Desktop: "desktop",
	})
	var view voiceSamplesView
	for i := 0; i < wakesample.MinTakes; i++ {
		decodeJSON(t, f.post("/voice-samples/takes/volume-up/positive", take(1.0+float64(i)/100, i), "audio/wav"), &view)
	}
	decodeJSON(t, f.post("/voice-samples/takes/volume-up/negative", take(1.3, 50), "audio/wav"), &view)
	if !view.CanSubmit {
		t.Fatalf("CanSubmit = false at %d takes", view.PositiveCount)
	}
	rec := f.post("/voice-samples/submit/volume-up", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d: %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &view)
	if len(view.Bundles) != 1 {
		t.Fatalf("bundles = %+v", view.Bundles)
	}
	bundle := view.Bundles[0]
	if !strings.HasPrefix(bundle.Name, "volume-up-") || !strings.HasSuffix(bundle.Name, ".tar.gz") {
		t.Errorf("bundle name = %q", bundle.Name)
	}
	if bundle.Positives != wakesample.MinTakes || bundle.Negatives != 1 {
		t.Errorf("bundle counts = %d/%d", bundle.Positives, bundle.Negatives)
	}
	if !strings.HasPrefix(bundle.SCP, "scp rbpi@pi:/home/rbpi/clanka-bot/exports/") || !strings.Contains(bundle.Taildrop, "desktop:") {
		t.Errorf("commands = %q / %q", bundle.SCP, bundle.Taildrop)
	}
	if bundle.DownloadURL == "" {
		t.Error("no download URL")
	}
	download := f.get(bundle.DownloadURL)
	if download.Code != http.StatusOK {
		t.Fatalf("download = %d", download.Code)
	}
	if got := download.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Errorf("Content-Disposition = %q", got)
	}
	gz, err := gzip.NewReader(download.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	reader := tar.NewReader(gz)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, header.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "manifest.json") || !strings.Contains(joined, "real/positive/real_p_00001.wav") || !strings.Contains(joined, "real/negative/real_n_00001.wav") {
		t.Errorf("bundle entries = %v", names)
	}
}

func TestVoiceSamplesSubmitRefusesTooFewTakes(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	for i := 0; i < wakesample.MinTakes-1; i++ {
		f.post("/voice-samples/takes/clanker/positive", take(1.1+float64(i)/100, i), "audio/wav")
	}
	rec := f.post("/voice-samples/submit/clanker", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var view voiceSamplesView
	decodeJSON(t, rec, &view)
	if !strings.Contains(view.Error, "at least") {
		t.Errorf("error = %q", view.Error)
	}
}

func TestVoiceSamplesWritesAreOwnerOnlyAndSameOrigin(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	write := func(login string, sameOrigin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/voice-samples/takes/clanker/positive", bytes.NewReader(take(1, 1)))
		req.Host = "127.0.0.1:8082"
		req.RemoteAddr = "127.0.0.1:12345"
		if login != "" {
			req.Header.Set("Tailscale-User-Login", login)
		}
		if sameOrigin {
			req.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := write("", true); rec.Code != http.StatusForbidden {
		t.Errorf("no identity = %d, want 403", rec.Code)
	}
	if rec := write("mallory@example.com", true); rec.Code != http.StatusForbidden {
		t.Errorf("other identity = %d, want 403", rec.Code)
	}
	if rec := write(owner, false); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin = %d, want 403", rec.Code)
	}
	if rec := write(owner, true); rec.Code != http.StatusOK {
		t.Errorf("owner same-origin = %d, want 200", rec.Code)
	}
	for _, target := range []string{"/api/ui/voice-samples", "/voice-samples/take/clanker/positive/real_p_00001", "/voice-samples/bundle/clanker/clanker-20261112-093000.tar.gz"} {
		if rec := serve(f.handler, request(http.MethodGet, target, "")); rec.Code != http.StatusForbidden {
			t.Errorf("%s without identity = %d", target, rec.Code)
		}
	}
}

func TestVoiceSamplesPageHidesWithoutAStore(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	var view voiceSamplesView
	rec := getJSON(t, handler, "/api/ui/voice-samples", &view)
	if rec.Code != http.StatusOK || view.HasStore || view.RecordingAvailable {
		t.Fatalf("status/view = %d/%v/%v", rec.Code, view.HasStore, view.RecordingAvailable)
	}
	// The write route exists but refuses to run without a store.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/voice-samples/submit/clanker", nil)
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("submit without a store = %d, want 404", rec.Code)
	}
}

func TestVoiceSampleMediaCSPIsDocumentOnly(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	doc := f.get("/voice-samples")
	csp := doc.Header().Get("Content-Security-Policy")
	for _, want := range []string{"media-src 'self' blob:", "script-src 'self'", "form-action 'self' https://accounts.spotify.com", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("document CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("document CSP has an unsafe exception: %q", csp)
	}
	read := f.get("/api/ui/voice-samples")
	if got := read.Header().Get("Content-Security-Policy"); strings.Contains(got, "media-src") {
		t.Errorf("JSON CSP widened with media-src: %q", got)
	}
	audio := f.get("/voice-samples/take/clanker/positive/real_p_00001")
	if audio.Code != http.StatusNotFound {
		t.Errorf("missing take = %d, want 404", audio.Code)
	}
}

func TestVoiceSamplesBundleRejectsATraversalName(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	for _, target := range []string{
		"/voice-samples/bundle/clanker/../assistant.db",
		"/voice-samples/bundle/clanker/other-20261112-093000.tar.gz",
		"/voice-samples/bundle/../clanker/clanker-20261112-093000.tar.gz",
	} {
		if rec := f.get(target); rec.Code == http.StatusOK {
			t.Errorf("%s = 200, want a miss", target)
		}
	}
}

func TestVoiceSamplesBundleFilesAreModePrivate(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	for i := 0; i < wakesample.MinTakes; i++ {
		f.post("/voice-samples/takes/clanker/positive", take(1.0+float64(i)/100, i), "audio/wav")
	}
	f.post("/voice-samples/submit/clanker", nil, "")
	entries, err := os.ReadDir(f.store.ExportDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("exports = %v, %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode = %o, want 600", info.Mode().Perm())
	}
}
