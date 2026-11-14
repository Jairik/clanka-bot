package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestUIArtifactFailureCoolsDown pins the failure path: a wedged or broken
// artifact costs one timeout, then the embedded bundle answers for a cooldown
// instead of one timeout per sibling per asset on every page load.
func TestUIArtifactFailureCoolsDown(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	artifact := newTestUIArtifact(t, server.URL)
	if _, _, ok := artifact.fetch(context.Background(), "index.html", "br"); ok {
		t.Fatal("a 503 answered as if the artifact served the file")
	}
	if !artifact.cooling() {
		t.Fatal("a failed artifact did not arm the cooldown")
	}
	before := hits.Load()
	if _, _, ok := artifact.fetch(context.Background(), "assets/app.js", "br"); ok {
		t.Fatal("the artifact answered during the cooldown")
	}
	if got := hits.Load(); got != before {
		t.Fatalf("cooldown made %d more request(s), want 0", got-before)
	}
}

// TestUIArtifactMissingFileKeepsItHealthy pins the other half: during a UI-only
// deploy an old tab asks for an asset the new build no longer has, and that is
// not an outage.
func TestUIArtifactMissingFileKeepsItHealthy(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	artifact := newTestUIArtifact(t, server.URL)
	if _, _, ok := artifact.fetch(context.Background(), "assets/old-hash.js", "br"); ok {
		t.Fatal("a 404 answered as if the artifact served the file")
	}
	if artifact.cooling() {
		t.Fatal("a missing file put the artifact into the failure cooldown")
	}
	artifact.mu.Lock()
	healthy, known := artifact.healthy, artifact.known
	artifact.mu.Unlock()
	if known {
		t.Fatalf("a missing file changed the artifact's health (known=%v healthy=%v)", known, healthy)
	}
}

// TestUIArtifactReportsTheBuildThatAnswered covers a rollback of the artifact
// alone: the assistant keeps running, and the header must name the old build
// the restarted artifact now serves.
func TestUIArtifactReportsTheBuildThatAnswered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/app.js" {
			w.Header().Set("X-Dashboard-UI-Version", "old-sha")
			_, _ = w.Write([]byte("js"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	artifact := newTestUIArtifact(t, server.URL)
	if artifact.reportedVersion() != "pin-sha" {
		t.Fatalf("before any fetch the pin is the only version: %q", artifact.reportedVersion())
	}
	if _, _, ok := artifact.fetch(context.Background(), "assets/app.js", ""); !ok {
		t.Fatal("the asset did not come from the artifact")
	}
	if got := artifact.reportedVersion(); got != "old-sha" {
		t.Fatalf("reportedVersion = %q, want the build the artifact reported", got)
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
