// Package settingsapi exposes the settings and Tool-override API as a plain
// http.Handler. It performs no authentication of its own: mount it only on an
// authenticated listener (the localhost debug entry today, the Tailscale
// Dashboard in #47). The public health listener never mounts it.
package settingsapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
)

const maxBodyBytes = 64 << 10

// New builds the settings handler. policy may be nil when only generic
// settings are wanted; the /tools routes are then absent.
func New(svc *settings.Service, policy *toolpolicy.Policy, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{settings: svc, tools: policy, logger: logger}
	mux := http.NewServeMux()
	if svc == nil {
		// A wiring mistake should fail closed, not panic inside a request.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			h.fail(w, http.StatusInternalServerError, "settings are not configured")
		})
	}
	mux.HandleFunc("GET /settings", h.listSettings)
	mux.HandleFunc("GET /settings/{key}", h.getSetting)
	mux.HandleFunc("PUT /settings/{key}", h.putSetting)
	mux.HandleFunc("DELETE /settings/{key}", h.deleteSetting)
	if policy != nil {
		mux.HandleFunc("GET /tools", h.listTools)
		mux.HandleFunc("GET /tools/{name}", h.getTool)
		mux.HandleFunc("PUT /tools/{name}", h.putTool)
		mux.HandleFunc("DELETE /tools/{name}", h.deleteTool)
	}
	return mux
}

type handler struct {
	settings *settings.Service
	tools    *toolpolicy.Policy
	logger   *slog.Logger
}

func (h *handler) listSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": h.settings.List()})
}

func (h *handler) getSetting(w http.ResponseWriter, r *http.Request) {
	value, ok := h.settings.Get(r.PathValue("key"))
	if !ok {
		h.fail(w, http.StatusNotFound, "unknown setting")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

type settingRequest struct {
	Value json.RawMessage `json:"value"`
}

func (h *handler) putSetting(w http.ResponseWriter, r *http.Request) {
	var body settingRequest
	if !decode(w, r, &body) {
		return
	}
	key := r.PathValue("key")
	value, err := h.settings.SetJSON(r.Context(), key, body.Value)
	if err != nil {
		h.reject(w, r, key, err, isUnknownSetting(err))
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) deleteSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	value, err := h.settings.Reset(r.Context(), key)
	if err != nil {
		h.reject(w, r, key, err, isUnknownSetting(err))
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) listTools(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": h.tools.List()})
}

func (h *handler) getTool(w http.ResponseWriter, r *http.Request) {
	state, ok := h.tools.Get(r.PathValue("name"))
	if !ok {
		h.fail(w, http.StatusNotFound, "unknown tool")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

type toolRequest struct {
	Enabled    *bool   `json:"enabled"`
	Permission *string `json:"permission"`
	// LocalOnly is accepted only to reject it with a clear message: the flag
	// is code-declared and never overridable.
	LocalOnly *bool `json:"local_only"`
}

func (h *handler) putTool(w http.ResponseWriter, r *http.Request) {
	var body toolRequest
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	if body.LocalOnly != nil {
		h.fail(w, http.StatusBadRequest, "local-only is code-declared and cannot be overridden")
		return
	}
	update := toolpolicy.Update{Enabled: body.Enabled}
	if body.Permission != nil {
		permission := tool.Permission(*body.Permission)
		update.Permission = &permission
	}
	if update.Enabled == nil && update.Permission == nil {
		h.fail(w, http.StatusBadRequest, "enabled or permission is required")
		return
	}
	state, err := h.tools.Set(r.Context(), name, update)
	if err != nil {
		h.reject(w, r, name, err, isUnknownTool(err))
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *handler) deleteTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	state, err := h.tools.Reset(r.Context(), name)
	if err != nil {
		h.reject(w, r, name, err, isUnknownTool(err))
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// decode reads one strict JSON object. Unknown fields and trailing data are
// request errors, so a misspelled field cannot silently do nothing.
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

// reject maps a service error to a status. Validation failures are 400s;
// unknown names are 404s; only the message crosses the API, never stored data.
func (h *handler) reject(w http.ResponseWriter, r *http.Request, name string, err error, notFound bool) {
	status := http.StatusBadRequest
	if notFound {
		status = http.StatusNotFound
	}
	h.logger.Warn("settings api",
		"method", r.Method,
		"path", r.URL.Path,
		"name", name,
		"status", status,
		"error", err.Error(),
	)
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (h *handler) fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func isUnknownSetting(err error) bool {
	return errors.Is(err, settings.ErrUnknownSetting)
}

func isUnknownTool(err error) bool {
	return errors.Is(err, toolpolicy.ErrUnknownTool)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
