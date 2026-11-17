package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wakethreshold"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// WakewordModel is one configured detector and the threshold the Assistant
// sends for it on every detect stream.
type WakewordModel struct {
	Name      string
	Threshold float64
	// Command marks a wake-free command model. It acts without starting a
	// turn, so the two kinds are shown separately.
	Command bool
	// Enabled is always true for a wake word. A command model can be
	// configured but switched off, in which case it is never requested.
	Enabled bool
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
// listens for. Command models are included even while disabled, so the page
// shows the switch rather than hiding the feature.
func WakewordFromConfig(cfg config.Config) WakewordInfo {
	info := WakewordInfo{Addr: strings.TrimSpace(cfg.Wakeword.Addr)}
	for _, name := range cfg.Wakeword.WakeModels() {
		info.Models = append(info.Models, WakewordModel{
			Name:      name,
			Threshold: cfg.Wakeword.ThresholdFor(name),
			Enabled:   true,
		})
	}
	for _, name := range cfg.Wakeword.Commands.ModelNames() {
		info.Models = append(info.Models, WakewordModel{
			Name:      name,
			Threshold: cfg.Wakeword.ThresholdFor(name),
			Command:   true,
			Enabled:   cfg.Wakeword.Commands.Enabled,
		})
	}
	return info
}

// wakewordSettingsView is the Wake word section on Settings. The model list and
// address are connection-level (restart required) and live in .env or
// config.yaml, so they are reported rather than offered as a write. Each
// threshold is a live setting (wakeword.threshold.<model>), so its row is a
// form with a Reset.
type wakewordSettingsView struct {
	Enabled bool
	Addr    string
	Models  []wakewordModelView
	Note    string
}

type wakewordModelView struct {
	Name      string
	Threshold string
	Command   bool
	Enabled   bool
	// Setting is the live threshold editor. It is nil when the Dashboard was
	// built without the settings service, and the row then shows the
	// configured value read-only.
	Setting *settingView
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
			Command:   model.Command,
			Enabled:   model.Enabled,
		})
	}
	if !view.Enabled {
		view.Note = "Voice input is off: wakeword.addr is empty. Set the wakeword address and at least one model to enable it."
		return view
	}
	view.Note = "The model list (WAKEWORD_MODELS) and address are connection settings: change them in .env or config.yaml and restart the assistant. Each threshold is live: save wakeword.threshold.<model> and it applies to the next detection, and Reset returns to WAKEWORD_THRESHOLDS or config.yaml. Wake-free commands are controlled by WAKEWORD_COMMANDS_ENABLED and are off by default."
	return view
}

// takeThreshold attaches one live threshold value to its model row. The Wake
// word section owns every wakeword.* key, so the generic Live and Restart
// groups never show a second copy; the address stays read-only above.
func (v *wakewordSettingsView) takeThreshold(key string, setting settingView) {
	name, ok := strings.CutPrefix(key, wakethreshold.Prefix)
	if !ok {
		return
	}
	for index := range v.Models {
		if v.Models[index].Name != name {
			continue
		}
		setting.Label = "Threshold"
		v.Models[index].Setting = &setting
		v.Models[index].Threshold = setting.Display
		return
	}
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
	if h.wakeModels == nil {
		probeErr = errors.New("the wakeword model cache is not wired")
	} else {
		advertised, probeErr = h.wakeModels.models(ctx)
	}

	views := make([]checkView, 0, len(h.wakeword.Models))
	for _, model := range h.wakeword.Models {
		view := checkView{
			Name:   model.Name,
			Detail: fmt.Sprintf("Threshold %.2f", model.Threshold),
		}
		if model.Command {
			view.Detail = "Wake-free command · " + view.Detail
			if !model.Enabled {
				view.Status = string(tool.StatusNotConfigured)
				view.StatusLabel = "off"
				view.Detail += " · commands are disabled"
				views = append(views, view)
				continue
			}
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

// describeWakeModels asks the wakeword service which models it serves. It is
// the one runtime signal for "this phrase has a trained model": the detector
// image bakes in wakeword/models/*.tflite and advertises each by name, so a
// model that is not listed is not one the Assistant can listen for. One
// WakeModelCache shares the answer between the Compose row, Health and the
// Voice samples page, so a render describes the service once.
func describeWakeModels(ctx context.Context, addr string) (map[string]bool, error) {
	client := wyoming.NewClient(addr, wyoming.WithTimeout(wyomingHealthTimeout))
	describe, err := client.Describe(ctx)
	if err != nil {
		return nil, err
	}
	advertised := make(map[string]bool)
	for _, program := range describe.Wake {
		for _, model := range program.Models {
			advertised[model.Name] = true
		}
	}
	return advertised, nil
}
