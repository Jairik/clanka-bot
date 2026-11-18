// Package lights exposes reversible BLE controls to both assistant paths.
package lights

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lighting"
	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module owns the same controller service used by the Dashboard.
type Module struct {
	Service *core.Service
	// PC is the second transport a general command reaches: the PC / desk
	// lights on the Monster cloud. tools.NewSet wires the registered Monster
	// module; nil keeps this module Bluetooth-only. The handler reads it per
	// request, after registration, so the service behind it already exists.
	PC lighting.Transport
}

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

// groupOps are the operations both transports understand, so "the lights"
// covers the saved controllers and the PC bar. A mode is Bluetooth-only and
// white/scene belong to the Monster Tools; those keep their own wording.
var groupOps = map[string]bool{"power": true, "brightness": true, "color": true}

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
		// selected light rather than multiplying by the device count. A general
		// command runs the PC transport beside it, so the slower side still
		// answers inside this budget.
		t := tool.Tool[In, Out]{Name: "lights." + op, Description: fmt.Sprintf("Set light %s. Value: %s. Device is a saved BLE light name, an address, or \"all\" for every light; omit it to mean every light. %s", op, map[string]string{"power": "on/off", "brightness": "0–100 (HappyLighting sets grayscale RGB)", "color": "#RRGGBB", "mode": "rainbow/pulse/strobe"}[op], map[string]string{"power": "Omitted or \"all\" also sets the PC / Monster light bar.", "brightness": "Omitted or \"all\" also sets the PC / Monster light bar.", "color": "Omitted or \"all\" also sets the PC / Monster light bar.", "mode": "Modes are Bluetooth-only; the PC / Monster light bar has white and scene Tools instead."}[op]), Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo, Timeout: 15 * time.Second, Examples: examples[op], Speak: func(out Out) string { return out.Reply },
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
					// A saved name wins over the plural target and over the PC
					// aliases, so a controller literally called "Light Bar"
					// keeps its own commands. This pass is literal only.
					named, ambiguous := m.Service.SpokenName(context.Background(), text, false)
					if ambiguous {
						return In{}, false
					}
					device = named
				}
				if device == "" {
					// "pc light", "monitor light" and "light bar" name the PC
					// light, not a room controller: the Monster Tools own that
					// phrasing, even when it arrives in the plural.
					if m.PC != nil && m.PC.Targets(text) {
						return In{}, false
					}
					switch {
					case saysLights:
						device = core.AllTarget
					case m.Service != nil && len(m.Service.Devices()) == 1:
						// One saved light is the only possible target. Naming it
						// keeps a singular "the light" off the PC bar, which an
						// omitted device would mean.
						device = m.Service.Devices()[0].Name
					default:
						// No saved name and no plural target: a fuzzy reading
						// may still resolve one, otherwise the Agent path must
						// decide what "the light" means.
						if m.Service != nil {
							named, ambiguous := m.Service.SpokenName(context.Background(), text, true)
							if ambiguous {
								return In{}, false
							}
							device = named
						}
						if device == "" {
							return In{}, false
						}
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
				return m.apply(ctx, op, in)
			},
		}
		if err := tool.Add(r, t); err != nil {
			return err
		}
	}
	return tool.Add(r, m.list())
}

// apply runs one operation. A general target — an omitted device or "all" —
// also reaches the PC lights on the operations both transports understand. The
// two transports run at once and each keeps its own budget, so a slow cloud
// call cannot delay a BLE write and a cold BLE connect cannot hold the cloud
// call.
func (m *Module) apply(ctx context.Context, op string, in In) (Out, error) {
	if m.Service == nil {
		return Out{}, &tool.ToolError{Say: "Lights are unavailable."}
	}
	if !groupOps[op] || m.PC == nil || !isGroupTarget(in.Device) {
		res, err := m.Service.Apply(ctx, in.Device, op, in.Value)
		if err != nil {
			return Out{}, &tool.ToolError{Say: err.Error()}
		}
		return Out{Reply: res.Reply()}, nil
	}

	// A general command skips a transport the assistant does not have: lights
	// disabled on the page, no saved controller, or no Monster account at all.
	// A transport that owns lights but cannot answer is reported below.
	bleOn := m.Service.Enabled() && len(m.Service.Devices()) > 0
	var ble core.Result
	var bleErr error
	var pc lighting.Result
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	if bleOn {
		run(func() { ble, bleErr = m.Service.Apply(ctx, in.Device, op, in.Value) })
	}
	run(func() { pc = m.PC.ApplyEvery(ctx, op, in.Value) })
	wg.Wait()

	switch {
	case !bleOn && !pc.Addressed():
		return Out{}, &tool.ToolError{Say: "No lights are saved or set up."}
	case !bleOn:
		if pc.Err != nil {
			return Out{}, &tool.ToolError{Say: pc.Err.Error()}
		}
		return Out{Reply: "PC light command sent."}, nil
	case !pc.Addressed():
		if bleErr != nil {
			return Out{}, &tool.ToolError{Say: bleErr.Error()}
		}
		return Out{Reply: ble.Reply()}, nil
	}
	if bleErr != nil && pc.Err != nil {
		return Out{}, &tool.ToolError{Say: "Neither the room lights nor the PC light answered."}
	}
	return Out{Reply: groupReply(ble, bleErr, pc)}, nil
}

// groupReply composes the spoken line when both transports were addressed and
// at least one answered. It names each side that fell short and never pretends
// a write reached a light it did not.
func groupReply(ble core.Result, bleErr error, pc lighting.Result) string {
	sentences := []string{"Light command sent."}
	switch {
	case bleErr != nil:
		sentences = append(sentences, "No room light answered.")
	case ble.Done < ble.Total:
		sentences = append(sentences, fmt.Sprintf("%d of %d room lights answered.", ble.Done, ble.Total))
	}
	switch {
	case pc.Err != nil && pc.Applied == 0:
		sentences = append(sentences, "The PC light did not answer.")
	case pc.Err != nil || pc.Applied < pc.Total:
		sentences = append(sentences, fmt.Sprintf("%d of %d PC lights answered.", pc.Applied, pc.Total))
	}
	return strings.Join(sentences, " ")
}

// isGroupTarget reports that the words mean every light rather than one saved
// name: an omitted device (the Agent's "all of them") or the explicit "all".
func isGroupTarget(device string) bool {
	device = strings.TrimSpace(device)
	return device == "" || strings.EqualFold(device, core.AllTarget)
}

// list reports the saved lights so the Agent can choose a device name. It is
// a read-only leaf operation: names and protocols only, never addresses.
func (m *Module) list() tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        "lights.list",
		Description: "List the saved BLE lights by name and protocol. Addresses are never returned; pass a name to lights.power, lights.brightness, lights.color or lights.mode. The PC / Monster light bar is not a saved BLE light and is controlled by the monster Tools.",
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
