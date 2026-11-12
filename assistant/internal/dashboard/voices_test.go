package dashboard_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib/voicelibtest"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// fakeTTS is a minimal Wyoming server covering the events the Voice page uses:
// voice-info, voice-validate, voice-forget and synthesize. It records every
// call so a test can prove an invalid upload never reaches the model.
type fakeValidateCall struct {
	Path  string
	Cache bool
}

type fakeSynthCall struct {
	Text  string
	Voice string
	Rate  float64
}

type fakeTTS struct {
	addr string

	ln  net.Listener
	wg  sync.WaitGroup
	mu  sync.Mutex
	rev string

	validates      []fakeValidateCall
	forgotten      []string
	synths         []fakeSynthCall
	failSynth      bool
	failValidateAt int
	validateCount  int
}

func newFakeTTS(t *testing.T, revision string) *fakeTTS {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake TTS listen: %v", err)
	}
	f := &fakeTTS{addr: listener.Addr().String(), ln: listener, rev: revision}
	t.Cleanup(func() {
		_ = listener.Close()
		f.wg.Wait()
	})
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.serve(conn)
			}()
		}
	}()
	return f
}

func (f *fakeTTS) Addr() string { return f.addr }

func (f *fakeTTS) setRevision(revision string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev = revision
}

func (f *fakeTTS) failValidationAt(call int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failValidateAt = call
}

func (f *fakeTTS) setSynthFailure(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSynth = fail
}

func (f *fakeTTS) validationCalls() []fakeValidateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeValidateCall(nil), f.validates...)
}

func (f *fakeTTS) forgetCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgotten...)
}

func (f *fakeTTS) synthCalls() []fakeSynthCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSynthCall(nil), f.synths...)
}

