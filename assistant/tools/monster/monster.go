// Package monster exposes Monster Smart Lighting controls to both assistant paths.
package monster

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lighting"
	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the Monster Smart Lighting domain.
type Module struct {
	Service *core.Service
	Names   *namematch.Matcher
}

// NewModule builds the module with no service until Register runs.
func NewModule() *Module { return &Module{} }

// Name is the module name and the secret-declaration domain.
func (*Module) Name() string { return "monster" }

// RequiredSecrets declares the Monster app account. The Secrets page lists
// both names automatically; the stored values are write-only.
func (*Module) RequiredSecrets() []string {
	return []string{core.SecretEmail, core.SecretPassword}
}

// In selects an optional target device and the operation's value.
type In struct {
	Device string `json:"device,omitempty"`
	Value  string `json:"value"`
	Scene  string `json:"scene,omitempty"`
}

// Out is the spoken acknowledgement for a completed Monster light command.
type Out struct {
	Reply string `json:"reply"`
}

var (
	// monsterTarget is the wording that names the PC / desk bar. The group
	// owner in tools/lights asks Targets() about the same pattern, so the two
	// transports cannot drift apart on who owns "pc light".
	monsterTarget = regexp.MustCompile(`(?i)(?:\b(?:monitor light|monitor lights|light bar|light bars|duo plus|duo|pc light|pc lights)\b|duo\+)`)
	powerPattern  = regexp.MustCompile(`(?i)\b(on|off)\b`)
	brightPattern = regexp.MustCompile(`\b(100|[0-9]{1,2})\b`)
	colorPattern  = regexp.MustCompile(`(?i)#[0-9a-f]{6}\b|\b(red|green|blue|white|yellow|orange|purple|pink|cyan|magenta)\b`)
	whiteWords    = regexp.MustCompile(`(?i)\b(warm\s*white|cool\s*white|warm|cool|daylight)\b`)
	kelvinPattern = regexp.MustCompile(`\b([2-6][0-9]{3}|7000)\s*(?:k|kelvin)?\b`)

	// barPattern recognises the monitor bar by name or model, so a generic
	// "pc light" resolves to the bar on an account that also holds strips. The
	// two model numbers are the Duo+ bar the PC light means.
	barPattern = regexp.MustCompile(`(?i)\b(?:duo|light\s*bar|monitor|mnw7-2027-icw|mlb7-2027-ww)\b`)

	sceneToSuffix = regexp.MustCompile(`(?i)\b(?:to|scene)\s+([a-z0-9 _-]+?)\s+scene$`)
	sceneToPrefix = regexp.MustCompile(`(?i)\bscene\s+to\s+([a-z0-9 _-]+)$`)
	sceneDirect   = regexp.MustCompile(`(?i)(?:\b(?:monitor light|light bar|duo plus|duo|pc lights?)\b|duo\+)\s+([a-z0-9 _-]+?)\s+scene$`)
	sceneTo       = regexp.MustCompile(`(?i)\bto\s+([a-z0-9 _-]+)$`)
)

var colorMap = map[string]string{
	"red":     "#ff0000",
	"green":   "#00ff00",
	"blue":    "#0000ff",
	"white":   "#ffffff",
	"yellow":  "#ffff00",
	"cyan":    "#00ffff",
	"magenta": "#ff00ff",
	"orange":  "#ff8000",
	"purple":  "#800080",
	"pink":    "#ffc0cb",
}

// Register builds the shared service and registers the 5 Monster tools.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.Names = deps.Names
	credentials := func() (string, string, bool) {
		if deps.Secrets == nil {
			return "", "", false
		}
		email, emailOK := deps.Secrets.Get(core.SecretEmail)
		password, passwordOK := deps.Secrets.Get(core.SecretPassword)
		if !emailOK || !passwordOK || strings.TrimSpace(email) == "" || password == "" {
			return "", "", false
		}
		return strings.TrimSpace(email), password, true
	}
	if m.Service == nil {
		m.Service = core.NewService(core.Options{
			API:         core.NewClient(),
			Credentials: credentials,
		})
	}

	for _, t := range m.tools() {
		if err := tool.Add(r, t); err != nil {
			return err
		}
	}
	return nil
}

