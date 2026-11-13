package dashboard

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// WakewordModel is one configured detector and the threshold the Assistant
// sends for it on every detect stream.
type WakewordModel struct {
	Name      string
	Threshold float64
}

// WakewordInfo is the configured wake-word allowlist. The Settings page
// renders it read-only; the Health page also probes Addr to check that the
// service advertises each configured model.
type WakewordInfo struct {
	Addr   string
	Models []WakewordModel
}

// WakewordFromConfig resolves the allowlist and thresholds main gives the voice
// loop, so the pages cannot show a different list from the one the Assistant
// listens for.
func WakewordFromConfig(cfg config.Config) WakewordInfo {
	info := WakewordInfo{Addr: strings.TrimSpace(cfg.Wakeword.Addr)}
	for _, name := range cfg.Wakeword.WakeModels() {
		info.Models = append(info.Models, WakewordModel{
			Name:      name,
			Threshold: cfg.Wakeword.ThresholdFor(name),
		})
	}
	return info
}

// wakewordSettingsView is the read-only Wake word section on Settings. These
// values are connection-level (restart required) and live in .env or
// config.yaml, so the page reports them rather than offering a write that
// would only take effect after a restart.
type wakewordSettingsView struct {
	Enabled bool
	Addr    string
	Models  []wakewordModelView
	Note    string
}

type wakewordModelView struct {
	Name      string
	Threshold string
}

func (h *Handler) wakewordSettings() wakewordSettingsView {
	view := wakewordSettingsView{
		Enabled: h.wakeword.Addr != "",
		Addr:    h.wakeword.Addr,
	}
	for _, model := range h.wakeword.Models {
		view.Models = append(view.Models, wakewordModelView{
			Name:      model.Name,
			Threshold: strconv.FormatFloat(model.Threshold, 'f', 2, 64),
		})
	}
	if !view.Enabled {
		view.Note = "Voice input is off: wakeword.addr is empty. Set the wakeword address and at least one model to enable it."
		return view
	}
	view.Note = "Use WAKEWORD_MODELS for the comma-separated list and WAKEWORD_THRESHOLDS (for example clanker:0.6,okay_nabu:0.5) for per-model overrides. These are connection settings: change them in .env or config.yaml and restart the assistant."
	return view
}

// wakewordChecks reports one row per configured model, with the service's
// advertised list as the readiness check. The upstream service advertises
// every model it can serve; a model that is not listed is a typo, not a model
// that will load on demand.
func (h *Handler) wakewordChecks(ctx context.Context) []checkView {
	if strings.TrimSpace(h.wakeword.Addr) == "" {
		return []checkView{{
			Name:        "wakeword",
			Status:      string(tool.StatusNotConfigured),
			StatusLabel: "off",
			Detail:      "Voice input is disabled (wakeword.addr is empty).",
		}}
	}
	if len(h.wakeword.Models) == 0 {
		return []checkView{{
			Name:        "wakeword",
			Status:      string(tool.StatusError),
			StatusLabel: "no models",
			Detail:      "No wake model is configured, so voice input cannot start a turn.",
		}}
	}

	var advertised map[string]bool
	var probeErr error
	client := wyoming.NewClient(h.wakeword.Addr, wyoming.WithTimeout(wyomingHealthTimeout))
	describe, err := client.Describe(ctx)
	if err != nil {
		probeErr = err
	} else {
		advertised = make(map[string]bool)
		for _, program := range describe.Wake {
			for _, model := range program.Models {
				advertised[model.Name] = true
			}
		}
	}

	views := make([]checkView, 0, len(h.wakeword.Models))
	for _, model := range h.wakeword.Models {
		view := checkView{
			Name:   model.Name,
			Detail: fmt.Sprintf("Threshold %.2f", model.Threshold),
		}
		switch {
		case probeErr != nil:
			view.Status = string(tool.StatusError)
			view.StatusLabel = "unknown"
			view.Detail += " · the wakeword service did not answer"
		case advertised[model.Name]:
			view.Status = string(tool.StatusOK)
			view.StatusLabel = "served"
			view.Detail += " · advertised by the wakeword service"
		default:
			view.Status = string(tool.StatusError)
			view.StatusLabel = "not served"
			view.Detail += " · the wakeword service does not advertise this model"
		}
		views = append(views, view)
	}
	return views
}