// serve reads one event at a time until the client closes the connection.
func (f *fakeTTS) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var frame struct {
			Type          string         `json:"type"`
			Data          map[string]any `json:"data"`
			PayloadLength int            `json:"payload_length"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			return
		}
		if frame.PayloadLength > 0 {
			payload := make([]byte, frame.PayloadLength)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return
			}
		}
		switch frame.Type {
		case "voice-info":
			f.reply(conn, map[string]any{"type": "voice-info-result", "data": map[string]any{
				"revision": f.revision(), "voices": []string{"alba"}, "dir": "/voices",
			}}, nil)
		case "voice-validate":
			path, _ := frame.Data["path"].(string)
			cache := true
			if raw, ok := frame.Data["cache"]; ok {
				cache, _ = raw.(bool)
			}
			f.mu.Lock()
			f.validateCount++
			call := f.validateCount
			f.validates = append(f.validates, fakeValidateCall{Path: path, Cache: cache})
			fail := f.failValidateAt > 0 && call == f.failValidateAt
			revision := f.rev
			f.mu.Unlock()
			if fail {
				f.reply(conn, map[string]any{"type": "voice-validated", "data": map[string]any{
					"ok": false, "revision": "", "error": "model rejected the state",
				}}, nil)
				continue
			}
			f.reply(conn, map[string]any{"type": "voice-validated", "data": map[string]any{
				"ok": true, "revision": revision, "error": "",
			}}, nil)
		case "voice-forget":
			voice, _ := frame.Data["voice"].(string)
			f.mu.Lock()
			f.forgotten = append(f.forgotten, voice)
			f.mu.Unlock()
			f.reply(conn, map[string]any{"type": "voice-forgotten", "data": map[string]any{"ok": true, "error": ""}}, nil)
		case "synthesize":
			text, _ := frame.Data["text"].(string)
			voice := ""
			if fields, ok := frame.Data["voice"].(map[string]any); ok {
				voice, _ = fields["name"].(string)
			}
			rate, _ := frame.Data["rate"].(float64)
			f.mu.Lock()
			f.synths = append(f.synths, fakeSynthCall{Text: text, Voice: voice, Rate: rate})
			fail := f.failSynth
			f.mu.Unlock()
			if fail {
				f.reply(conn, map[string]any{"type": "error", "data": map[string]any{"text": "model failed", "code": "synthesis-failed"}}, nil)
				continue
			}
			f.reply(conn, map[string]any{"type": "audio-start", "data": map[string]any{"rate": 24000, "width": 2, "channels": 1}}, nil)
			f.reply(conn, map[string]any{"type": "audio-chunk"}, []byte{1, 2, 3, 4, 5, 6, 7, 8})
			f.reply(conn, map[string]any{"type": "audio-stop"}, nil)
		}
	}
}

func (f *fakeTTS) revision() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rev
}

func (f *fakeTTS) reply(conn net.Conn, event map[string]any, payload []byte) {
	if payload != nil {
		event["payload_length"] = len(payload)
	}
	raw, _ := json.Marshal(event)
	_, _ = conn.Write(append(raw, '\n'))
	if payload != nil {
		_, _ = conn.Write(payload)
	}
}

// voicesFixture wires the real store, library and settings services over the
// fake TTS boundary.
type voicesFixture struct {
	t          *testing.T
	dir        string
	handler    *dashboard.Handler
	settings   *settings.Service
	voices     *voicelib.Service
	tts        *fakeTTS
	probe      *voicelib.TTSProbe
	observedMu sync.Mutex
	observed   []string
}

func newVoicesFixture(t *testing.T) *voicesFixture {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	tts := newFakeTTS(t, "test-revision-1")
	st, err := store.Open(ctx, filepath.Join(dir, "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	voices, err := voicelib.New(ctx, st, voicelib.Config{
		Dir:      filepath.Join(dir, "voices"),
		TTSDir:   "/voices",
		Default:  "alba",
		Builtins: []string{"alba"},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	settingsSvc, err := settings.New(ctx, st, voices.Definitions(voicelib.DefinitionConfig{Name: "alba", Rate: 1.0})...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	voices.Bind(settingsSvc)
	client := wyoming.NewClient(tts.Addr(), wyoming.WithTimeout(2*time.Second))
	probe := voicelib.NewTTSProbe(client, 2*time.Second)
	if err := voices.Rescan(ctx, probe); err != nil {
		t.Fatalf("initial Rescan: %v", err)
	}
	f := &voicesFixture{t: t, dir: dir, settings: settingsSvc, voices: voices, tts: tts, probe: probe}
	f.handler = newHandler(t, dashboard.Options{
		Settings:     settingsSvc,
		Voices:       voices,
		VoiceProbe:   probe,
		VoicePreview: voicesample.New(client, 2*time.Second),
	})
	settingsSvc.SetObserver(func(keys []string) {
		f.observedMu.Lock()
		f.observed = append(f.observed, keys...)
		f.observedMu.Unlock()
	})
	return f
}

func (f *voicesFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, target, owner))
	return rec
}

func (f *voicesFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
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

func (f *voicesFixture) upload(filename string, data []byte, replace bool) *httptest.ResponseRecorder {
	f.t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if replace {
		if err := writer.WriteField("replace", "yes"); err != nil {
			f.t.Fatalf("write replace field: %v", err)
		}
	}
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
	req := httptest.NewRequest(http.MethodPost, "/voices/upload", body)
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *voicesFixture) observedKeys() []string {
	f.observedMu.Lock()
	defer f.observedMu.Unlock()
	return append([]string(nil), f.observed...)
}

// stateBytes builds a structurally valid upload payload.
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

func loadVoicePath(t *testing.T, f *voicesFixture, name string) string {
	t.Helper()
	voice, ok := f.voices.Get(name)
	if !ok {
		t.Fatalf("voice %q is not in the library", name)
	}
	return voice.Path
}

func TestVoicesPageListsTheLibraryAndSettings(t *testing.T) {
	f := newVoicesFixture(t)
	rec := f.get("/voices")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Voices", "Upload a voice state", "32 MB", ".safetensors",
		"alba", "Built-in", "test-revision-1", "Speech rate", "voice.rate",
		"Browser preview", `href="/sounds"`, `href="/secrets"`, `href="/settings"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(body, `href="/voices" aria-current="page"`) {
		t.Error("nav did not mark Voices current")
	}
}

