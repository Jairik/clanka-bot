// The standalone Dashboard web artifact (#105). The UI is built and shipped as
// its own SHA-tagged Compose service, so a page fix does not rebuild the Go
// binary or restart the assistant. The browser still talks only to the
// assistant: this file fetches the document and assets from the internal
// service on the same origin, and every failure falls back to the bundle
// embedded in the binary. The identity and Host checks stay here, on the server
// that owns them.
package dashboard

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// uiArtifactTimeout bounds one artifact fetch. The Pi serves it over the
	// Compose network, so a wedged container fails fast and the fallback runs.
	uiArtifactTimeout = 2 * time.Second
	// maxUIArtifact bounds one file, so a misconfigured upstream cannot stream
	// an unbounded body into the Dashboard.
	maxUIArtifact = 8 << 20
	// uiArtifactCooldown is how long a failed artifact is skipped before one
	// request probes it again. A stopped container refuses instantly, but a
	// wedged one would otherwise cost one timeout per sibling per asset on
	// every page load.
	uiArtifactCooldown = 30 * time.Second
)

// uiArtifact is the internal client for the standalone Dashboard build.
type uiArtifact struct {
	base    *url.URL
	pin     string
	client  *http.Client
	logger  *slog.Logger

	mu      sync.Mutex
	healthy bool
	known   bool
	// downUntil skips the artifact for a cooldown after a transport or server
	// failure; a missing file does not set it.
	downUntil time.Time
	// version is the build the artifact reports in X-Dashboard-UI-Version, so
	// a --no-deps rollback of the artifact is visible without restarting the
	// assistant. pin is the fallback until the artifact answers.
	version string
}

// fetchOutcome separates "the artifact is down" from "the artifact does not
// have this file": the first arms the cooldown, the second must not.
type fetchOutcome int

const (
	fetchOK fetchOutcome = iota
	fetchMissing
	fetchDown
)

// newUIArtifact parses the configured base URL. An empty value disables the
// artifact and leaves the embedded bundle as the only copy.
func newUIArtifact(raw, version string, logger *slog.Logger) (*uiArtifact, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("dashboard: dashboard.ui_url %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("dashboard: dashboard.ui_url must be http(s), got %q", raw)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("dashboard: dashboard.ui_url has no host: %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("dashboard: dashboard.ui_url must be a base URL without credentials, query or fragment")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return &uiArtifact{
		base:    parsed,
		pin:     version,
		logger:  logger,
		client:  &http.Client{Timeout: uiArtifactTimeout},
	}, nil
}

// reportedVersion names the copy that answered: the build the artifact reports
// when it has told us, the configured pin before that.
func (artifact *uiArtifact) reportedVersion() string {
	if artifact == nil {
		return ""
	}
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	if artifact.version != "" {
		return artifact.version
	}
	return artifact.pin
}

// fetch reads one file, preferring a build-time .br/.gz sibling the client
// accepts. ok=false means the caller must serve the embedded copy.
func (artifact *uiArtifact) fetch(ctx context.Context, name, accept string) ([]byte, string, bool) {
	if artifact == nil || artifact.cooling() {
		return nil, "", false
	}
	for _, candidate := range encodingCandidates(name, accept) {
		data, outcome := artifact.get(ctx, name+candidate.suffix)
		switch outcome {
		case fetchOK:
			artifact.recovered()
			return data, candidate.coding, true
		case fetchDown:
			artifact.failed()
			return nil, "", false
		}
		// A sibling the build did not emit is normal; try the next candidate.
	}
	data, outcome := artifact.get(ctx, name)
	switch outcome {
	case fetchOK:
		artifact.recovered()
		return data, "", true
	case fetchDown:
		artifact.failed()
	}
	// A missing file is not an outage: an old tab asks for an asset the new
	// build no longer has. Fall back without touching the artifact's health.
	return nil, "", false
}

func (artifact *uiArtifact) get(ctx context.Context, name string) ([]byte, fetchOutcome) {
	ctx, cancel := context.WithTimeout(ctx, uiArtifactTimeout)
	defer cancel()
	target := *artifact.base
	target.Path = path.Join(artifact.base.Path, name)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fetchDown
	}
	request.Header.Set("Accept", "*/*")
	response, err := artifact.client.Do(request)
	if err != nil {
		return nil, fetchDown
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, fetchMissing
	}
	if response.StatusCode != http.StatusOK {
		// A stopped or still-starting container, or a broken upstream.
		return nil, fetchDown
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUIArtifact+1))
	if err != nil || len(data) > maxUIArtifact {
		return nil, fetchDown
	}
	artifact.observeVersion(response.Header.Get("X-Dashboard-UI-Version"))
	return data, fetchOK
}

// observeVersion records the artifact's own build so a rollback of the artifact
// alone is reported truthfully, and warns when it disagrees with the pin.
func (artifact *uiArtifact) observeVersion(raw string) {
	version := strings.TrimSpace(raw)
	if version == "" {
		return
	}
	artifact.mu.Lock()
	changed := artifact.version != version
	artifact.version = version
	pin := artifact.pin
	artifact.mu.Unlock()
	if changed && pin != "" && version != pin {
		artifact.logger.Warn("dashboard ui artifact is not the configured build", "artifact", version, "pin", pin)
	}
}

// cooling reports whether a recent failure still suppresses artifact fetches.
func (artifact *uiArtifact) cooling() bool {
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	return time.Now().Before(artifact.downUntil)
}

// failed and recovered log only the transition, so a stopped artifact does not
// write one warning per asset request.
func (artifact *uiArtifact) failed() {
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	artifact.downUntil = time.Now().Add(uiArtifactCooldown)
	if !artifact.known || artifact.healthy {
		artifact.logger.Warn("dashboard ui artifact unavailable", "fallback", "embedded bundle", "url", artifact.base.String())
	}
	artifact.known = true
	artifact.healthy = false
}

func (artifact *uiArtifact) recovered() {
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	artifact.downUntil = time.Time{}
	if !artifact.known || !artifact.healthy {
		artifact.logger.Info("dashboard ui artifact serving", "version", artifact.reportedVersionLocked(), "url", artifact.base.String())
	}
	artifact.known = true
	artifact.healthy = true
}

// reportedVersionLocked is reportedVersion for a caller that holds the lock.
func (artifact *uiArtifact) reportedVersionLocked() string {
	if artifact.version != "" {
		return artifact.version
	}
	return artifact.pin
}

// encodingCandidate is one precompressed sibling and its Content-Encoding.
type encodingCandidate struct {
	suffix string
	coding string
}

// encodingCandidates lists the siblings the build emits, best first. It mirrors
// the embedded-bundle rule in precompressed: only text assets get siblings.
func encodingCandidates(name, accept string) []encodingCandidate {
	switch path.Ext(name) {
	case ".js", ".css", ".html", ".svg", ".json":
	default:
		return nil
	}
	var out []encodingCandidate
	if acceptsEncoding(accept, "br") {
		out = append(out, encodingCandidate{suffix: ".br", coding: "br"})
	}
	if acceptsEncoding(accept, "gzip") {
		out = append(out, encodingCandidate{suffix: ".gz", coding: "gzip"})
	}
	return out
}
