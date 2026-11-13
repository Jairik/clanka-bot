package dashboard

import (
	"encoding/json"
	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/tools/lights"
	"net/http"
)

// lightsAPI uses the registered module so Tools, Health and Dashboard share one service.
func (h *Handler) lightsAPI(w http.ResponseWriter, r *http.Request) {
	if h.toolModules == nil {
		http.Error(w, "Lights unavailable", 503)
		return
	}
	m, ok := h.toolModules.Module("lights").(*lights.Module)
	if !ok || m.Service == nil {
		http.Error(w, "Lights unavailable", 503)
		return
	}
	s := m.Service
	if r.Method == "GET" {
		h.writeJSON(w, 200, map[string]any{"enabled": s.Enabled(), "devices": s.Devices(), "status": s.Last()})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in struct {
		Action  string        `json:"action"`
		Device  string        `json:"device"`
		Value   string        `json:"value"`
		Devices []core.Device `json:"devices"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "Invalid request", 400)
		return
	}
	switch in.Action {
	case "scan":
		ds, err := s.Scan(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		h.writeJSON(w, 200, ds)
		return
	case "enable":
		if h.settings == nil || (in.Value != "true" && in.Value != "false") {
			http.Error(w, "Invalid enabled setting", 400)
			return
		}
		if _, err := h.settings.SetJSON(r.Context(), "lights.enabled", json.RawMessage(in.Value)); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	case "save":
		if h.settings == nil {
			http.Error(w, "Settings unavailable", 503)
			return
		}
		raw, _ := json.Marshal(in.Devices)
		value, _ := json.Marshal(string(raw))
		if _, err := h.settings.SetJSON(r.Context(), "lights.devices", value); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	default:
		if err := s.Apply(r.Context(), in.Device, in.Action, in.Value); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	h.writeJSON(w, 200, map[string]bool{"ok": true})
}
