package hermesadapter

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const (
	// maxBodyBytes bounds list/schema/invoke payloads. Tool arguments are small.
	maxBodyBytes = 16 << 10
	// minTokenBytes is the same floor the Hermes sidecar already requires.
	minTokenBytes = 32
)

// NewHandler serves the private adapter on the health listener. Auth is the
// shared HERMES_RUNTIME_TOKEN bearer; a short or empty token yields nil so
// main can leave the route unmounted and fail closed.
func NewHandler(adapter *Adapter, token string) http.Handler {
	if adapter == nil || len(token) < minTokenBytes {
		return nil
	}
	h := &httpAPI{adapter: adapter, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", h.list)
	mux.HandleFunc("GET /tools/{name}", h.schema)
	mux.HandleFunc("POST /tools/{name}", h.invoke)
	return h.auth(mux)
}

type httpAPI struct {
	adapter *Adapter
	token   string
}

type invokeRequest struct {
	Arguments json.RawMessage `json:"arguments"`
}

// auth checks the bearer token before any listing or call.
func (h *httpAPI) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Revoked grants and removed recipes must take effect on the next request.
		w.Header().Set("Cache-Control", "no-store")
		if !bearerOK(r.Header.Get("Authorization"), h.token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// list returns currently callable Tools with no per-tool schemas.
func (h *httpAPI) list(w http.ResponseWriter, r *http.Request) {
	tools := h.adapter.surface(r.Context()).Allowed
	items := make([]map[string]string, 0, len(tools))
	for _, item := range tools {
		items = append(items, map[string]string{
			"name":        item.Name,
			"description": item.Description,
			"permission":  item.Permission,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": items})
}

// schema loads one Tool's contract only after the caller names it.
func (h *httpAPI) schema(w http.ResponseWriter, r *http.Request) {
	schema, err := h.adapter.schema(r.Context(), r.PathValue("name"))
	if err != nil {
		writeDenied(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":          schema.Name,
		"description":   schema.Description,
		"permission":    schema.Permission,
		"input_schema":  json.RawMessage(schema.InputSchema),
		"output_schema": json.RawMessage(schema.OutputSchema),
	})
}

// invoke runs one allowed Tool and returns its result or a Tool-level error.
func (h *httpAPI) invoke(w http.ResponseWriter, r *http.Request) {
	var req invokeRequest
	if !decodeJSON(w, r, &req) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	result, err := h.adapter.Invoke(r.Context(), r.PathValue("name"), req.Arguments)
	if err != nil {
		var toolErr *tool.ToolError
		if errors.As(err, &toolErr) && toolErr.Say != "" {
			writeJSON(w, http.StatusOK, map[string]string{
				"name":  r.PathValue("name"),
				"error": toolErr.Say,
			})
			return
		}
		writeDenied(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   r.PathValue("name"),
		"result": result,
	})
}

// bearerOK is a constant-time compare of the shared runtime token.
func bearerOK(header, token string) bool {
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || len(got) != len(token) || len(token) < minTokenBytes {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// writeDenied maps every policy refusal to a generic 403.
func writeDenied(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrDenied) || errors.Is(err, ErrSecret) {
		writeError(w, http.StatusForbidden, "not allowed")
		return
	}
	writeError(w, http.StatusForbidden, "not allowed")
}

// decodeJSON reads one bounded object. Unknown fields are refused.
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

// writeJSON writes one JSON object. Encoding errors are ignored after the
// status is sent; the body is small and built from our own structs.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeError is the one-line failure body. It never includes a secret.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