// tools defines the five voice Tools for Monster lighting.
func (m *Module) tools() []tool.Tool[In, Out] {
	timeout := 8 * time.Second

	powerTool := tool.Tool[In, Out]{
		Name:        "monster.power",
		Description: "Set the PC / Monster light's power. Value: on or off. Device is a cloud device name, \"pc light\", \"monitor light\" or \"light bar\"; omit it when only one device is saved.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     timeout,
		Examples: []string{
			"turn the monitor light on",
			"turn off the light bar",
			"switch on the monitor light",
			"power off the light bar",
			"turn on Duo+",
			"power on the monitor light",
		},
		Speak: func(out Out) string { return out.Reply },
		Extract: func(text string) (In, bool) {
			text = strings.ToLower(text)
			if hasQuestionMarkers(text) {
				return In{}, false
			}
			dev, ok := m.extractDevice(text)
			if !ok {
				return In{}, false
			}
			v := powerPattern.FindString(text)
			if v == "" {
				return In{}, false
			}
			return In{Device: dev, Value: strings.ToLower(v)}, true
		},
		Handler: func(ctx context.Context, in In) (Out, error) {
			dev, err := m.resolveDevice(ctx, in.Device)
			if err != nil {
				return Out{}, err
			}
			v := strings.ToLower(strings.TrimSpace(in.Value))
			if v != "on" && v != "off" {
				return Out{}, &tool.ToolError{Say: "Power takes on or off."}
			}
			if err := m.Service.Apply(ctx, dev.DSN, "power", v); err != nil {
				return Out{}, toolError(err)
			}
			return Out{Reply: "Done."}, nil
		},
	}

	brightnessTool := tool.Tool[In, Out]{
		Name:        "monster.brightness",
		Description: "Set the PC / Monster light's brightness. Value: 0–100. Device is a cloud device name, \"pc light\", \"monitor light\" or \"light bar\"; omit it when only one device is saved.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     timeout,
		Examples: []string{
			"set light bar level to 50 percent",
			"turn the monitor light brightness to 75",
			"light bar dimmer 40 percent",
			"set light bar level to 60 percent",
			"dim Duo+ to 30 percent",
			"monitor light dimmer 80 percent",
		},
		Speak: func(out Out) string { return out.Reply },
		Extract: func(text string) (In, bool) {
			text = strings.ToLower(text)
			if hasQuestionMarkers(text) {
				return In{}, false
			}
			dev, ok := m.extractDevice(text)
			if !ok {
				return In{}, false
			}
			v := brightPattern.FindString(text)
			if v == "" {
				return In{}, false
			}
			return In{Device: dev, Value: v}, true
		},
		Handler: func(ctx context.Context, in In) (Out, error) {
			dev, err := m.resolveDevice(ctx, in.Device)
			if err != nil {
				return Out{}, err
			}
			val := strings.TrimSpace(strings.TrimSuffix(in.Value, "%"))
			val = strings.TrimSpace(strings.TrimSuffix(val, "percent"))
			n, convErr := strconv.Atoi(val)
			if convErr != nil || n < 0 || n > 100 {
				return Out{}, &tool.ToolError{Say: "Brightness takes 0 to 100."}
			}
			if err := m.Service.Apply(ctx, dev.DSN, "brightness", strconv.Itoa(n)); err != nil {
				return Out{}, toolError(err)
			}
			return Out{Reply: "Done."}, nil
		},
	}

	colorTool := tool.Tool[In, Out]{
		Name:        "monster.color",
		Description: "Set the PC / Monster light's color. Value: #RRGGBB or color name. Device is a cloud device name, \"pc light\", \"monitor light\" or \"light bar\"; omit it when only one device is saved.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     timeout,
		Examples: []string{
			"make the monitor light red",
			"set light bar to blue",
			"monitor light color green",
			"make the light bar purple",
			"change Duo+ color to orange",
			"set monitor light color to #ff00ff",
		},
		Speak: func(out Out) string { return out.Reply },
		Extract: func(text string) (In, bool) {
			text = strings.ToLower(text)
			if hasQuestionMarkers(text) {
				return In{}, false
			}
			dev, ok := m.extractDevice(text)
			if !ok {
				return In{}, false
			}
			v := colorPattern.FindString(text)
			if v == "" {
				return In{}, false
			}
			return In{Device: dev, Value: strings.ToLower(v)}, true
		},
		Handler: func(ctx context.Context, in In) (Out, error) {
			dev, err := m.resolveDevice(ctx, in.Device)
			if err != nil {
				return Out{}, err
			}
			val := strings.ToLower(strings.TrimSpace(in.Value))
			if hex, ok := colorMap[val]; ok {
				val = hex
			}
			if err := m.Service.Apply(ctx, dev.DSN, "color", val); err != nil {
				return Out{}, toolError(err)
			}
			return Out{Reply: "Done."}, nil
		},
	}

	whiteTool := tool.Tool[In, Out]{
		Name:        "monster.white",
		Description: "Set the PC / Monster light's white color temperature. Value: 2000–7000 kelvin, or warm, cool, daylight. Device is a cloud device name, \"pc light\", \"monitor light\" or \"light bar\"; omit it when only one device is saved.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     timeout,
		Examples: []string{
			"set the monitor light to warm white",
			"change light bar to cool white",
			"set monitor light to daylight white",
			"set light bar white to 4000 kelvin",
			"set Duo+ to 2700 kelvin",
			"turn the monitor light to 5000 kelvin",
		},
		Speak: func(out Out) string { return out.Reply },
		Extract: func(text string) (In, bool) {
			text = strings.ToLower(text)
			if hasQuestionMarkers(text) {
				return In{}, false
			}
			dev, ok := m.extractDevice(text)
			if !ok {
				return In{}, false
			}
			v := whiteWords.FindString(text)
			if v != "" {
				v = strings.ToLower(strings.Join(strings.Fields(v), " "))
				if v == "warm white" {
					v = "warm"
				}
				if v == "cool white" {
					v = "cool"
				}
				return In{Device: dev, Value: v}, true
			}
			kelvin := kelvinPattern.FindStringSubmatch(text)
			if len(kelvin) > 1 {
				return In{Device: dev, Value: kelvin[1]}, true
			}
			return In{}, false
		},
		Handler: func(ctx context.Context, in In) (Out, error) {
			dev, err := m.resolveDevice(ctx, in.Device)
			if err != nil {
				return Out{}, err
			}
			val := strings.ToLower(strings.TrimSpace(in.Value))
			if err := m.Service.Apply(ctx, dev.DSN, "white", val); err != nil {
				return Out{}, toolError(err)
			}
			return Out{Reply: "Done."}, nil
		},
	}

	sceneTool := tool.Tool[In, Out]{
		Name:        "monster.scene",
		Description: "Set the PC / Monster light's scene effect. Value: scene name. Device is a cloud device name, \"pc light\", \"monitor light\" or \"light bar\"; omit it when only one device is saved.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Timeout:     timeout,
		Examples: []string{
			"set monitor light to reading scene",
			"change light bar to party scene",
			"set the monitor light scene to relax",
			"switch light bar to dynamic scene",
			"set Duo+ scene to gaming",
			"change monitor light to movie scene",
		},
		Speak: func(out Out) string { return out.Reply },
		Extract: func(text string) (In, bool) {
			text = strings.ToLower(text)
			if hasQuestionMarkers(text) {
				return In{}, false
			}
			dev, ok := m.extractDevice(text)
			if !ok {
				return In{}, false
			}
			scene := extractSceneName(text)
			if scene == "" {
				return In{}, false
			}
			return In{Device: dev, Value: scene, Scene: scene}, true
		},
		Handler: func(ctx context.Context, in In) (Out, error) {
			dev, err := m.resolveDevice(ctx, in.Device)
			if err != nil {
				return Out{}, err
			}
			sceneVal := in.Value
			if in.Scene != "" && sceneVal == "" {
				sceneVal = in.Scene
			}
			eff, err := m.resolveScene(ctx, dev.DSN, sceneVal)
			if err != nil {
				return Out{}, err
			}
			if err := m.Service.Apply(ctx, dev.DSN, "effect", eff.ID); err != nil {
				return Out{}, toolError(err)
			}
			return Out{Reply: "Done."}, nil
		},
	}

	return []tool.Tool[In, Out]{powerTool, brightnessTool, colorTool, whiteTool, sceneTool}
}

