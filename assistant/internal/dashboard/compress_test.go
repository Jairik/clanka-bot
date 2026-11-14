package dashboard_test

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

// The asset is large enough that gzip and brotli both win; the .br bytes are a
// real stream of this exact body, produced with `brotli -q 11`.
var plainAsset = strings.Repeat("console.log('compressed asset');\n", 64)

const brotliAsset = "Hz8I+MVt7E1dzR8RncJhXYgqvRimYeClGXvgNVZDlG6Pafe3zx4="

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var b strings.Builder
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return []byte(b.String())
}

func compressedFixture(t *testing.T) fstest.MapFS {
	t.Helper()
	br, err := base64.StdEncoding.DecodeString(brotliAsset)
	if err != nil {
		t.Fatalf("brotli fixture: %v", err)
	}
	return fstest.MapFS{
		"index.html":        {Data: []byte(uiIndex)},
		"index.html.br":     {Data: []byte("br-index")},
		"assets/app.js":     {Data: []byte(plainAsset)},
		"assets/app.js.gz":  {Data: gzipBytes(t, []byte(plainAsset))},
		"assets/app.js.br":  {Data: br},
		"assets/logo.woff2": {Data: []byte{0x77, 0x4f, 0x46, 0x32, 0x00, 0x01}},
		"assets/plain.png":  {Data: []byte{0x89, 0x50, 0x4e, 0x47}},
	}
}

func assetRequest(t *testing.T, handler http.Handler, target, accept string) *http.Response {
	t.Helper()
	req := request(http.MethodGet, target, owner)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	rec := serve(handler, req)
	return rec.Result()
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func TestAssetWithoutAcceptEncodingIsByteIdentical(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: compressedFixture(t)})
	resp := assetRequest(t, handler, "/assets/app.js", "")
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none", got)
	}
	if body := string(readBody(t, resp)); body != plainAsset {
		t.Fatalf("body differs from the built file (%d bytes)", len(body))
	}
	if got := resp.Header.Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
}

func TestAssetPrefersThePrecompressedSibling(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: compressedFixture(t)})
	plain := len(plainAsset)

	gz := assetRequest(t, handler, "/assets/app.js", "gzip")
	if got := gz.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q", got)
	}
	gzBody := readBody(t, gz)
	if len(gzBody) >= plain {
		t.Errorf("gzip body = %d bytes, want smaller than %d", len(gzBody), plain)
	}
	if gz.ContentLength != int64(len(gzBody)) {
		t.Errorf("Content-Length = %d, want %d", gz.ContentLength, len(gzBody))
	}
	zr, err := gzip.NewReader(strings.NewReader(string(gzBody)))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(decoded) != plainAsset {
		t.Error("the gzip body does not decode to the built file")
	}

	br := assetRequest(t, handler, "/assets/app.js", "gzip, br")
	if got := br.Header.Get("Content-Encoding"); got != "br" {
		t.Fatalf("br Content-Encoding = %q", got)
	}
	brBody := readBody(t, br)
	if len(brBody) >= len(gzBody) {
		t.Errorf("brotli body = %d bytes, want smaller than gzip's %d", len(brBody), len(gzBody))
	}
	if br.ContentLength != int64(len(brBody)) {
		t.Errorf("br Content-Length = %d, want %d", br.ContentLength, len(brBody))
	}
	if got := br.Header.Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Errorf("br Cache-Control = %q, want the immutable asset cache", got)
	}
}

func TestAssetFallsBackWhenTheEncodingIsRefused(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: compressedFixture(t)})
	resp := assetRequest(t, handler, "/assets/app.js", "gzip;q=0, br;q=0")
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none for q=0", got)
	}
	if body := string(readBody(t, resp)); body != plainAsset {
		t.Error("q=0 did not fall back to the plain file")
	}
}

func TestAlreadyCompressedAssetsAreNeverRecompressed(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: compressedFixture(t)})
	for _, target := range []string{"/assets/logo.woff2", "/assets/plain.png"} {
		resp := assetRequest(t, handler, target, "gzip, br")
		if got := resp.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("%s Content-Encoding = %q, want none", target, got)
		}
		_ = readBody(t, resp)
	}
}

func TestDocumentPrefersBrotli(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: compressedFixture(t)})
	resp := assetRequest(t, handler, "/settings", "gzip, br")
	if got := resp.Header.Get("Content-Encoding"); got != "br" {
		t.Fatalf("document Content-Encoding = %q, want br", got)
	}
	if body := string(readBody(t, resp)); body != "br-index" {
		t.Errorf("body = %q, want the precompressed document", body)
	}
	if resp.ContentLength != int64(len("br-index")) {
		t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len("br-index"))
	}
}

func TestDocumentIsGzipped(t *testing.T) {
	// The default fixture has no siblings, so this is the dynamic path.
	handler := newHandler(t, dashboard.Options{})
	resp := assetRequest(t, handler, "/activity", "gzip")
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("document Content-Encoding = %q", got)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(body) != uiIndex {
		t.Error("the gzipped document does not decode to index.html")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self'") || strings.Contains(csp, "accounts.spotify.com") {
		t.Errorf("the document lost its CSP: %q", csp)
	}
	if resp.ContentLength != -1 {
		t.Errorf("Content-Length = %d, want a chunked gzipped body", resp.ContentLength)
	}
}

func TestJSONViewsAreGzipped(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	req := request(http.MethodGet, "/api/ui/health", owner)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := serve(handler, req)
	resp := rec.Result()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("json Content-Encoding = %q", got)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	var view map[string]any
	if err := json.NewDecoder(zr).Decode(&view); err != nil {
		t.Fatalf("decode gzipped json: %v", err)
	}
	if view["Owner"] != owner {
		t.Errorf("Owner = %v, want %q", view["Owner"], owner)
	}
}

func TestHeadAndRangeRequestsAreNotCompressed(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: compressedFixture(t)})

	head := request(http.MethodHead, "/api/ui/health", owner)
	head.Header.Set("Accept-Encoding", "gzip")
	if got := serve(handler, head).Header().Get("Content-Encoding"); got != "" {
		t.Errorf("HEAD Content-Encoding = %q, want none", got)
	}

	req := request(http.MethodGet, "/assets/app.js", owner)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-9")
	rec := serve(handler, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Range Content-Encoding = %q, want the plain bytes", got)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206", rec.Code)
	}
}
