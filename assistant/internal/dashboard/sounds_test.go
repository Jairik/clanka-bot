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
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
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
	defs = append(defs, ring.Definitions()...)
	defs = append(defs,
		// A future per-kind field must appear without a page edit.
		settings.String("ring.timer.future_field", "shown without a page change"),
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

// soundsResp decodes the Sounds view (encoding/json default field names).
type soundsResp struct {
	Flash      string
	Error      string
	HasLibrary bool
	MaxUpload  string
	Sounds     []struct{ Name, Duration, Added, Origin, AudioURL string }
	HasRoles   bool
	Roles      []struct {
		Role, Label, Key, Cap, Display, Source, SourceLabel, LowerLabel string
		Override                                                        bool
		Options                                                         []struct {
			Value, Label string
			Selected     bool
		}
	}
	HasRing   bool
	RingKinds []struct {
		Kind, Label string
		Settings    []settingRow
	}
}

func (v soundsResp) hasSound(name string) bool {
	for _, row := range v.Sounds {
		if row.Name == name {
			return true
		}
	}
	return false
}

func (v soundsResp) ringSetting(key string) (settingRow, bool) {
	for _, kind := range v.RingKinds {
		for _, row := range kind.Settings {
			if row.Key == key {
				return row, true
			}
		}
	}
	return settingRow{}, false
}

// view reads /api/ui/sounds, which must be a 200.
func (f *soundsFixture) view() (soundsResp, string) {
	f.t.Helper()
	return f.viewAt("/api/ui/sounds")
}

func (f *soundsFixture) viewAt(target string) (soundsResp, string) {
	f.t.Helper()
	var v soundsResp
	rec := getJSON(f.t, f.handler, target, &v)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	return v, rec.Body.String()
}

// decode decodes a write response into the Sounds view.
func decodeSounds(t *testing.T, rec *httptest.ResponseRecorder) soundsResp {
	t.Helper()
	var v soundsResp
	decodeJSON(t, rec, &v)
	return v
}

func (f *soundsFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
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
	req.Host = "127.0.0.1:8082"
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

func TestSoundsReadListsLibraryRolesAndRingSettings(t *testing.T) {
	f := newSoundsFixture(t, nil)
	v, body := f.view()
	if !v.HasLibrary || !v.HasRoles || !v.HasRing || v.MaxUpload != "10 MB" {
		t.Errorf("view flags = library %v roles %v ring %v max %q", v.HasLibrary, v.HasRoles, v.HasRing, v.MaxUpload)
	}
	if !v.hasSound("chime") {
		t.Errorf("library lacks the built-in chime: %+v", v.Sounds)
	}
	roles := map[string]string{}
	for _, role := range v.Roles {
		roles[role.Key] = role.Role
	}
	for _, key := range []string{"sounds.wake", "sounds.timer_ring", "sounds.alarm_ring", "sounds.reminder", "sounds.error"} {
		if _, ok := roles[key]; !ok {
			t.Errorf("roles lack %s: %v", key, roles)
		}
	}
	kinds := map[string]bool{}
	for _, kind := range v.RingKinds {
		kinds[kind.Label] = true
	}
	if !kinds["Timer"] || !kinds["Alarm"] {
		t.Errorf("Ring kinds = %v, want Timer and Alarm", kinds)
	}
	for key, want := range map[string]string{
		"ring.timer.max_ring": "5m0s", "ring.alarm.ramp": "30s",
		"ring.timer.future_field": "shown without a page change",
	} {
		row, ok := v.ringSetting(key)
		if !ok || row.Value != want {
			t.Errorf("Ring setting %s = %+v (found %v), want value %q", key, row, ok, want)
		}
	}
	for _, key := range []string{"ring.timer.volume", "ring.alarm.volume"} {
		if _, ok := v.ringSetting(key); !ok {
			t.Errorf("Ring settings lack %s", key)
		}
	}
	if strings.Contains(body, "unrelated.key") {
		t.Error("view carried a non-Ring setting")
	}
}

func TestSoundsUploadWAVAndMP3AppearWithDuration(t *testing.T) {
	f := newSoundsFixture(t, nil)
	rec := f.upload("My Birds!.wav", testWAV(8000, 2, 500*time.Millisecond, 0.2))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	uploaded := decodeSounds(t, rec)
	if !strings.HasPrefix(uploaded.Flash, "Uploaded.") {
		t.Errorf("upload Flash = %q", uploaded.Flash)
	}
	found := false
	for _, row := range uploaded.Sounds {
		if row.Name == "My-Birds" {
			found = true
			if row.Duration != "500ms" || row.Origin != "Uploaded" || row.AudioURL != "/sounds/audio/My-Birds" {
				t.Errorf("uploaded row = %+v", row)
			}
		}
	}
	if !found {
		t.Errorf("upload did not appear in the refreshed view: %+v", uploaded.Sounds)
	}
	if v, _ := f.view(); !v.hasSound("My-Birds") {
		t.Error("a plain read does not list the upload")
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
	if rec := f.upload("tone.mp3", mp3); rec.Code != http.StatusOK {
		t.Fatalf("mp3 upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := f.sounds.Find("tone"); !ok {
		t.Fatal("mp3 upload is not in the library")
	}
}

func TestSoundsAudioServesCanonicalWAVWithRanges(t *testing.T) {
	f := newSoundsFixture(t, nil)
	if rec := f.upload("birds.wav", testWAV(16000, 1, 100*time.Millisecond, 0.2)); rec.Code != http.StatusOK {
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
	rangeReq.Host = "127.0.0.1:8082"
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
	if v := decodeSounds(t, rec); !strings.Contains(v.Error, "not a valid WAV or MP3") {
		t.Fatalf("error = %q", v.Error)
	}

	oversize := make([]byte, audio.MaxUploadBytes+1)
	rec = f.upload("huge.wav", oversize)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v := decodeSounds(t, rec); !strings.Contains(v.Error, "10 MB limit") {
		t.Fatalf("oversize error = %q", v.Error)
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
	if rec := f.upload("long.wav", testWAV(16000, 1, 1500*time.Millisecond, 0.2)); rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec := f.post("/sounds/roles/wake", url.Values{"value": {"long"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wake assignment status = %d", rec.Code)
	}
	if v := decodeSounds(t, rec); !strings.Contains(v.Error, "cap") || !strings.Contains(v.Error, "1s") {
		t.Fatalf("wake error = %q", v.Error)
	}
	value, _ := f.settings.Get(sound.Key(sound.RoleWake))
	if value.Source != settings.SourceDefault {
		t.Fatalf("rejected wake assignment changed the setting: %+v", value)
	}

	rec = f.post("/sounds/roles/timer_ring", url.Values{"value": {"long"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("Timer Ring assignment status = %d, body = %s", rec.Code, rec.Body.String())
	}
	value, _ = f.settings.Get(sound.Key(sound.RoleTimerRing))
	if value.Source != settings.SourceOverride || value.Value != "long" {
		t.Fatalf("Timer Ring setting = %+v", value)
	}
	assigned := decodeSounds(t, rec)
	overridden := false
	for _, role := range assigned.Roles {
		if role.Role == "timer_ring" {
			overridden = role.Override && role.SourceLabel == "Override" && role.Display == "long"
		}
	}
	if !overridden {
		t.Fatalf("view does not show the Timer Ring override: %+v", assigned.Roles)
	}

	rec = f.post("/sounds/roles/timer_ring/reset", url.Values{})
	if rec.Code != http.StatusOK {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Duration("ring.timer.max_ring"); got != 2*time.Minute {
		t.Fatalf("max_ring = %s, want 2m", got)
	}
	if v, _ := f.view(); func() string { row, _ := v.ringSetting("ring.timer.max_ring"); return row.Value }() != "2m0s" {
		t.Fatal("view does not show the saved Ring value")
	}

	rec = f.post("/sounds/values/ring.timer.max_ring/reset", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d", rec.Code)
	}
	if got := f.settings.Duration("ring.timer.max_ring"); got != 5*time.Minute {
		t.Fatalf("max_ring after reset = %s, want the 5m default", got)
	}

	// The real #52 definitions save and reset through the same typed forms.
	rec = f.post("/sounds/values/ring.timer.volume", url.Values{"value": {"45"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("volume save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Int("ring.timer.volume"); got != 45 {
		t.Fatalf("timer volume = %d, want 45", got)
	}
	rec = f.post("/sounds/values/ring.timer.volume/reset", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("volume reset status = %d", rec.Code)
	}
	if got := f.settings.Int("ring.timer.volume"); got != 80 {
		t.Fatalf("timer volume after reset = %d, want 80", got)
	}
	rec = f.post("/sounds/values/ring.alarm.ramp", url.Values{"value": {"1m0s"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("alarm ramp save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Duration("ring.alarm.ramp"); got != time.Minute {
		t.Fatalf("alarm ramp = %s, want 1m", got)
	}
	rec = f.post("/sounds/values/ring.alarm.ramp/reset", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("alarm ramp reset status = %d", rec.Code)
	}
	if got := f.settings.Duration("ring.alarm.ramp"); got != 30*time.Second {
		t.Fatalf("alarm ramp after reset = %s, want the 30s default", got)
	}

	// An invalid Ring value is rejected by the ordinary typed form conversion.
	rec = f.post("/sounds/values/ring.alarm.ramp", url.Values{"value": {"soon"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid duration status = %d", rec.Code)
	}
	if v := decodeSounds(t, rec); !strings.Contains(v.Error, "duration") {
		t.Fatalf("invalid duration error = %q", v.Error)
	}
	if got := f.settings.Duration("ring.alarm.ramp"); got != 30*time.Second {
		t.Fatalf("invalid save changed the alarm ramp to %s", got)
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
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v := decodeSounds(t, rec); !strings.Contains(v.Flash, "pending Confirmation") {
		t.Fatalf("preview response does not explain the queue: %q", v.Flash)
	}
	if v, _ := f.viewAt("/api/ui/sounds?flash=queued"); !strings.Contains(v.Flash, "pending Confirmation") {
		t.Fatal("?flash=queued read does not explain the preview queue")
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

	if rec := f.post("/sounds/preview/chime", url.Values{}); rec.Code != http.StatusOK {
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
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing identity status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/sounds/roles/wake", strings.NewReader("value=chime"))
	req.Host = "127.0.0.1:8082"
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

// TestSoundsWritesAnswerJSONWithTheRealStatus pins the write contract the
// React app relies on: an upload or role write answers 200 with the refreshed
// view and a Flash, and a rejection answers the view with the real 4xx status
// and an Error.
func TestSoundsWritesAnswerJSONWithTheRealStatus(t *testing.T) {
	f := newSoundsFixture(t, nil)
	rec := f.upload("birds.wav", testWAV(16000, 1, 100*time.Millisecond, 0.2))
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("upload status = %d", rec.Code)
	}
	if v := decodeSounds(t, rec); !strings.HasPrefix(v.Flash, "Uploaded.") || !v.hasSound("birds") {
		t.Fatalf("upload view = flash %q sounds %+v", v.Flash, v.Sounds)
	}

	// A cap rejection on upload is not a failure of the upload itself (the
	// library has no cap), but on a role it is a 400 with the message.
	if rec := f.upload("long.wav", testWAV(16000, 1, 1500*time.Millisecond, 0.2)); rec.Code != http.StatusOK {
		t.Fatalf("long upload status = %d", rec.Code)
	}
	rec = f.post("/sounds/roles/wake", url.Values{"value": {"long"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cap error status = %d, want 400", rec.Code)
	}
	if v := decodeSounds(t, rec); !strings.Contains(v.Error, "cap") || v.Flash != "" {
		t.Fatalf("cap error view = error %q flash %q", v.Error, v.Flash)
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

	if rec := f.post("/sounds/preview/chime", url.Values{}); rec.Code != http.StatusOK {
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

func TestSoundsHostileLibraryNameRoundTripsInertly(t *testing.T) {
	f := newSoundsFixture(t, nil)
	const hostile = "evil<script>"
	if err := os.WriteFile(filepath.Join(f.sounds.LibraryDir(), hostile+".wav"), testWAV(16000, 1, 100*time.Millisecond, 0.2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	v, body := f.view()
	if !v.hasSound(hostile) {
		t.Fatalf("hostile library name did not round-trip exactly: %+v", v.Sounds)
	}
	noRawScript(t, body)
}

func TestSoundsAudioRouteServesAManuallyDroppedNameWithSpaces(t *testing.T) {
	f := newSoundsFixture(t, nil)
	if err := os.WriteFile(filepath.Join(f.sounds.LibraryDir(), "my birds.wav"), testWAV(16000, 1, 100*time.Millisecond, 0.2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sounds.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	v, _ := f.view()
	urls := map[string]string{}
	for _, row := range v.Sounds {
		urls[row.Name] = row.AudioURL
	}
	if urls["my birds"] != "/sounds/audio/my%20birds" {
		t.Fatalf("audio URL = %q, want the path-escaped name: %v", urls["my birds"], urls)
	}
	if rec := f.get("/sounds/audio/my%20birds"); rec.Code != http.StatusOK {
		t.Fatalf("escaped audio status = %d, want 200", rec.Code)
	}
}