// Targets reports whether the words name the PC / desk light. The group owner
// in tools/lights uses it so a BLE command never claims "pc light",
// "monitor light" or "light bar".
func (m *Module) Targets(text string) bool { return monsterTarget.MatchString(text) }

// groupBudget bounds one general command's cloud fan-out. Both transports run
// in parallel and the BLE batch budget is fourteen seconds, so the slower side
// must still answer inside the Tool's own fifteen-second deadline.
const groupBudget = 8 * time.Second

// ApplyEvery applies op to every light on the account for a general lighting
// command ("turn the lights off"). No account, or an account with no lights,
// is zero lights and no error: a general command skips the PC side silently. A
// rejected sign-in or an unreachable cloud is an error, so the reply can say
// the PC light did not answer instead of pretending the bar was never there.
func (m *Module) ApplyEvery(ctx context.Context, op, value string) lighting.Result {
	if m == nil || m.Service == nil {
		return lighting.Result{}
	}
	ctx, cancel := context.WithTimeout(ctx, groupBudget)
	defer cancel()
	devices, err := m.Service.Devices(ctx)
	if err != nil {
		if errors.Is(err, core.ErrNotConfigured) {
			return lighting.Result{}
		}
		return lighting.Result{Err: toolError(err)}
	}
	out := lighting.Result{Total: len(devices)}
	for _, d := range devices {
		if err := m.Service.Apply(ctx, d.DSN, op, value); err != nil {
			if out.Err == nil {
				out.Err = toolError(err)
			}
			continue
		}
		out.Applied++
	}
	return out
}

