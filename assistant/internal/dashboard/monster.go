package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	monstertools "github.com/Jairik/clanka-bot/assistant/tools/monster"
)

// monsterEffectView is one scene slot on a Monster light.
type monsterEffectView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// monsterDeviceView is one light plus its last cloud state. Error is set per
// device so one unreachable light does not hide the rest.
type monsterDeviceView struct {
	DSN        string              `json:"dsn"`
	Name       string              `json:"name"`
	Model      string              `json:"model"`
	On         bool                `json:"on"`
	Mode       string              `json:"mode"`
	Brightness int                 `json:"brightness"`
	Color      string              `json:"color"`
	Kelvin     int                 `json:"kelvin"`
	Effects    []monsterEffectView `json:"effects"`
	Error      string              `json:"error,omitempty"`
}

// monsterView is the whole Monster page. Configured false carries the setup
// line in Status and no devices; a configured account that cannot be read
// carries the cause in Error.
type monsterView struct {
	Configured bool                `json:"configured"`
	Status     string              `json:"status"`
	Error      string              `json:"error,omitempty"`
	Devices    []monsterDeviceView `json:"devices"`
}

// monsterViewFromCore copies the service view into the page's view struct. The
// JSON tags above match core.View exactly, so this is a wire-for-wire copy.
func monsterViewFromCore(v core.View) monsterView {
	out := monsterView{
		Configured: v.Configured,
		Status:     v.Status,
		Error:      v.Error,
		Devices:    make([]monsterDeviceView, 0, len(v.Devices)),
	}
	for _, d := range v.Devices {
		device := monsterDeviceView{
			DSN: d.DSN, Name: d.Name, Model: d.Model, On: d.On, Mode: d.Mode,
			Brightness: d.Brightness, Color: d.Color, Kelvin: d.Kelvin, Error: d.Error,
			Effects: make([]monsterEffectView, 0, len(d.Effects)),
		}
		for _, e := range d.Effects {
			device.Effects = append(device.Effects, monsterEffectView{ID: e.ID, Name: e.Name})
		}
		out.Devices = append(out.Devices, device)
	}
	return out
}

// monsterAPI is the Monster Smart Lighting page view and its writes. Reads
// always answer 200: upstream trouble belongs in the view's status line, not
// in a JSON error the page cannot render.
func (h *Handler) monsterAPI(w http.ResponseWriter, r *http.Request) {
	var m *monstertools.Module
	if h.toolModules != nil {
		m, _ = h.toolModules.Module("monster").(*monstertools.Module)
	}
	if m == nil || m.Service == nil {
		if r.Method == http.MethodGet {
			h.writeJSON(w, http.StatusOK, monsterView{
				Devices: []monsterDeviceView{},
				Status:  "Monster lights are not wired into this assistant build.",
			})
			return
		}
		http.Error(w, "Monster lights unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		h.writeJSON(w, http.StatusOK, monsterViewFromCore(m.Service.View(r.Context())))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in struct {
		Action string `json:"action"`
		Device string `json:"device"`
		Value  string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if err := m.Service.Apply(r.Context(), in.Device, in.Action, in.Value); err != nil {
		switch {
		case errors.Is(err, core.ErrNotConfigured), errors.Is(err, core.ErrCredentials):
			http.Error(w, core.Friendly(err), http.StatusBadRequest)
		case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrUpstream), errors.Is(err, context.DeadlineExceeded):
			http.Error(w, core.Friendly(err), http.StatusBadGateway)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