func TestVoicesUploadRejectsBadFilesBeforeTTS(t *testing.T) {
	t.Run("wrong extension", func(t *testing.T) {
		f := newVoicesFixture(t)
		rec := f.upload("jarvis.pt", stateBytes(t, 2, true), false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "Only .safetensors") {
			t.Fatalf("body = %s", body)
		}
		if calls := f.tts.validationCalls(); len(calls) != 0 {
			t.Fatalf("TTS was called for a .pt file: %v", calls)
		}
		assertNoStagedFiles(t, filepath.Join(f.dir, "voices"))
	})

	t.Run("malformed header", func(t *testing.T) {
		f := newVoicesFixture(t)
		rec := f.upload("jarvis.safetensors", []byte("this is not a safetensors header"), false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "not a valid Pocket TTS voice state") {
			t.Fatalf("body = %s", body)
		}
		if calls := f.tts.validationCalls(); len(calls) != 0 {
			t.Fatalf("TTS was called for a malformed header: %v", calls)
		}
		assertNoStagedFiles(t, filepath.Join(f.dir, "voices"))
	})

	t.Run("oversize", func(t *testing.T) {
		f := newVoicesFixture(t)
		rec := f.upload("jarvis.safetensors", make([]byte, voicelib.MaxStateBytes+1), false)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "32 MB") {
			t.Fatalf("body = %s", body)
		}
		if calls := f.tts.validationCalls(); len(calls) != 0 {
			t.Fatalf("TTS was called for an oversize file: %v", calls)
		}
		assertNoStagedFiles(t, filepath.Join(f.dir, "voices"))
	})
}

