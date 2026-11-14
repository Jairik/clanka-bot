package dashboard_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const micContentType = "audio/L16; rate=16000; channels=1; endianness=little-endian"

// 16 kHz mono 16-bit is 32000 bytes per second.
const (
	micBytesPerSecond = 32000
	micMinBytes       = micBytesPerSecond / 4
	micMaxBytes       = micBytesPerSecond * 30
)

func activityStoreForTest() *activity.Store { return activity.New(activity.DefaultLimit) }

// micTranscriber is a scripted speech-to-text service.
type micTranscriber struct {
	mu    sync.Mutex
	text  string
	err   error
	calls [][]byte
}

func (m *micTranscriber) Transcribe(_ context.Context, pcm []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, append([]byte(nil), pcm...))
	return m.text, m.err
}

func (m *micTranscriber) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// micTyped is a TypedResponder that records the transcripts it was asked and
// feeds the request timeline the way reply.Session does, so the Activity entry
// carries the transcript and reply.
type micTyped struct {
	mu    sync.Mutex
	asked []string
}

func (m *micTyped) Ask(ctx context.Context, _ tool.Request, transcript string) (pipeline.Reply, error) {
	m.mu.Lock()
	m.asked = append(m.asked, transcript)
	m.mu.Unlock()
	latency.SetRoute(ctx, "fast_path", "time.now", transcript)
	latency.SetSummary(ctx, "It's 3:04 PM.", false, false)
	return pipeline.Reply{Text: "It's 3:04 PM."}, nil
}

func (m *micTyped) questions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.asked...)
}

type micFixture struct {
	handler     *dashboard.Handler
	store       *activity.Store
	typed       *micTyped
	transcriber *micTranscriber
}

func newMicFixture(t *testing.T) *micFixture {
	t.Helper()
	f := &micFixture{
		store:       activityStoreForTest(),
		typed:       &micTyped{},
		transcriber: &micTranscriber{text: "what time is it"},
	}
	f.handler = newHandler(t, dashboard.Options{Activity: f.store, Typed: f.typed, Transcriber: f.transcriber})
	return f
}

func micPost(handler http.Handler, contentType string, body []byte, edit func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/ui/ask/audio", bytes.NewReader(body))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if edit != nil {
		edit(req)
	}
	return serve(handler, req)
}

