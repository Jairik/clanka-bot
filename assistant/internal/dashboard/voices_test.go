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

// voicesResp decodes the Voices view (encoding/json default field names).
type voicesResp struct {
	Flash, Error                                     string
	HasLibrary, CanUpload, CanPreview                bool
	MaxUpload, Revision, ActiveName, Default         string
	Rate, RateSource, RateLabel, RateLower, RateEdit string
	HasRate                                          bool
	Voices                                           []voiceRow
}

type voiceRow struct {
	Name, Origin, Status, Revision, Note, LastError, StaleNotice string
	Builtin, Ready, Stale, Active, Unvalidated                   bool
	PreviewURL, SelectAction, NoteAction                         string
}

func (v voicesResp) voice(t *testing.T, name string) voiceRow {
	t.Helper()
	for _, row := range v.Voices {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("view has no voice %q: %+v", name, v.Voices)
	return voiceRow{}
}

// view reads /api/ui/voices, which must be a 200.
func (f *voicesFixture) view() (voicesResp, string) {
	f.t.Helper()
	var v voicesResp
	rec := getJSON(f.t, f.handler, "/api/ui/voices", &v)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET voices = %d: %s", rec.Code, rec.Body.String())
	}
	return v, rec.Body.String()
}

func decodeVoices(t *testing.T, rec *httptest.ResponseRecorder) voicesResp {
	t.Helper()
	var v voicesResp
	decodeJSON(t, rec, &v)
	return v
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

func TestVoicesReadListsTheLibraryAndSettings(t *testing.T) {
	f := newVoicesFixture(t)
	v, _ := f.view()
	if !v.HasLibrary || !v.CanUpload || !v.CanPreview {
		t.Errorf("view flags = library %v upload %v preview %v", v.HasLibrary, v.CanUpload, v.CanPreview)
	}
	if v.MaxUpload != "32 MB" || v.Revision != "test-revision-1" {
		t.Errorf("max upload %q revision %q", v.MaxUpload, v.Revision)
	}
	alba := v.voice(t, "alba")
	if !alba.Builtin || alba.Origin != "Built-in" || !alba.Ready || alba.PreviewURL != "/voices/audio/alba" {
		t.Errorf("alba = %+v, want a ready built-in with a preview URL", alba)
	}
	if !v.HasRate || v.Rate != "1" || v.RateEdit != "1" {
		t.Errorf("rate = %q edit %q has=%v, want the effective 1", v.Rate, v.RateEdit, v.HasRate)
	}
	if v.ActiveName != "alba" || v.Default != "alba" {
		t.Errorf("active %q default %q", v.ActiveName, v.Default)
	}
}

func TestVoicesUploadRejectsBadFilesBeforeTTS(t *testing.T) {
	t.Run("wrong extension", func(t *testing.T) {
		f := newVoicesFixture(t)
		rec := f.upload("jarvis.pt", stateBytes(t, 2, true), false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if v := decodeVoices(t, rec); !strings.Contains(v.Error, "Only .safetensors") {
			t.Fatalf("error = %q", v.Error)
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
		if v := decodeVoices(t, rec); !strings.Contains(v.Error, "not a valid Pocket TTS voice state") {
			t.Fatalf("error = %q", v.Error)
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
		if v := decodeVoices(t, rec); !strings.Contains(v.Error, "32 MB") {
			t.Fatalf("error = %q", v.Error)
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
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	uploaded := decodeVoices(t, rec)
	if uploaded.Flash == "" || uploaded.Error != "" {
		t.Fatalf("upload view = flash %q error %q", uploaded.Flash, uploaded.Error)
	}
	if row := uploaded.voice(t, "Jarvis"); !row.Ready {
		t.Fatalf("the upload response does not list the ready voice: %+v", row)
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

	v, _ := f.view()
	row := v.voice(t, "Jarvis")
	if row.Status != "Ready" || !row.Ready || row.Revision != "test-revision-1" || row.Origin != "Custom" ||
		row.PreviewURL != "/voices/audio/Jarvis" || row.SelectAction != "/voices/active/Jarvis" || row.NoteAction != "/voices/note/Jarvis" {
		t.Errorf("uploaded row = %+v", row)
	}

	rec = f.post("/voices/active/Jarvis", url.Values{})
	if rec.Code != http.StatusOK {
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
	v, _ = f.view()
	if v.ActiveName != "Jarvis" || !v.voice(t, "Jarvis").Active || v.voice(t, "alba").Active {
		t.Errorf("view does not mark Jarvis as the active voice: active %q", v.ActiveName)
	}
}

func TestVoicesNamesWithSpacesRoundTripThroughURLs(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("My Voice.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	v, _ := f.view()
	row := v.voice(t, "My Voice")
	for got, want := range map[string]string{
		row.PreviewURL:   "/voices/audio/My%20Voice",
		row.SelectAction: "/voices/active/My%20Voice",
		row.NoteAction:   "/voices/note/My%20Voice",
	} {
		if got != want {
			t.Errorf("URL = %q, want %q", got, want)
		}
	}
	if rec := f.post("/voices/active/My%20Voice", url.Values{}); rec.Code != http.StatusOK {
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
	if rec := f.upload("jarvis.safetensors", first, false); rec.Code != http.StatusOK {
		t.Fatalf("first upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := f.upload("jarvis.safetensors", stateBytes(t, 3, true), false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v := decodeVoices(t, rec); !strings.Contains(v.Error, "already exists") {
		t.Fatalf("duplicate error = %q", v.Error)
	}
	if before, after := first, mustRead(t, loadVoicePath(t, f, "jarvis")); !bytes.Equal(before, after) {
		t.Fatal("a refused duplicate changed the file")
	}

	rec = f.upload("jarvis.safetensors", stateBytes(t, 4, false), true)
	if rec.Code != http.StatusOK {
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
	if v := decodeVoices(t, rec); !strings.Contains(v.Error, "Nothing was published") {
		t.Fatalf("error = %q", v.Error)
	}
	if _, ok := f.voices.Get("jarvis"); ok {
		t.Fatal("failed upload entered the index")
	}
	assertNoStagedFiles(t, filepath.Join(f.dir, "voices"))
}

func TestVoicesRateSavesRejectsAndResets(t *testing.T) {
	f := newVoicesFixture(t)
	rec := f.post("/voices/rate", url.Values{"value": {"1.25"}})
	if rec.Code != http.StatusOK {
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
		v := decodeVoices(t, rec)
		if !strings.Contains(v.Error, "Rate must be a number") {
			t.Fatalf("rate %q error = %q", value, v.Error)
		}
		if v.RateEdit != value || v.Rate != "1.25" {
			t.Errorf("rate %q: edit %q effective %q, want the input preserved and the setting unchanged", value, v.RateEdit, v.Rate)
		}
		if got := f.settings.Float(voicelib.KeyRate); got != 1.25 {
			t.Fatalf("rejected rate %q changed the setting to %v", value, got)
		}
	}

	rec = f.post("/voices/rate/reset", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.settings.Float(voicelib.KeyRate); got != 1.0 {
		t.Fatalf("reset rate = %v, want 1.0", got)
	}
	if v, _ := f.view(); v.Rate != "1" || v.RateEdit != "1" || v.RateSource == "override" {
		t.Errorf("rate form after reset = %q / %q (%s), want the effective 1", v.Rate, v.RateEdit, v.RateSource)
	}
}

func TestVoicesNoteRoundTripsInertlyAndSurvivesRescan(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	note := `<b>cloned from a friend's clip</b> & recorded with permission <script>x</script>`
	rec := f.post("/voices/note/jarvis", url.Values{"note": {note}})
	if rec.Code != http.StatusOK {
		t.Fatalf("note status = %d, body = %s", rec.Code, rec.Body.String())
	}
	voice, _ := f.voices.Get("jarvis")
	if voice.Note != note {
		t.Fatalf("stored note = %q, want verbatim %q", voice.Note, note)
	}
	v, body := f.view()
	if got := v.voice(t, "jarvis").Note; got != note {
		t.Fatalf("view note = %q, want it unchanged", got)
	}
	noRawScript(t, body)
	if strings.Contains(body, "<b>") {
		t.Fatal("note is a raw tag in the body")
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
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusOK {
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
	v, _ := f.view()
	stale := v.voice(t, "jarvis")
	if stale.Status != "Stale" || !stale.Stale || stale.Ready {
		t.Errorf("stale row = %+v", stale)
	}
	for _, want := range []string{"Re-export it on the desktop", "cannot be selected", "test-revision-2"} {
		if !strings.Contains(stale.StaleNotice, want) {
			t.Errorf("stale notice lacks %q: %q", want, stale.StaleNotice)
		}
	}
	// The row still carries its preview URL, so the app must gate the player
	// on Ready; the server refuses it below either way.
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
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.post("/voices/rate", url.Values{"value": {"1.25"}}); rec.Code != http.StatusOK {
		t.Fatalf("rate = %d", rec.Code)
	}
	if rec := f.post("/voices/active/jarvis", url.Values{}); rec.Code != http.StatusOK {
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

func TestVoicesReadRequiresTheOwner(t *testing.T) {
	f := newVoicesFixture(t)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request(http.MethodGet, "/api/ui/voices", ""))
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

// TestVoicesWritesAnswerJSONWithTheRealStatus pins the write contract the
// React app relies on: success is 200 with the refreshed view and a Flash, a
// rejection is the view with the real 4xx status and an Error, never a
// redirect.
func TestVoicesWritesAnswerJSONWithTheRealStatus(t *testing.T) {
	f := newVoicesFixture(t)
	rec := f.post("/voices/rate", url.Values{"value": {"1.1"}})
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("rate status = %d", rec.Code)
	}
	if v := decodeVoices(t, rec); v.Flash == "" || v.Error != "" || v.Rate != "1.1" {
		t.Fatalf("rate view = flash %q error %q rate %q", v.Flash, v.Error, v.Rate)
	}
	rec = f.post("/voices/active/nope", url.Values{})
	if rec.Code == http.StatusOK || rec.Code == http.StatusSeeOther {
		t.Fatalf("unknown voice status = %d, want an error", rec.Code)
	}
	if v := decodeVoices(t, rec); v.Error == "" {
		t.Errorf("unknown voice view has no Error: %+v", v)
	}
}