func TestVoicesUploadValidatesPublishesAndSelects(t *testing.T) {
	f := newVoicesFixture(t)
	rec := f.upload("Jarvis.safetensors", stateBytes(t, 3, true), false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/voices?flash=voice-uploaded" {
		t.Fatalf("Location = %q", location)
	}

	calls := f.tts.validationCalls()
	if len(calls) != 2 {
		t.Fatalf("validation calls = %v, want staging then final", calls)
	}
	if calls[0].Cache || !strings.HasPrefix(filepath.Base(calls[0].Path), ".upload-") {
		t.Fatalf("staging validation = %+v", calls[0])
	}
	if !calls[1].Cache || calls[1].Path != "/voices/Jarvis.safetensors" {
		t.Fatalf("final validation = %+v", calls[1])
	}
	if forgotten := f.tts.forgetCalls(); len(forgotten) != 1 || forgotten[0] != "/voices/Jarvis.safetensors" {
		t.Fatalf("forget calls = %v", forgotten)
	}
	voice, ok := f.voices.Get("Jarvis")
	if !ok || !voice.Ready() || voice.ValidatedRevision != "test-revision-1" {
		t.Fatalf("voice = %+v, %v", voice, ok)
	}
	if _, err := os.Stat(loadVoicePath(t, f, "Jarvis")); err != nil {
		t.Fatalf("published state: %v", err)
	}
	assertNoStagedFiles(t, filepath.Join(f.dir, "voices"))

	page := f.get("/voices").Body.String()
	for _, want := range []string{"Jarvis", "Ready", "test-revision-1", "/voices/audio/Jarvis", "Use this voice", "Source / consent note"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	rec = f.post("/voices/active/Jarvis", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("select status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(voicelib.KeyName); got != "Jarvis" {
		t.Fatalf("voice.name = %q, want Jarvis", got)
	}
	ref, rate, ok := f.voices.ActiveSpeech()
	if !ok || ref != "/voices/Jarvis.safetensors" || rate != 1.0 {
		t.Fatalf("ActiveSpeech = %q, %v, %v", ref, rate, ok)
	}
	if !containsString(f.observedKeys(), voicelib.KeyName) {
		t.Fatalf("settings observer did not see voice.name: %v", f.observedKeys())
	}
	page = f.get("/voices").Body.String()
	if !strings.Contains(page, "Active voice") {
		t.Error("page does not mark the active voice")
	}
}

func TestVoicesNamesWithSpacesRoundTripThroughURLs(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("My Voice.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := f.get("/voices").Body.String()
	for _, want := range []string{"/voices/audio/My%20Voice", "/voices/active/My%20Voice", "/voices/note/My%20Voice"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if rec := f.post("/voices/active/My%20Voice", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("select = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(voicelib.KeyName); got != "My Voice" {
		t.Fatalf("voice.name = %q", got)
	}
	if rec := f.get("/voices/audio/My%20Voice"); rec.Code != http.StatusOK {
		t.Fatalf("preview = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestVoicesUploadReplacesOnlyWithTheExplicitFlag(t *testing.T) {
	f := newVoicesFixture(t)
	first := stateBytes(t, 2, true)
	if rec := f.upload("jarvis.safetensors", first, false); rec.Code != http.StatusSeeOther {
		t.Fatalf("first upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := f.upload("jarvis.safetensors", stateBytes(t, 3, true), false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "already exists") {
		t.Fatalf("duplicate body = %s", body)
	}
	if before, after := first, mustRead(t, loadVoicePath(t, f, "jarvis")); !bytes.Equal(before, after) {
		t.Fatal("a refused duplicate changed the file")
	}

	rec = f.upload("jarvis.safetensors", stateBytes(t, 4, false), true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("replace status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if after := mustRead(t, loadVoicePath(t, f, "jarvis")); bytes.Equal(first, after) {
		t.Fatal("replace did not publish the new bytes")
	}
	if calls := f.tts.forgetCalls(); len(calls) < 2 {
		t.Fatalf("replace did not evict the cached final path: %v", calls)
	}
}

func TestVoicesUploadRollsBackAFinalPathFailure(t *testing.T) {
	f := newVoicesFixture(t)
	// The staging smoke test passes; the validation of the published path
	// fails, so the upload must be removed and leave no row.
	f.tts.failValidationAt(2)
	rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Nothing was published") {
		t.Fatalf("body = %s", body)
	}
	if _, ok := f.voices.Get("jarvis"); ok {
		t.Fatal("failed upload entered the index")
	}
	assertNoStagedFiles(t, filepath.Join(f.dir, "voices"))
}

func TestVoicesRateSavesRejectsAndResets(t *testing.T) {
	f := newVoicesFixture(t)
	rec := f.post("/voices/rate", url.Values{"value": {"1.25"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Float(voicelib.KeyRate); got != 1.25 {
		t.Fatalf("voice.rate = %v", got)
	}
	if !containsString(f.observedKeys(), voicelib.KeyRate) {
		t.Fatalf("settings observer did not see voice.rate: %v", f.observedKeys())
	}

	for _, value := range []string{"3", "0.1", "fast"} {
		rec := f.post("/voices/rate", url.Values{"value": {value}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("rate %q status = %d, body = %s", value, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "Rate must be a number") {
			t.Fatalf("rate %q body = %s", value, body)
		}
		if got := f.settings.Float(voicelib.KeyRate); got != 1.25 {
			t.Fatalf("rejected rate %q changed the setting to %v", value, got)
		}
	}

	rec = f.post("/voices/rate/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Float(voicelib.KeyRate); got != 1.0 {
		t.Fatalf("reset rate = %v, want 1.0", got)
	}
	page := f.get("/voices").Body.String()
	if !strings.Contains(page, "Rate multiplier") || !strings.Contains(page, `value="1"`) {
		t.Error("rate form does not show the effective value")
	}
}

func TestVoicesNoteIsSavedEscapedAndSurvivesRescan(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	note := `<b>cloned from a friend's clip</b> & recorded with permission`
	rec := f.post("/voices/note/jarvis", url.Values{"note": {note}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("note status = %d, body = %s", rec.Code, rec.Body.String())
	}
	voice, _ := f.voices.Get("jarvis")
	if voice.Note != note {
		t.Fatalf("stored note = %q, want verbatim %q", voice.Note, note)
	}
	page := f.get("/voices").Body.String()
	if strings.Contains(page, note) || strings.Contains(page, "<b>cloned") {
		t.Fatal("page rendered the note as HTML")
	}
	if !strings.Contains(page, "&lt;b&gt;cloned from a friend&#39;s clip&lt;/b&gt;") {
		t.Fatal("page does not show the escaped note")
	}

	// A rescan (for example after TTS restarts) must keep the note.
	if err := f.voices.Rescan(context.Background(), f.probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if voice, _ := f.voices.Get("jarvis"); voice.Note != note {
		t.Fatalf("note after rescan = %q", voice.Note)
	}
}

func TestVoicesStaleIsFlaggedAndDisablesSelectionAndPreview(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	f.tts.setRevision("test-revision-2")
	if err := f.voices.Rescan(context.Background(), f.probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if voice, _ := f.voices.Get("jarvis"); !voice.Stale || voice.Ready() {
		t.Fatalf("jarvis = %+v, want stale", voice)
	}

	synthBefore := len(f.tts.synthCalls())
	page := f.get("/voices").Body.String()
	for _, want := range []string{"Stale", "Re-export it on the desktop", "cannot be selected", "test-revision-2"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "/voices/audio/jarvis") {
		t.Error("stale voice still offers a preview")
	}
	rec := f.post("/voices/active/jarvis", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("select stale status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("stale selection changed voice.name to %q", got)
	}
	audio := f.get("/voices/audio/jarvis")
	if audio.Code != http.StatusConflict {
		t.Fatalf("stale preview status = %d, want 409", audio.Code)
	}
	if got := len(f.tts.synthCalls()); got != synthBefore {
		t.Fatalf("stale preview reached TTS (%d -> %d calls)", synthBefore, got)
	}
}

func TestVoicesBrowserPreviewIsAuthenticatedWAVAndKeepsTheActiveVoice(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.post("/voices/rate", url.Values{"value": {"1.25"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("rate = %d", rec.Code)
	}
	if rec := f.post("/voices/active/jarvis", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("select = %d", rec.Code)
	}
	// Preview a different voice: the persistent active setting must not move.
	rec := f.get("/voices/audio/alba")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "audio/wav" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	body := rec.Body.Bytes()
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("preview is not a WAV (%d bytes)", len(body))
	}
	synths := f.tts.synthCalls()
	if len(synths) != 1 {
		t.Fatalf("synth calls = %v", synths)
	}
	if synths[0].Voice != "alba" || synths[0].Rate != 1.25 || synths[0].Text != voicesample.Phrase {
		t.Fatalf("synth call = %+v", synths[0])
	}
	if got := f.settings.String(voicelib.KeyName); got != "jarvis" {
		t.Fatalf("preview changed voice.name to %q", got)
	}

	// Range support keeps the audio element's scrubbing working.
	rangeReq := httptest.NewRequest(http.MethodGet, "/voices/audio/alba", nil)
	rangeReq.Host = "127.0.0.1:8082"
	rangeReq.RemoteAddr = "127.0.0.1:12345"
	rangeReq.Header.Set("Tailscale-User-Login", owner)
	rangeReq.Header.Set("Range", "bytes=0-3")
	rangeRec := httptest.NewRecorder()
	f.handler.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent || rangeRec.Body.String() != "RIFF" {
		t.Fatalf("range status = %d body = %q", rangeRec.Code, rangeRec.Body.String())
	}
}

func TestVoicesPreviewReportsAServiceFailure(t *testing.T) {
	f := newVoicesFixture(t)
	f.tts.setSynthFailure(true)
	rec := f.get("/voices/audio/alba")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if synth := f.tts.synthCalls(); len(synth) != 1 {
		t.Fatalf("synth calls = %v", synth)
	}
}

func TestVoicesPageRequiresTheOwner(t *testing.T) {
	f := newVoicesFixture(t)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, "/voices", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func assertNoStagedFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read voices dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("staging or backup file left behind: %q", entry.Name())
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
