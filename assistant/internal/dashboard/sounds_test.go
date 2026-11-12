package dashboard_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

var testCueFormat = audio.Format{Rate: 16000, Width: 2, Channels: 1}

// soundsSink records preview playback so the test can assert the schedule.
type soundsSink struct {
	mu      sync.Mutex
	writes  [][]byte
	formats []audio.Format
	started chan struct{}
}

func newSoundsSink() *soundsSink {
	return &soundsSink{started: make(chan struct{}, 16)}
}

func (s *soundsSink) Open(_ context.Context, format audio.Format) (io.WriteCloser, error) {
	s.mu.Lock()
	s.formats = append(s.formats, format)
	s.mu.Unlock()
	return &soundsWriter{sink: s}, nil
}

type soundsWriter struct{ sink *soundsSink }

func (w *soundsWriter) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	w.sink.writes = append(w.sink.writes, append([]byte(nil), p...))
	w.sink.mu.Unlock()
	select {
	case w.sink.started <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *soundsWriter) Close() error { return nil }

func (s *soundsSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *soundsSink) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(2 * time.Second):
		t.Fatal("preview did not reach the sink")
	}
}

// soundsFixture wires the real store, sound service and settings service.
type soundsFixture struct {
	t        *testing.T
	dir      string
	handler  *dashboard.Handler
	settings *settings.Service
	sounds   *sound.Service
	sink     *soundsSink
}

func newSoundsFixture(t *testing.T, previews dashboard.PreviewQueue) *soundsFixture {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cues, err := sound.New(ctx, st, dir, testCueFormat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	defs := cues.Definitions(sound.Config{})
	defs = append(defs,
		// #52 owns the real definitions; the page must render them dynamically.
		settings.Duration("ring.timer.max_ring", 5*time.Minute, settings.WithDescription("Longest Timer Ring")),
		settings.Int("ring.timer.volume", 80),
		settings.Duration("ring.timer.ramp", 0),
		settings.Duration("ring.timer.gap", time.Second),
		settings.String("ring.timer.speak_label", "start"),
		settings.Duration("ring.alarm.max_ring", 5*time.Minute),
		settings.Duration("ring.alarm.snooze", 9*time.Minute),
		settings.Int("ring.alarm.volume", 80),
		settings.Duration("ring.alarm.ramp", 30*time.Second),
		settings.Duration("ring.alarm.gap", time.Second),
		settings.String("ring.alarm.speak_label", "start"),
		settings.String("unrelated.key", "not a ring setting"),
	)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	sink := newSoundsSink()
	cues.Bind(svc, sink)
	handler := newHandler(t, dashboard.Options{Settings: svc, Sounds: cues, PreviewQueue: previews})
	return &soundsFixture{t: t, dir: dir, handler: handler, settings: svc, sounds: cues, sink: sink}
}

func (f *soundsFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, target, owner))
	return rec
}

