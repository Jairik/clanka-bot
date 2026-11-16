package dashboard_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
	"log/slog"
)

// The recorder is only invoked by authenticated same-origin writes and returns no stored take.
type sampleRecorder struct{ calls int }

func (s *sampleRecorder) RecordSample(context.Context) ([]byte, error) {
	s.calls++
	return bytes.Repeat([]byte{7}, 96000), nil
}

// errSampleRecorder returns a fixed error (or success) for each capture.
type errSampleRecorder struct{ err error }

func (s *errSampleRecorder) RecordSample(context.Context) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return bytes.Repeat([]byte{7}, 96000), nil
}

// TestPiSampleCaptureMapsEachRecorderFailure asserts the Dashboard reports each
// recorder failure class with a distinct, honest status and sentence, and keeps
// the success body as raw PCM.
func TestPiSampleCaptureMapsEachRecorderFailure(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{
			name:     "busy",
			err:      voice.ErrMicBusy,
			wantCode: http.StatusConflict,
			wantBody: "Microphone is busy. Wait for the current turn or take to finish.",
		},
		{
			name:     "overflow",
			err:      voice.ErrSampleOverflow,
			wantCode: http.StatusConflict,
			wantBody: "That take overflowed. Redo it.",
		},
		{
			name:     "deadline",
			err:      context.DeadlineExceeded,
			wantCode: http.StatusGatewayTimeout,
			wantBody: "The microphone did not deliver audio in time.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &errSampleRecorder{err: tc.err}
			h := newHandler(t, dashboard.Options{SampleRecorder: rec})
			r := request(http.MethodPost, "/voice-samples/capture", owner)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			resp := serve(h, r)
			if resp.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.Code, tc.wantCode)
			}
			if got := strings.TrimRight(resp.Body.String(), "\n"); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// TestPiSampleCaptureCancelledRequestWritesNoBody proves a departed browser
// (a cancelled request context) is answered with no body rather than a 409.
func TestPiSampleCaptureCancelledRequestWritesNoBody(t *testing.T) {
	rec := &errSampleRecorder{err: errors.New("ignored")}
	h := newHandler(t, dashboard.Options{SampleRecorder: rec})
	r := request(http.MethodPost, "/voice-samples/capture", owner)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	resp := serve(h, r)
	if resp.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty (client departed)", resp.Body.String())
	}
}

// TestPiSampleCaptureUnknownErrorLogsDetail asserts an unexpected failure is a
// 500 with a fixed sentence and the surprising detail only in the log.
func TestPiSampleCaptureUnknownErrorLogsDetail(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	rec := &errSampleRecorder{err: errors.New("player fuse blew")}
	h := newHandler(t, dashboard.Options{SampleRecorder: rec, Logger: logger})
	r := request(http.MethodPost, "/voice-samples/capture", owner)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	resp := serve(h, r)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
	const want = "The microphone could not be recorded. Check the Pi and try again."
	if got := strings.TrimRight(resp.Body.String(), "\n"); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if !strings.Contains(logs.String(), "player fuse blew") {
		t.Fatalf("log missing detail: %s", logs.String())
	}
}

func TestPiSampleCapture(t *testing.T) {
	recorder := &sampleRecorder{}
	h := newHandler(t, dashboard.Options{SampleRecorder: recorder})
	r := request(http.MethodPost, "/voice-samples/capture", owner)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	response := serve(h, r)
	if response.Code != http.StatusOK || response.Body.Len() != 96000 || recorder.calls != 1 {
		t.Fatalf("capture: status=%d bytes=%d calls=%d", response.Code, response.Body.Len(), recorder.calls)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private audio can be cached")
	}
	r = request(http.MethodPost, "/voice-samples/capture", "stranger@example.com")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := serve(h, r); got.Code == http.StatusOK || recorder.calls != 1 {
		t.Fatal("unauthorized recording allowed")
	}
	r = request(http.MethodPost, "/voice-samples/capture", owner)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := serve(h, r); got.Code == http.StatusOK || recorder.calls != 1 {
		t.Fatal("cross-site recording allowed")
	}
}
