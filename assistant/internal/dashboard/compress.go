// Response compression for the Dashboard's own bytes. Static text assets are
// precompressed by the Vite build (.gz and .br siblings) and pick their
// encoding in appFile; everything dynamic (the document and the JSON views) is
// gzipped once here, in the one place every route already passes through.
package dashboard

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// acceptsEncoding reports whether the client accepts coding, honouring q=0.
func acceptsEncoding(header, coding string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), coding) {
			continue
		}
		for _, param := range fields[1:] {
			param = strings.TrimSpace(param)
			if !strings.HasPrefix(param, "q=") {
				continue
			}
			q, err := strconv.ParseFloat(strings.TrimPrefix(param, "q="), 64)
			// q=0 means "not acceptable", the only value that turns it off.
			return err != nil || q > 0
		}
		return true
	}
	return false
}

// compressible reports whether a content type benefits from gzip. Media,
// fonts and images are already compressed and must pass through byte for byte.
func compressible(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(strings.ToLower(base)) {
	case "text/html", "text/css", "text/plain", "text/xml",
		"application/javascript", "text/javascript",
		"application/json", "application/manifest+json",
		"application/xml", "image/svg+xml":
		return true
	}
	return false
}

// compressWriter gzips a 200 answer whose content type is text. It decides at
// WriteHeader time, so a handler that already chose an encoding (a
// precompressed asset) or a non-text body is untouched.
type compressWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (w *compressWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.Header()
	// Vary is always set: the same URL has a gzipped and a plain form.
	h.Add("Vary", "Accept-Encoding")
	if status == http.StatusOK && h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
		// The length changes; a streamed body is chunked rather than lying.
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		w.gz = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz == nil {
		return w.ResponseWriter.Write(p)
	}
	return w.gz.Write(p)
}

func (w *compressWriter) Close() error {
	if w.gz == nil {
		return nil
	}
	return w.gz.Close()
}

// Unwrap keeps http.ResponseController (and anything that needs the original
// writer, such as a future streaming route) working through the wrapper.
func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush sends what gzip has buffered when the underlying writer supports it.
func (w *compressWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
