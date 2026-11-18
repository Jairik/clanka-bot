package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A failing upstream should delay only the first navigation in a burst.
func TestUIArtifactFailureCoolsDown(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	artifact := newTestUIArtifact(t, server.URL)
	for range 2 {
		if artifact.refresh(context.Background()) != nil {
			t.Fatal("a 503 answered as if the artifact served the file")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream got %d requests, want 1", got)
	}
}

// A deploy that changes the upstream mid-download must not publish a mixed build.
func TestUIArtifactRejectsADigestMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui-bundle.sha256" {
			_, _ = io.WriteString(w, strings.Repeat("0", 64))
			return
		}
		_, _ = io.WriteString(w, "new build")
	}))
	t.Cleanup(server.Close)

	artifact := newTestUIArtifact(t, server.URL)
	if artifact.refresh(context.Background()) != nil || artifact.find("theme.js", "") != nil {
		t.Fatal("published a bundle whose digest does not match")
	}
}

func newTestUIArtifact(t *testing.T, url string) *uiArtifact {
	t.Helper()
	artifact, err := newUIArtifact(url, "pin-sha", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newUIArtifact: %v", err)
	}
	return artifact
}
