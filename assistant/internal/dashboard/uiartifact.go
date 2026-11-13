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
)

// uiArtifact is the internal client for the standalone Dashboard build.
type uiArtifact struct {
	base    *url.URL
	version string
	client  *http.Client
	logger  *slog.Logger

	mu      sync.Mutex
	healthy bool
	known   bool
}

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
		version: version,
		logger:  logger,
		client:  &http.Client{Timeout: uiArtifactTimeout},
	}, nil
}

// fetch reads one file, preferring a build-time .br/.gz sibling the client
// accepts. ok=false means the caller must serve the embedded copy.
func (artifact *uiArtifact) fetch(ctx context.Context, name, accept string) ([]byte, string, bool) {
	if artifact == nil {
		return nil, "", false
	}
	for _, candidate := range encodingCandidates(name, accept) {
		if data, ok := artifact.get(ctx, name+candidate.suffix); ok {
			artifact.recovered()
			return data, candidate.coding, true
		}
	}
	data, ok := artifact.get(ctx, name)
	if !ok {
		artifact.failed()
		return nil, "", false
	}
	artifact.recovered()
	return data, "", true
}

func (artifact *uiArtifact) get(ctx context.Context, name string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, uiArtifactTimeout)
	defer cancel()
	target := *artifact.base
	target.Path = path.Join(artifact.base.Path, name)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, false
	}
	request.Header.Set("Accept", "*/*")
	response, err := artifact.client.Do(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// A stopped or still-starting container, or an asset the new build no
		// longer has. Either way the embedded bundle is the compatibility copy.
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUIArtifact+1))
	if err != nil || len(data) > maxUIArtifact {
		return nil, false
	}
	return data, true
}

// failed and recovered log only the transition, so a stopped artifact does not
// write one warning per asset request.
func (artifact *uiArtifact) failed() {
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	if !artifact.known || artifact.healthy {
		artifact.logger.Warn("dashboard ui artifact unavailable", "fallback", "embedded bundle", "url", artifact.base.String())
	}
	artifact.known = true
	artifact.healthy = false
}

func (artifact *uiArtifact) recovered() {
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	if !artifact.known || !artifact.healthy {
		artifact.logger.Info("dashboard ui artifact serving", "version", artifact.version, "url", artifact.base.String())
	}
	artifact.known = true
	artifact.healthy = true
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
