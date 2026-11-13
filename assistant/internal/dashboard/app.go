package dashboard

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
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
	index, err := fs.ReadFile(h.ui, "index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("The dashboard UI is not built. Run `make web` (or rebuild the assistant image), then restart.\n"))
		return
	}
	// The Spotify Connect button is a real form POST to /spotify/connect,
	// which answers 303 to Spotify's authorize page. Browsers apply
	// form-action to that redirect too, and the app is one document for every
	// page, so the document allows exactly Spotify's accounts origin.
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'self' "+spotifyAccountsOrigin))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

// isAssetPath reports a build file rather than a page route: everything Vite
// emits under assets/ plus the few root files copied from web/public.
func isAssetPath(name string) bool {
	if strings.HasPrefix(name, "assets/") {
		return true
	}
	switch name {
	case "theme.js", "favicon.svg":
		return true
	}
	return false
}

func (h *Handler) appFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := h.ui.Open(name)
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
	if strings.HasPrefix(name, "assets/") {
		// Vite content-hashes these names, so a cached copy can never be
		// stale. private: they are still identity-gated.
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
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
	http.ServeContent(w, r, info.Name(), info.ModTime(), seeker)
}