// extractDevice resolves whether the text mentions a Monster target or specific device.
func (m *Module) extractDevice(text string) (string, bool) {
	// Check if text matches a known cloud device name from the 60s cache. The
	// lookup gets its own short budget: a cold sign-in may run for the client's
	// whole request timeout, and Extract runs before the Tool's deadline
	// starts. A miss falls back to the static aliases below.
	if m != nil && m.Service != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		devs, err := m.Service.Devices(ctx)
		cancel()
		if err == nil {
			for _, d := range devs {
				if d.Name != "" && strings.Contains(strings.ToLower(text), strings.ToLower(d.Name)) {
					return d.Name, true
				}
			}
		}
	}
	// Check if text addresses monitor light or light bar generically
	if monsterTarget.MatchString(text) {
		return "", true
	}
	return "", false
}

// pcBar picks the account's monitor bar when exactly one device looks like it.
// The Duo+ models (MNW7-2027-ICW, MLB7-2027-WW) are the PC light a general
// command means; an account with two bars is ambiguous and stays unresolved.
func pcBar(devices []core.Device) (core.Device, bool) {
	var found core.Device
	matches := 0
	for _, d := range devices {
		if barPattern.MatchString(d.Name) || barPattern.MatchString(d.Model) {
			found = d
			matches++
		}
	}
	return found, matches == 1
}

// resolveDevice matches a spoken or requested device name against the account's devices.
func (m *Module) resolveDevice(ctx context.Context, spoken string) (core.Device, error) {
	if m == nil || m.Service == nil {
		return core.Device{}, &tool.ToolError{Say: "Monster lights are not configured."}
	}
	devices, err := m.Service.Devices(ctx)
	if err != nil {
		return core.Device{}, toolError(err)
	}
	if len(devices) == 0 {
		return core.Device{}, &tool.ToolError{Say: "The Monster account has no lights."}
	}

	spoken = strings.TrimSpace(spoken)
	if spoken == "" {
		if len(devices) == 1 {
			return devices[0], nil
		}
		// "The PC light" / "the monitor light" names the bar, not whichever
		// device the account lists first: pick the one bar-like device when the
		// account also holds strips or bulbs.
		if bar, ok := pcBar(devices); ok {
			return bar, nil
		}
		return core.Device{}, &tool.ToolError{Say: "Specify which Monster light to control."}
	}

	// Exact or case-insensitive match on name or DSN
	for _, d := range devices {
		if strings.EqualFold(d.Name, spoken) || strings.EqualFold(d.DSN, spoken) {
			return d, nil
		}
	}

	// Semantic namematch against device names
	if m.Names != nil {
		names := make([]string, len(devices))
		for i, d := range devices {
			names[i] = d.Name
		}
		res := m.Names.Match(ctx, namematch.SourceLights, spoken, names)
		if res.Matched() {
			for _, d := range devices {
				if d.Name == res.Name {
					return d, nil
				}
			}
		}
		if res.Near != "" {
			return core.Device{}, &tool.ToolError{Say: namematch.NearQuestion(res.Near, "light")}
		}
	}

	return core.Device{}, &tool.ToolError{Say: fmt.Sprintf("I couldn't find a Monster light called %s.", spoken)}
}

