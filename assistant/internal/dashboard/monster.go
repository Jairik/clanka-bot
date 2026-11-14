package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	monstertools "github.com/Jairik/clanka-bot/assistant/tools/monster"
)

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
			h.writeJSON(w, http.StatusOK, core.View{
				Devices: []core.DeviceView{},
				Status:  "Monster lights are not wired into this assistant build.",
			})
			return
		}
		http.Error(w, "Monster lights unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		h.writeJSON(w, http.StatusOK, m.Service.View(r.Context()))
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
