// Package lights exposes reversible BLE controls to both assistant paths.
package lights

import (
	"context"
	"fmt"
	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"regexp"
	"strings"
)

// Module owns the same controller service used by the Dashboard.
type Module struct{ Service *core.Service }

func NewModule() *Module     { return &Module{} }
func (*Module) Name() string { return "lights" }

// In selects a saved device; omit device only when exactly one is saved.
type In struct {
	Device string `json:"device,omitempty"`
	Value  string `json:"value"`
}
type Out struct {
	Reply string `json:"reply"`
}

var patterns = map[string]*regexp.Regexp{
	"power":      regexp.MustCompile(`(?i)\b(on|off)\b`),
	"brightness": regexp.MustCompile(`\b(100|[0-9]{1,2})\b`),
	"color":      regexp.MustCompile(`(?i)#[0-9a-f]{6}\b|\b(red|green|blue|white)\b`),
	"mode":       regexp.MustCompile(`\b(rainbow|pulse|strobe)\b`),
}

// Register declares act and LocalOnlyNo: outputs contain only command acknowledgements.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.Service = core.New(deps.Settings)
	for _, op := range []string{"power", "brightness", "color", "mode"} {
		examples := map[string][]string{"power": {"turn the lights on", "switch the lights off", "power on the lights"}, "brightness": {"set lights brightness to 50 percent", "dim the lights to 20 percent", "lights brightness 80 percent"}, "color": {"set the lights color to red", "change lights color to blue", "lights color #00ff00"}, "mode": {"set the lights mode to rainbow", "lights pulse mode", "change lights mode to strobe"}}
		t := tool.Tool[In, Out]{Name: "lights." + op, Description: fmt.Sprintf("Set a saved BLE light's %s. Value: %s. Device is its saved name or address; omit only with one saved light.", op, map[string]string{"power": "on/off", "brightness": "0–100 (HappyLighting sets grayscale RGB)", "color": "#RRGGBB", "mode": "rainbow/pulse/strobe"}[op]), Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo, Examples: examples[op], Speak: func(out Out) string { return out.Reply },
			Extract: func(text string) (In, bool) {
				text = strings.ToLower(text)
				// Questions and combined requests must remain on the Agent path.
				for _, marker := range []string{"how do", "how can", "why ", "what ", "explain ", " and "} {
					if strings.Contains(text, marker) {
						return In{}, false
					}
				}
				if !strings.Contains(text, "light") {
					return In{}, false
				}
				v := patterns[op].FindString(text)
				if v == "" {
					return In{}, false
				}
				if op == "color" {
					if c, ok := map[string]string{"red": "#ff0000", "green": "#00ff00", "blue": "#0000ff", "white": "#ffffff"}[v]; ok {
						v = c
					}
				}
				return In{Value: v}, true
			},
			Handler: func(ctx context.Context, in In) (Out, error) {
				if err := m.Service.Apply(ctx, in.Device, op, in.Value); err != nil {
					return Out{}, &tool.ToolError{Say: err.Error()}
				}
				return Out{Reply: "Light command sent."}, nil
			},
		}
		if err := tool.Add(r, t); err != nil {
			return err
		}
	}
	return nil
}

// AuxHealth never connects just to probe a write-only controller.
func (m *Module) AuxHealth(context.Context) []tool.AuxHealth {
	state := tool.StatusNotConfigured
	detail := "Disabled by default"
	if m.Service != nil && m.Service.Enabled() {
		state = tool.StatusNoCheck
		detail = m.Service.Last()
		if detail == "" {
			detail = "Enabled; physical state unverified"
		}
		if strings.Contains(detail, "failed") {
			state = tool.StatusError
		}
	}
	return []tool.AuxHealth{{Name: "BLE lights", State: state, Detail: detail}}
}
