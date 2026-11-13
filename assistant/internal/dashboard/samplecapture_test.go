package dashboard_test

import (
	"bytes"
	"context"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"net/http"
	"testing"
)

// The recorder is only invoked by authenticated same-origin writes and returns no stored take.
type sampleRecorder struct{ calls int }

func (s *sampleRecorder) RecordSample(context.Context) ([]byte, error) {
	s.calls++
	return bytes.Repeat([]byte{7}, 96000), nil
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
