// Package lights exposes reversible BLE controls to both assistant paths.
package lights

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module owns the same controller service used by the Dashboard.
type Module struct{ Service *core.Service }

func NewModule() *Module     { return &Module{} }
func (*Module) Name() string { return "lights" }

// In selects a saved device, the "all" target or, by omission, every saved
// light. A single saved light is the only case an omitted device used to mean.
type In struct {
	Device string `json:"device,omitempty"`
	Value  string `json:"value"`
}
type Out struct {
	Reply string `json:"reply"`
}

// ListIn takes no arguments, so the Fast path can ask what is saved.
type ListIn struct{}

// ListDevice is one saved light as the Agent sees it: a name to target and a
// protocol hint, never the Bluetooth address.
type ListDevice struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

type ListOut struct {
	Devices []ListDevice `json:"devices"`
}

var patterns = map[string]*regexp.Regexp{
	"power":      regexp.MustCompile(`(?i)\b(on|off)\b`),
	"brightness": regexp.MustCompile(`\b(100|[0-9]{1,2})\b`),
	"color":      regexp.MustCompile(`(?i)#[0-9a-f]{6}\b|\b(red|green|blue|white)\b`),
	"mode":       regexp.MustCompile(`\b(rainbow|pulse|strobe)\b`),
}

// lightsTarget is the plural wording that means every saved light.
var lightsTarget = regexp.MustCompile(`(?i)\blights\b`)

// Register declares act and LocalOnlyNo: outputs contain only command
// acknowledgements. lights.list is read-only and LocalOnlyNo because saved
// names and protocols are not addresses, so the Agent may use them to choose.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.Service = core.New(deps.Settings)
	m.Service.Names = deps.Names
	for _, op := range []string{"power", "brightness", "color", "mode"} {
		examples := map[string][]string{"power": {"turn the lights on", "switch the lights off", "power on the lights"}, "brightness": {"set lights brightness to 50 percent", "dim the lights to 20 percent", "lights brightness 80 percent"}, "color": {"set the lights color to red", "change lights color to blue", "lights color #00ff00"}, "mode": {"set the lights mode to rainbow", "lights pulse mode", "change lights mode to strobe"}}
		// Fifteen seconds sits just outside the BLE write budget. The batch
		// inside Apply shares one budget, so the static Timeout covers every
		// selected light rather than multiplying by the device count.
		t := tool.Tool[In, Out]{Name: "lights." + op, Description: fmt.Sprintf("Set a saved BLE light's %s. Value: %s. Device is a saved name, an address, or \"all\" for every saved light; omit it only to target all of them.", op, map[string]string{"power": "on/off", "brightness": "0–100 (HappyLighting sets grayscale RGB)", "color": "#RRGGBB", "mode": "rainbow/pulse/strobe"}[op]), Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo, Timeout: 15 * time.Second, Examples: examples[op], Speak: func(out Out) string { return out.Reply },
			Extract: func(text string) (In, bool) {
				text = strings.ToLower(text)
				// Questions and combined requests must remain on the Agent path.
				for _, marker := range []string{"how do", "how can", "why ", "what ", "explain ", " and "} {
					if strings.Contains(text, marker) {
						return In{}, false
					}
				}
				saysLights := lightsTarget.MatchString(text)
				device := ""
				if m.Service != nil {
					// A saved name wins over the plural target. Fuzzy resolution
					// is skipped when the words already say "lights", so a plain
					// "turn the lights off" needs no embedding call.
					named, ambiguous := m.Service.SpokenName(context.Background(), text, !saysLights)
					if ambiguous {
						return In{}, false
					}
					device = named
				}
				if device == "" {
					switch {
					case saysLights:
						device = core.AllTarget
					case m.Service != nil && len(m.Service.Devices()) == 1:
						// One saved light is the only possible target.
					default:
						// No saved name and no plural target: the Agent path
						// must decide what "the light" means.
						return In{}, false
					}
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
				return In{Device: device, Value: v}, true
			},
			Handler: func(ctx context.Context, in In) (Out, error) {
				res, err := m.Service.Apply(ctx, in.Device, op, in.Value)
				if err != nil {
					return Out{}, &tool.ToolError{Say: err.Error()}
				}
				return Out{Reply: res.Reply()}, nil
			},
		}
		if err := tool.Add(r, t); err != nil {
			return err
		}
	}
	return tool.Add(r, m.list())
}

// list reports the saved lights so the Agent can choose a device name. It is
// a read-only leaf operation: names and protocols only, never addresses.
func (m *Module) list() tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        "lights.list",
		Description: "List the saved BLE lights by name and protocol. Addresses are never returned; pass a name to lights.power, lights.brightness, lights.color or lights.mode.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Examples: []string{
			"what lights are saved",
			"list my saved lights",
			"which lights are saved",
			"show me the saved lights",
			"what saved lights do I have",
			"list the saved lights",
		},
		Speak: func(out ListOut) string {
			if len(out.Devices) == 0 {
				return "No lights are saved."
			}
			parts := make([]string, 0, len(out.Devices))
			for _, d := range out.Devices {
				parts = append(parts, d.Name+" ("+d.Protocol+")")
			}
			return "Saved lights: " + strings.Join(parts, ", ") + "."
		},
		Handler: func(context.Context, ListIn) (ListOut, error) {
			if m.Service == nil {
				return ListOut{}, &tool.ToolError{Say: "Lights are unavailable."}
			}
			devices := m.Service.Devices()
			out := ListOut{Devices: make([]ListDevice, 0, len(devices))}
			for _, d := range devices {
				out.Devices = append(out.Devices, ListDevice{Name: d.Name, Protocol: d.Protocol})
			}
			return out, nil
		},
	}
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
		if m.Service.Failed() {
			state = tool.StatusError
		}
	}
	return []tool.AuxHealth{{Name: "BLE lights", State: state, Detail: detail}}
}