func TestMicTranscribesAndAnswersThroughThePipeline(t *testing.T) {
	f := newMicFixture(t)
	pcm := make([]byte, micBytesPerSecond) // one second
	rec := micPost(f.handler, micContentType, pcm, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.typed.questions(); len(got) != 1 || got[0] != "what time is it" {
		t.Fatalf("pipeline asked %v, want the transcript", got)
	}
	if calls := f.transcriber.calls; len(calls) != 1 || len(calls[0]) != len(pcm) {
		t.Fatalf("transcriber saw %d calls, want the whole recording once", len(calls))
	}

	var v activityResp
	decodeJSON(t, rec, &v)
	if v.Error != "" || v.Just == nil {
		t.Fatalf("view = error %q just %+v", v.Error, v.Just)
	}
	if v.Just.Source != "dashboard_mic" || v.Just.SourceLabel != "Dashboard mic" {
		t.Errorf("source = %q (%q), want the Dashboard mic", v.Just.Source, v.Just.SourceLabel)
	}
	if v.Just.Transcript != "what time is it" || v.Just.Reply != "It's 3:04 PM." {
		t.Errorf("transcript/reply = %q / %q", v.Just.Transcript, v.Just.Reply)
	}
	if !containsString(stageLabels(v.Just), "STT finalize") {
		t.Errorf("stages = %v, want an stt_finalize stage", stageLabels(v.Just))
	}
	var names []string
	for _, stage := range v.Just.Stages {
		names = append(names, stage.Name)
	}
	if !containsString(names, "stt_finalize") {
		t.Errorf("stage names = %v, want stt_finalize", names)
	}
	if v.Just.Measured {
		t.Error("a mic request invented a Response latency")
	}
	entry, ok := f.store.Get(v.Just.ID)
	if !ok || entry.Source != "dashboard_mic" {
		t.Fatalf("stored entry = %+v, ok=%v", entry, ok)
	}
}

func TestMicTrimsAnOverlongTranscript(t *testing.T) {
	f := newMicFixture(t)
	f.transcriber.text = "  " + string(bytes.Repeat([]byte("x"), 2500)) + "  "
	rec := micPost(f.handler, micContentType, make([]byte, micBytesPerSecond), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := f.typed.questions(); len(got) != 1 || len([]rune(got[0])) != 2000 {
		t.Fatalf("pipeline asked a transcript of %d runes, want it capped at 2000", len([]rune(got[0])))
	}
}

func TestMicRejectsTheWrongContentType(t *testing.T) {
	for _, contentType := range []string{
		"",
		"audio/webm",
		"audio/L16; rate=48000; channels=1; endianness=little-endian",
		"audio/L16; rate=16000; channels=2; endianness=little-endian",
		"audio/L16; rate=16000; channels=1",
		"audio/L16; rate=16000; channels=1; endianness=big-endian",
	} {
		t.Run(contentType, func(t *testing.T) {
			f := newMicFixture(t)
			rec := micPost(f.handler, contentType, make([]byte, micBytesPerSecond), nil)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415", rec.Code)
			}
			assertMicRejected(t, f, rec, "16 kHz mono")
		})
	}
}

func TestMicRejectsATooShortOrOddBody(t *testing.T) {
	for name, size := range map[string]int{
		"empty":            0,
		"under a quarter":  micMinBytes - 2,
		"odd sample count": micBytesPerSecond + 1,
	} {
		t.Run(name, func(t *testing.T) {
			f := newMicFixture(t)
			rec := micPost(f.handler, micContentType, make([]byte, size), nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			assertMicRejected(t, f, rec, "too short")
		})
	}
}

func TestMicRejectsARecordingOverThirtySeconds(t *testing.T) {
	f := newMicFixture(t)
	rec := micPost(f.handler, micContentType, make([]byte, micMaxBytes+2), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	assertMicRejected(t, f, rec, "30 seconds")

	// Exactly thirty seconds is allowed.
	f = newMicFixture(t)
	if rec := micPost(f.handler, micContentType, make([]byte, micMaxBytes), nil); rec.Code != http.StatusOK {
		t.Fatalf("a 30 s recording status = %d, want 200", rec.Code)
	}
}

func TestMicEmptyTranscriptIsUnprocessable(t *testing.T) {
	f := newMicFixture(t)
	f.transcriber.text = "   "
	rec := micPost(f.handler, micContentType, make([]byte, micBytesPerSecond), nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if f.transcriber.callCount() != 1 {
		t.Error("the recording never reached the transcriber")
	}
	var v activityResp
	decodeJSON(t, rec, &v)
	if v.Error == "" || len(f.typed.questions()) != 0 || f.store.Len() != 0 {
		t.Errorf("an empty transcript reached the pipeline: error %q, asked %v, entries %d", v.Error, f.typed.questions(), f.store.Len())
	}
}

func TestMicTranscriberFailureIsABadGatewayWithoutLeakingTheCause(t *testing.T) {
	f := newMicFixture(t)
	f.transcriber.err = errors.New("dial tcp 10.0.0.5:10300: connection refused")
	rec := micPost(f.handler, micContentType, make([]byte, micBytesPerSecond), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var v activityResp
	decodeJSON(t, rec, &v)
	if v.Error == "" {
		t.Error("502 carried no message")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("10.0.0.5")) {
		t.Error("the STT error text leaked to the client")
	}
	if len(f.typed.questions()) != 0 {
		t.Error("a failed transcription reached the pipeline")
	}
}

func TestMicWithoutATranscriberOrTypedResponderIsNotFound(t *testing.T) {
	for name, opts := range map[string]dashboard.Options{
		"no transcriber": {Activity: activityStoreForTest(), Typed: &micTyped{}},
		"no typed":       {Activity: activityStoreForTest(), Transcriber: &micTranscriber{text: "hi"}},
	} {
		t.Run(name, func(t *testing.T) {
			handler := newHandler(t, opts)
			rec := micPost(handler, micContentType, make([]byte, micBytesPerSecond), nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestMicRequiresSameOrigin(t *testing.T) {
	for name, edit := range map[string]func(*http.Request){
		"no origin": func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"cross site": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Origin", "https://evil.example")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMicFixture(t)
			rec := micPost(f.handler, micContentType, make([]byte, micBytesPerSecond), edit)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if f.transcriber.callCount() != 0 || len(f.typed.questions()) != 0 {
				t.Error("a forbidden recording was transcribed")
			}
		})
	}
	// No identity is rejected before anything else.
	f := newMicFixture(t)
	rec := micPost(f.handler, micContentType, make([]byte, micBytesPerSecond), func(r *http.Request) {
		r.Header.Del("Tailscale-User-Login")
	})
	if rec.Code != http.StatusForbidden || f.transcriber.callCount() != 0 {
		t.Fatalf("unauthenticated status = %d, transcribed = %d", rec.Code, f.transcriber.callCount())
	}
}

// assertMicRejected checks a refused recording is the Activity view with an
// Error, and that nothing was transcribed or asked.
func assertMicRejected(t *testing.T, f *micFixture, rec *httptest.ResponseRecorder, wantInError string) {
	t.Helper()
	var v activityResp
	decodeJSON(t, rec, &v)
	if !bytes.Contains([]byte(v.Error), []byte(wantInError)) {
		t.Errorf("error = %q, want it to mention %q", v.Error, wantInError)
	}
	if f.transcriber.callCount() != 0 || len(f.typed.questions()) != 0 || f.store.Len() != 0 {
		t.Errorf("a refused recording had effects: transcribed %d, asked %v, entries %d",
			f.transcriber.callCount(), f.typed.questions(), f.store.Len())
	}
}
