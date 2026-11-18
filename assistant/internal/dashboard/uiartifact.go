package dashboard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	uiArtifactTimeout  = 2 * time.Second
	uiArtifactCooldown = 30 * time.Second
	maxUIBundle        = 16 << 20
	retainedUIBuilds   = 3
	uiBuildQuery       = "ui_build"
)

// uiBundle is immutable after publication so concurrent asset requests can
// keep using it while a new build is fetched or the upstream is unavailable.
type uiBundle struct {
	digest  string
	version string
	index   []byte
	files   map[string][]byte
}

// uiArtifact refreshes the standalone build on navigation and retains recent
// bundles so an already-served document can finish loading during an outage.
type uiArtifact struct {
	base   *url.URL
	pin    string
	client *http.Client
	logger *slog.Logger

	refreshMu sync.Mutex
	mu        sync.RWMutex
	active    *uiBundle
	recent    []*uiBundle
	downUntil time.Time
}

// newUIArtifact leaves the embedded bundle as the only copy when the URL is empty.
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
		base:   parsed,
		pin:    version,
		logger: logger,
		client: &http.Client{Timeout: uiArtifactTimeout},
	}, nil
}

// reportedVersion is the last verified artifact build for diagnostics.
func (artifact *uiArtifact) reportedVersion() string {
	artifact.mu.RLock()
	defer artifact.mu.RUnlock()
	if len(artifact.recent) > 0 {
		return artifact.recent[0].version
	}
	return artifact.pin
}

// refresh confirms that the artifact is live before serving a standalone
// document. The small digest response avoids transferring an unchanged build.
func (artifact *uiArtifact) refresh(ctx context.Context) *uiBundle {
	artifact.refreshMu.Lock()
	defer artifact.refreshMu.Unlock()
	if ctx.Err() != nil || time.Now().Before(artifact.downUntil) {
		return nil
	}

	body, version, err := artifact.get(ctx, "ui-bundle.sha256", 128)
	digest := strings.TrimSpace(string(body))
	if err == nil {
		var decoded []byte
		decoded, err = hex.DecodeString(digest)
		if err == nil && len(decoded) != sha256.Size {
			err = fmt.Errorf("invalid bundle digest")
		}
	}
	if err == nil {
		bundle := artifact.find("", digest)
		if bundle == nil {
			var archive []byte
			archive, _, err = artifact.get(ctx, "ui-bundle.tar.gz", maxUIBundle)
			if err == nil && fmt.Sprintf("%x", sha256.Sum256(archive)) != digest {
				err = fmt.Errorf("bundle changed during download")
			}
			if err == nil {
				bundle, err = readUIBundle(digest, archive)
			}
		}
		if err == nil {
			if version == "" {
				version = artifact.pin
			}
			if bundle.version != version {
				copy := *bundle
				copy.version = version
				bundle = &copy
			}
			artifact.publish(bundle)
			return bundle
		}
	}
	// An abandoned navigation should not trip the circuit breaker.
	if ctx.Err() == nil {
		artifact.downUntil = time.Now().Add(uiArtifactCooldown)
		artifact.mu.Lock()
		artifact.active = nil
		artifact.mu.Unlock()
		artifact.logger.Warn("dashboard ui artifact unavailable", "url", artifact.base.String(), "error", err)
	}
	return nil
}

// publish keeps the active build first and retains a small window for open tabs.
func (artifact *uiArtifact) publish(bundle *uiBundle) {
	artifact.mu.Lock()
	defer artifact.mu.Unlock()
	if artifact.active == bundle {
		return
	}
	if artifact.active == nil || artifact.active.version != bundle.version {
		artifact.logger.Info("dashboard ui artifact serving", "version", bundle.version, "url", artifact.base.String())
		if artifact.pin != "" && bundle.version != artifact.pin {
			artifact.logger.Warn("dashboard ui artifact is not the configured build", "artifact", bundle.version, "pin", artifact.pin)
		}
	}
	recent := []*uiBundle{bundle}
	for _, older := range artifact.recent {
		if older.digest != bundle.digest && len(recent) < retainedUIBuilds {
			recent = append(recent, older)
		}
	}
	artifact.active = bundle
	artifact.recent = recent
}

// find uses the document's digest for mutable files and the file name for
// content-hashed Vite assets, including lazy imports from an older document.
func (artifact *uiArtifact) find(name, digest string) *uiBundle {
	artifact.mu.RLock()
	defer artifact.mu.RUnlock()
	switch {
	case digest != "":
		for _, bundle := range artifact.recent {
			if bundle.digest == digest {
				return bundle
			}
		}
	case strings.HasPrefix(name, "assets/"):
		for _, bundle := range artifact.recent {
			if _, ok := bundle.files[name]; ok {
				return bundle
			}
		}
	default:
		return artifact.active
	}
	return nil
}

// get bounds the upstream body before parsing it or adding it to the cache.
func (artifact *uiArtifact) get(ctx context.Context, name string, limit int64) ([]byte, string, error) {
	target := *artifact.base
	target.Path = path.Join(target.Path, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := artifact.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s returned HTTP %d", name, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("%s exceeds the bundle limit", name)
	}
	return data, strings.TrimSpace(res.Header.Get("X-Dashboard-UI-Version")), nil
}

// readUIBundle completes and bounds extraction before the index is published.
func readUIBundle(digest string, archive []byte) (*uiBundle, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	limited := &io.LimitedReader{R: compressed, N: maxUIBundle + 1}
	reader := tar.NewReader(limited)
	bundle := &uiBundle{digest: digest, files: make(map[string][]byte)}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimPrefix(header.Name, "./")
		if !fs.ValidPath(name) || header.Typeflag != tar.TypeReg || header.Size > limited.N || len(bundle.files) >= 1024 {
			return nil, fmt.Errorf("invalid or oversized bundle member %q", name)
		}
		if _, exists := bundle.files[name]; exists {
			return nil, fmt.Errorf("duplicate bundle member %q", name)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		bundle.files[name] = body
	}
	index := string(bundle.files["index.html"])
	if index == "" || limited.N <= 0 {
		return nil, fmt.Errorf("bundle is oversized or missing index.html")
	}

	// The document links to mutable root files as well as hashed Vite assets.
	// Bind those links to this bundle so a later navigation can use the fallback.
	for _, name := range []string{"theme.js", "favicon.svg", "manifest.webmanifest", "icons/apple-touch-icon.png"} {
		index = strings.ReplaceAll(index, "\"/"+name+"\"", "\"/"+name+"?"+uiBuildQuery+"="+digest+"\"")
	}
	bundle.index = []byte(index)
	return bundle, nil
}
