package dashboard

import (
	"encoding/json"
	"net/http"

	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/tools/lights"
)

// lightsDeviceView is one saved controller with an explicitly selected protocol.
type lightsDeviceView struct {
	Address  string `json:"address"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// lightsView is the whole Lights page. Fields are declared in the alphabetical
// key order the old map[string]any response marshalled, so the bytes are equal.
type lightsView struct {
	Devices []lightsDeviceView `json:"devices"`
	Enabled bool               `json:"enabled"`
	Status  string             `json:"status"`
}

func lightsViewFrom(devices []core.Device, enabled bool, status string) lightsView {
	out := lightsView{
		Devices: make([]lightsDeviceView, 0, len(devices)),
		Enabled: enabled,
		Status:  status,
	}
	for _, d := range devices {
		out.Devices = append(out.Devices, lightsDeviceView{Address: d.Address, Name: d.Name, Protocol: d.Protocol})
	}
	return out
}

// lightsAPI uses the registered module so Tools, Health and Dashboard share one service.
func (h *Handler) lightsAPI(w http.ResponseWriter, r *http.Request) {
	var m *lights.Module
	if h.toolModules != nil {
		m, _ = h.toolModules.Module("lights").(*lights.Module)
	}
	if m == nil || m.Service == nil {
		// No lights module is a normal state (feature never wired), not a server
		// fault: reads report it as disabled so pages that poll stay clean.
		if r.Method == "GET" {
			h.writeJSON(w, 200, lightsViewFrom(nil, false, "unavailable"))
			return
		}
		http.Error(w, "Lights unavailable", 503)
		return
	}
	s := m.Service
	if r.Method == "GET" {
		h.writeJSON(w, 200, lightsViewFrom(s.Devices(), s.Enabled(), s.Last()))
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