// resolveScene matches a spoken scene name against the device's populated scene slots.
func (m *Module) resolveScene(ctx context.Context, dsn, spoken string) (core.Effect, error) {
	if m == nil || m.Service == nil {
		return core.Effect{}, &tool.ToolError{Say: "Monster lights are not configured."}
	}
	effs, err := m.Service.Effects(ctx, dsn)
	if err != nil {
		return core.Effect{}, toolError(err)
	}
	if len(effs) == 0 {
		return core.Effect{}, &tool.ToolError{Say: "That light has no saved scenes."}
	}

	spoken = strings.TrimSpace(spoken)
	if spoken == "" {
		return core.Effect{}, &tool.ToolError{Say: "Which scene should I set?"}
	}

	// Direct match if spoken is an exact Effect ID (e.g. "dynamic:dyn_pat:2")
	for _, eff := range effs {
		if eff.ID == spoken {
			return eff, nil
		}
	}

	// Prepare candidate names and lookup map
	candidates := make([]string, 0, len(effs)*2)
	byCandidate := make(map[string]core.Effect)
	for _, eff := range effs {
		candidates = append(candidates, eff.Name)
		byCandidate[strings.ToLower(eff.Name)] = eff

		// If name is "Dynamic: Sunset", also index "Sunset"
		if idx := strings.Index(eff.Name, ": "); idx != -1 {
			short := eff.Name[idx+2:]
			candidates = append(candidates, short)
			byCandidate[strings.ToLower(short)] = eff
		}
	}

	// Case-insensitive direct match
	if eff, ok := byCandidate[strings.ToLower(spoken)]; ok {
		return eff, nil
	}

	// Namematch
	if m.Names != nil {
		res := m.Names.Match(ctx, namematch.SourceLights, spoken, candidates)
		if res.Matched() {
			if eff, ok := byCandidate[strings.ToLower(res.Name)]; ok {
				return eff, nil
			}
		}
		if res.Near != "" {
			return core.Effect{}, &tool.ToolError{Say: namematch.NearQuestion(res.Near, "scene")}
		}
	}

	return core.Effect{}, &tool.ToolError{Say: fmt.Sprintf("I couldn't find a scene called %s on that light.", spoken)}
}

// extractSceneName extracts the candidate scene phrase from spoken text.
func extractSceneName(text string) string {
	text = strings.TrimSpace(text)
	if m := sceneToSuffix.FindStringSubmatch(text); len(m) > 1 {
		return cleanSceneName(m[1])
	}
	if m := sceneToPrefix.FindStringSubmatch(text); len(m) > 1 {
		return cleanSceneName(m[1])
	}
	if m := sceneDirect.FindStringSubmatch(text); len(m) > 1 {
		return cleanSceneName(m[1])
	}
	if strings.Contains(strings.ToLower(text), "scene") {
		if m := sceneTo.FindStringSubmatch(text); len(m) > 1 {
			return cleanSceneName(m[1])
		}
	}
	return ""
}

// cleanSceneName strips extraneous articles and the word "scene" from an extracted candidate.
func cleanSceneName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "the ")
	s = strings.TrimPrefix(s, "a ")
	s = strings.TrimPrefix(s, "an ")
	s = strings.TrimSuffix(s, " scene")
	s = strings.TrimSpace(s)
	if s == "scene" {
		return ""
	}
	return s
}

// hasQuestionMarkers reports whether the utterance is a question or compound command.
func hasQuestionMarkers(text string) bool {
	for _, marker := range []string{"how do", "how can", "why ", "what ", "explain ", " and "} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// toolError maps cloud and validation failures to spoken tool errors.
func toolError(err error) *tool.ToolError {
	if errors.Is(err, core.ErrCredentials) || errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUpstream) || errors.Is(err, context.DeadlineExceeded) {
		return &tool.ToolError{Say: core.Friendly(err)}
	}
	return &tool.ToolError{Say: err.Error()}
}

// AuxHealth reports the account and cloud reachability as one row. Missing
// credentials are a not-configured capability, not a broken module.
func (m *Module) AuxHealth(ctx context.Context) []tool.AuxHealth {
	if m == nil || m.Service == nil {
		return nil
	}
	configured, ok, detail := m.Service.Health(ctx)
	state := tool.StatusOK
	switch {
	case !configured:
		state = tool.StatusNotConfigured
	case !ok:
		state = tool.StatusError
	}
	return []tool.AuxHealth{{Name: "Monster lights", State: state, Detail: detail}}
}