func (f *soundsFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// upload posts one real multipart file.
func (f *soundsFixture) upload(filename string, data []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		f.t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		f.t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		f.t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sounds/upload", body)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// testWAV builds an interleaved PCM16 WAV for upload tests.
func testWAV(rate, channels int, duration time.Duration, amplitude float64) []byte {
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

func TestSoundsPageListsLibraryRolesAndRingSettings(t *testing.T) {
	f := newSoundsFixture(t, nil)
	rec := f.get("/sounds")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Sounds", "Upload a sound", "10 MB", "chime", "sounds.wake",
		"sounds.timer_ring", "sounds.alarm_ring", "sounds.reminder", "sounds.error",
		"Timer", "Alarm", "ring.timer.max_ring", "ring.alarm.snooze", "5m0s", "9m0s",
		"Browser preview", "Play on Pi",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "unrelated.key") {
		t.Error("page rendered a non-Ring setting")
	}
}

func TestSoundsUploadWAVAndMP3AppearWithDuration(t *testing.T) {
	f := newSoundsFixture(t, nil)
	rec := f.upload("My Birds!.wav", testWAV(8000, 2, 500*time.Millisecond, 0.2))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/sounds?flash=uploaded" {
		t.Fatalf("Location = %q", location)
	}
	page := f.get("/sounds").Body.String()
	for _, want := range []string{"My-Birds", "500ms", "Uploaded", "/sounds/audio/My-Birds"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	file, err := os.Open(filepath.Join("..", "audio", "testdata", "tone.mp3"))
	if err != nil {
		t.Fatalf("open mp3 fixture: %v", err)
	}
	mp3, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.upload("tone.mp3", mp3); rec.Code != http.StatusSeeOther {
		t.Fatalf("mp3 upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := f.sounds.Find("tone"); !ok {
		t.Fatal("mp3 upload is not in the library")
	}
}

func TestSoundsAudioServesCanonicalWAVWithRanges(t *testing.T) {
	f := newSoundsFixture(t, nil)
	if rec := f.upload("birds.wav", testWAV(16000, 1, 100*time.Millisecond, 0.2)); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d", rec.Code)
	}
	rec := f.get("/sounds/audio/birds")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "audio/wav" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if ranges := rec.Header().Get("Accept-Ranges"); ranges != "bytes" {
		t.Fatalf("Accept-Ranges = %q", ranges)
	}
	if body := rec.Body.Bytes(); len(body) < 44 || string(body[0:4]) != "RIFF" {
		t.Fatalf("body is not a WAV (%d bytes)", len(body))
	}
	rangeReq := httptest.NewRequest(http.MethodGet, "/sounds/audio/birds", nil)
	rangeReq.RemoteAddr = "127.0.0.1:12345"
	rangeReq.Header.Set("Tailscale-User-Login", owner)
	rangeReq.Header.Set("Range", "bytes=0-1")
	rangeRec := httptest.NewRecorder()
	f.handler.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", rangeRec.Code)
	}
	if got := rangeRec.Body.String(); got != "RI" {
		t.Fatalf("range body = %q", got)
	}
	if rec := f.get("/sounds/audio/..%2Fassistant.db"); rec.Code != http.StatusNotFound {
		t.Fatalf("path name status = %d, want 404", rec.Code)
	}
}

func TestSoundsUploadRejectsUndecodableOversizeAndLeavesNoFile(t *testing.T) {
	f := newSoundsFixture(t, nil)
	rec := f.upload("notes.wav", []byte("this is not audio"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("undecodable status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not a valid WAV or MP3") {
		t.Fatalf("error body = %s", body)
	}

	oversize := make([]byte, audio.MaxUploadBytes+1)
	rec = f.upload("huge.wav", oversize)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "10 MB limit") {
		t.Fatalf("oversize body = %s", body)
	}
	entries, err := os.ReadDir(f.sounds.LibraryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".wav") || strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("rejected upload left %q behind", entry.Name())
		}
	}
}

