// Package secretsapi exposes the write-only secrets metadata API as a plain
// http.Handler. It performs no authentication of its own: mount it only on an
// allowlisted debug listener or the authenticated Dashboard. A secret value can
// be set or cleared here, but no response or log ever carries it.
package secretsapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
)

const maxBodyBytes = 64 << 10

// New builds the secrets handler. A nil service fails closed rather than
// panicking inside a request.
func New(svc *secrets.Service, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{secrets: svc, logger: logger}
	if svc == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "secrets are not configured"})
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /secrets", h.list)
	mux.HandleFunc("GET /secrets/{name}", h.get)
	mux.HandleFunc("PUT /secrets/{name}", h.put)
	mux.HandleFunc("DELETE /secrets/{name}", h.clear)
	return mux
}

type handler struct {
	secrets *secrets.Service
	logger  *slog.Logger
}

func (h *handler) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"secrets": h.secrets.List()})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	meta, ok := h.secrets.Metadata(r.PathValue("name"))
	if !ok {
		h.fail(w, http.StatusNotFound, "unknown secret")
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

type setRequest struct {
	Value string `json:"value"`
}

func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	var body setRequest
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	meta, err := h.secrets.Set(r.Context(), name, body.Value)
	if err != nil {
		h.reject(w, r, name, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (h *handler) clear(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	meta, err := h.secrets.Clear(r.Context(), name)
	if err != nil {
		h.reject(w, r, name, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// decode reads one strict JSON object. Unknown fields and trailing data are
// request errors; the body is never logged.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

// reject maps a service error to a status. Only the error text crosses the
// API; the submitted value never does.
func (h *handler) reject(w http.ResponseWriter, r *http.Request, name string, err error) {
	status := http.StatusInternalServerError
	message := "secret store failed"
	switch {
	case errors.Is(err, secrets.ErrUnknownSecret):
		status = http.StatusNotFound
		message = "unknown secret"
	case errors.Is(err, secrets.ErrEmptyValue):
		status = http.StatusBadRequest
		message = "value is required"
	}
	h.logger.Warn("secrets api",
		"method", r.Method,
		"path", r.URL.Path,
		"name", name,
		"status", status,
		"error", err.Error(),
	)
	writeJSON(w, status, map[string]string{"error": message})
}

func (h *handler) fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
