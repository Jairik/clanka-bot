package dashboard

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// app serves the embedded React build. Hashed files under /assets/ are served
// as-is; any other GET is a client-side route and gets index.html, so a reload
// on /alerts or a bookmark keeps working. Unknown assets are a real 404 rather
// than index.html, so a stale tab fails loudly instead of parsing HTML as JS.
func (h *Handler) app(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if isAssetPath(name) {
		h.appFile(w, r, name)
		return
	}
	if strings.HasPrefix(name, "api/") {
		http.NotFound(w, r)
		return
	}
	// The document is static too: serve its build-time sibling when one was
	// emitted, and let the central gzip handle it otherwise.
	serveName, coding := "index.html", ""
	if r.Header.Get("Range") == "" {
		if name, enc := precompressed(h.ui, "index.html", r.Header.Get("Accept-Encoding")); name != "" {
			serveName, coding = name, enc
		}
	}
	index, err := fs.ReadFile(h.ui, serveName)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("The dashboard UI is not built. Run `make web` (or rebuild the assistant image), then restart.\n"))
		return
	}
	// The Spotify Connect button is a same-origin GET link to /spotify/connect,
	// which answers 303 to Spotify's authorize page. A form POST would make the
	// browser apply form-action to that redirect, so the policy would have to
	// name Spotify's origin; a navigation is not a form submission and the
	// document keeps form-action 'self'.
	//
	// media-src is on the document for the voice-sample recorder: a take is
	// played back from a blob: URL before it is kept, and a kept take is
	// same-origin. The app is one document for every page, so this cannot be
	// narrowed to one route without making the recorder a separate document.
	w.Header().Set("Content-Security-Policy", appSecurityPolicy())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if coding != "" {
		w.Header().Set("Content-Encoding", coding)
		w.Header().Set("Content-Length", strconv.Itoa(len(index)))
	}
	_, _ = w.Write(index)
}

// isAssetPath reports a build file rather than a page route: everything Vite
// emits under assets/ plus the few root files copied from web/public.
func isAssetPath(name string) bool {
	if strings.HasPrefix(name, "assets/") || strings.HasPrefix(name, "icons/") {
		return true
	}
	switch name {
	case "theme.js", "favicon.svg", "manifest.webmanifest", "offline.html", "sw.js":
		return true
	}
	return false
}

func (h *Handler) appFile(w http.ResponseWriter, r *http.Request, name string) {
	// The build emits .gz/.br siblings for text assets; serve one when the
	// client accepts it rather than compressing the same bytes on every hit.
	served, coding := "", ""
	if r.Header.Get("Range") == "" {
		served, coding = precompressed(h.ui, name, r.Header.Get("Accept-Encoding"))
	}
	if served == "" {
		served = name
	}
	f, err := h.ui.Open(served)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasPrefix(name, "assets/"):
		// Vite content-hashes these names, so a cached copy can never be
		// stale. private: they are still identity-gated.
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	case name == "sw.js":
		// A service worker must be revalidated or a new build never arrives.
		w.Header().Set("Cache-Control", "no-cache")
	}
	// Go's mime table does not know the manifest extension; without this the
	// file would be sniffed and Chrome would reject it.
	if path.Ext(name) == ".webmanifest" {
		w.Header().Set("Content-Type", "application/manifest+json")
	}
	if coding != "" {
		// The sibling has no useful extension: ServeContent is given the
		// original name so the content type still comes from it.
		w.Header().Set("Content-Encoding", coding)
		w.Header().Set("Accept-Ranges", "none")
		// ServeContent leaves the length to chunking when an encoding is set;
		// the sibling's size is exact, so send it.
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	seeker, ok := f.(interface {
		fs.File
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), seeker)
}

// precompressed chooses the best build-time encoding the client accepts. Only
// text assets get siblings; fonts and images always fall through to the plain
// file.
func precompressed(fsys fs.FS, name, accept string) (string, string) {
	switch path.Ext(name) {
	case ".js", ".css", ".html", ".svg", ".json":
	default:
		return "", ""
	}
	if _, err := fs.Stat(fsys, name); err != nil {
		return "", ""
	}
	if acceptsEncoding(accept, "br") {
		if _, err := fs.Stat(fsys, name+".br"); err == nil {
			return name + ".br", "br"
		}
	}
	if acceptsEncoding(accept, "gzip") {
		if _, err := fs.Stat(fsys, name+".gz"); err == nil {
			return name + ".gz", "gzip"
		}
	}
	return "", ""
}