func TestSoundsRoleAssignmentEnforcesCapAndReset(t *testing.T) {
	f := newSoundsFixture(t, nil)
	if rec := f.upload("long.wav", testWAV(16000, 1, 1500*time.Millisecond, 0.2)); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec := f.post("/sounds/roles/wake", url.Values{"value": {"long"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wake assignment status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "cap") || !strings.Contains(body, "1s") {
		t.Fatalf("wake error = %s", body)
	}
	value, _ := f.settings.Get(sound.Key(sound.RoleWake))
	if value.Source != settings.SourceDefault {
		t.Fatalf("rejected wake assignment changed the setting: %+v", value)
	}

	rec = f.post("/sounds/roles/timer_ring", url.Values{"value": {"long"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("Timer Ring assignment status = %d, body = %s", rec.Code, rec.Body.String())
	}
	value, _ = f.settings.Get(sound.Key(sound.RoleTimerRing))
	if value.Source != settings.SourceOverride || value.Value != "long" {
		t.Fatalf("Timer Ring setting = %+v", value)
	}
	if page := f.get("/sounds").Body.String(); !strings.Contains(page, "Override") {
		t.Fatal("page does not show the override")
	}

	rec = f.post("/sounds/roles/timer_ring/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset status = %d", rec.Code)
	}
	value, _ = f.settings.Get(sound.Key(sound.RoleTimerRing))
	if value.Source != settings.SourceDefault || value.Value != sound.BuiltinChime {
		t.Fatalf("reset Timer Ring setting = %+v", value)
	}
}

func TestSoundsRingSettingsEditAndReset(t *testing.T) {
	f := newSoundsFixture(t, nil)
	rec := f.post("/sounds/values/ring.timer.max_ring", url.Values{"value": {"2m0s"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Duration("ring.timer.max_ring"); got != 2*time.Minute {
		t.Fatalf("max_ring = %s, want 2m", got)
	}
	if page := f.get("/sounds").Body.String(); !strings.Contains(page, "2m0s") {
		t.Fatal("page does not show the saved Ring value")
	}

	rec = f.post("/sounds/values/ring.timer.max_ring/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset status = %d", rec.Code)
	}
	if got := f.settings.Duration("ring.timer.max_ring"); got != 5*time.Minute {
		t.Fatalf("max_ring after reset = %s, want the 5m default", got)
	}

	// An invalid Ring value is rejected by the ordinary typed form conversion.
	rec = f.post("/sounds/values/ring.alarm.snooze", url.Values{"value": {"soon"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid duration status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "duration") {
		t.Fatalf("invalid duration body = %s", body)
	}
	if got := f.settings.Duration("ring.alarm.snooze"); got != 9*time.Minute {
		t.Fatalf("invalid save changed snooze to %s", got)
	}

	// A non-Ring key is not editable from the Sounds page.
	if rec := f.post("/sounds/values/unrelated.key", url.Values{"value": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("non-Ring key status = %d, want 404", rec.Code)
	}
}

func TestSoundsPreviewQueuesUntilTheReplyFinishes(t *testing.T) {
	queue := announce.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), announce.WithPlayback())
	defer queue.Close()
	f := newSoundsFixture(t, queue)

	queue.TurnStarted()
	rec := f.post("/sounds/preview/chime", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if page := f.get("/sounds?flash=queued").Body.String(); !strings.Contains(page, "pending Confirmation") {
		t.Fatal("page does not explain the preview queue")
	}
	select {
	case <-f.sink.started:
		t.Fatal("preview played during a reply")
	case <-time.After(100 * time.Millisecond):
	}
	queue.TurnFinished()
	f.sink.wait(t)
	if f.sink.count() == 0 {
		t.Fatal("preview did not reach the sink after the reply")
	}
}

func TestSoundsPreviewWaitsForAPendingConfirmation(t *testing.T) {
	var mu sync.Mutex
	blocked := true
	queue := announce.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		announce.WithPlayback(), announce.WithBlocked(func() bool {
			mu.Lock()
			defer mu.Unlock()
			return blocked
		}))
	defer queue.Close()
	f := newSoundsFixture(t, queue)

	if rec := f.post("/sounds/preview/chime", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("preview status = %d", rec.Code)
	}
	select {
	case <-f.sink.started:
		t.Fatal("preview played while a Confirmation was pending")
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	blocked = false
	mu.Unlock()
	queue.TurnFinished()
	f.sink.wait(t)
}

func TestSoundsMutationsRequireIdentityAndSameOrigin(t *testing.T) {
	f := newSoundsFixture(t, nil)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "birds.wav")
	part.Write(testWAV(16000, 1, 100*time.Millisecond, 0.2))
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/sounds/upload", bytes.NewReader(body.Bytes()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing identity status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/sounds/roles/wake", strings.NewReader("value=chime"))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}
}

func TestSoundsFormsWorkAsHTMXFragments(t *testing.T) {
	f := newSoundsFixture(t, nil)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "birds.wav")
	part.Write(testWAV(16000, 1, 100*time.Millisecond, 0.2))
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/sounds/upload", bytes.NewReader(body.Bytes()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx upload status = %d", rec.Code)
	}
	content := rec.Body.String()
	if !strings.Contains(content, `id="sounds-content"`) || strings.Contains(content, "<html") {
		t.Fatalf("htmx response is not a fragment: %s", content)
	}
	if !strings.Contains(content, "Uploaded") {
		t.Fatal("htmx fragment lacks the success notice")
	}

	// A cap rejection over htmx stays 200 with role=alert so the swap shows it.
	body.Reset()
	writer = multipart.NewWriter(body)
	part, _ = writer.CreateFormFile("file", "long.wav")
	part.Write(testWAV(16000, 1, 1500*time.Millisecond, 0.2))
	writer.Close()
	req = httptest.NewRequest(http.MethodPost, "/sounds/upload", bytes.NewReader(body.Bytes()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx long upload status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/sounds/roles/wake", strings.NewReader("value=long"))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx cap error status = %d, want 200", rec.Code)
	}
	fragment := rec.Body.String()
	if !strings.Contains(fragment, `role="alert"`) || !strings.Contains(fragment, "cap") || strings.Contains(fragment, "<html") {
		t.Fatalf("htmx cap error fragment = %s", fragment)
	}
}

// blockingPreviewSink holds a preview inside Write until its context is
// cancelled, which is how the shared serial sink behaves when pacat is killed.
type blockingPreviewSink struct {
	started  chan struct{}
	canceled chan struct{}
	start    sync.Once
	cancel   sync.Once
}

func (s *blockingPreviewSink) Open(ctx context.Context, _ audio.Format) (io.WriteCloser, error) {
	return &blockingPreviewWriter{sink: s, ctx: ctx}, nil
}

type blockingPreviewWriter struct {
	sink *blockingPreviewSink
	ctx  context.Context
}

func (w *blockingPreviewWriter) Write(_ []byte) (int, error) {
	w.sink.start.Do(func() { close(w.sink.started) })
	<-w.ctx.Done()
	w.sink.cancel.Do(func() { close(w.sink.canceled) })
	return 0, w.ctx.Err()
}

func (w *blockingPreviewWriter) Close() error { return nil }

func TestSoundsPreviewIsCancelledByBargeIn(t *testing.T) {
	queue := announce.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), announce.WithPlayback())
	defer queue.Close()
	f := newSoundsFixture(t, queue)

	sink := &blockingPreviewSink{started: make(chan struct{}), canceled: make(chan struct{})}
	f.sounds.Bind(f.settings, sink)

	if rec := f.post("/sounds/preview/chime", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("preview status = %d", rec.Code)
	}
	select {
	case <-sink.started:
	case <-time.After(2 * time.Second):
		t.Fatal("preview did not start")
	}
	queue.TurnStarted()
	select {
	case <-sink.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("preview was not cancelled by Barge-in")
	}
}

func TestSoundsPageEscapesAManuallyDroppedName(t *testing.T) {
	f := newSoundsFixture(t, nil)
	if err := os.WriteFile(filepath.Join(f.sounds.LibraryDir(), "evil<script>.wav"), testWAV(16000, 1, 100*time.Millisecond, 0.2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	body := f.get("/sounds").Body.String()
	if !strings.Contains(body, "evil&lt;script&gt;") {
		t.Fatal("escaped library name is missing")
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("library name was rendered as markup")
	}
}

func TestSoundsAudioRouteServesAManuallyDroppedNameWithSpaces(t *testing.T) {
	f := newSoundsFixture(t, nil)
	if err := os.WriteFile(filepath.Join(f.sounds.LibraryDir(), "my birds.wav"), testWAV(16000, 1, 100*time.Millisecond, 0.2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	page := f.get("/sounds").Body.String()
	if !strings.Contains(page, `/sounds/audio/my%20birds`) {
		t.Fatalf("page lacks the escaped audio URL: %s", page)
	}
	if rec := f.get("/sounds/audio/my%20birds"); rec.Code != http.StatusOK {
		t.Fatalf("escaped audio status = %d, want 200", rec.Code)
	}
}
