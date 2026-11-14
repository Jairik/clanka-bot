package monster

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credentials returns the owner's Monster app email and password. The module
// adapts the write-only Secrets store to this shape; tests supply a closure.
type Credentials func() (email, password string, ok bool)

// Options build one Service. API is required; Credentials may be nil, which
// keeps the page in its not-configured state.
type Options struct {
	API         API
	Credentials Credentials
	Now         func() time.Time
}

// Service is the Dashboard and Health view over one Monster account. It has no
// background loop: the page reads on navigation and Health caches its one
// lookup briefly.
type Service struct {
	api  API
	cred Credentials
	now  func() time.Time

	mu     sync.Mutex
	health healthSnapshot
}

type healthSnapshot struct {
	at         time.Time
	configured bool
	ok         bool
	detail     string
}

// healthTTL keeps the Dashboard's health poll from turning into a cloud poll.
const healthTTL = 60 * time.Second

// NewService builds the shared view. A nil Now uses the real clock.
func NewService(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{api: opts.API, cred: opts.Credentials, now: now}
}

// Effect is one built-in scene slot the device exposes. The ID is opaque to
// the page and names the mode, selector property and slot.
type Effect struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DeviceView is one light plus its last cloud state. Error is set per device so
// one unreachable light does not hide the rest.
type DeviceView struct {
	DSN        string   `json:"dsn"`
	Name       string   `json:"name"`
	Model      string   `json:"model"`
	On         bool     `json:"on"`
	Mode       string   `json:"mode"`
	Brightness int      `json:"brightness"`
	Color      string   `json:"color"`
	Kelvin     int      `json:"kelvin"`
	Effects    []Effect `json:"effects"`
	Error      string   `json:"error,omitempty"`
}

// View is the whole Monster page. Configured false carries the setup line in
// Status and no devices; a configured account that cannot be read carries the
// cause in Error, so the page can show it as an error rather than a quiet
// status.
type View struct {
	Configured bool         `json:"configured"`
	Status     string       `json:"status"`
	Error      string       `json:"error,omitempty"`
	Devices    []DeviceView `json:"devices"`
}

// Configured reports whether both secrets are present.
func (s *Service) Configured() bool {
	if s == nil || s.cred == nil {
		return false
	}
	_, _, ok := s.cred()
	return ok
}

// View reads every device and its properties. Upstream failures become the
// page's Status instead of an HTTP error: the page stays mounted and says what
// to fix.
func (s *Service) View(ctx context.Context) View {
	v := View{Devices: []DeviceView{}}
	email, password, ok := s.credentials()
	if !ok {
		v.Status = "Add MONSTER_EMAIL and MONSTER_PASSWORD on the Secrets page."
		return v
	}
	v.Configured = true
	devices, err := s.api.Devices(ctx, email, password)
	if err != nil {
		v.Error = Friendly(err)
		return v
	}
	if len(devices) == 0 {
		v.Status = "The Monster account has no lights."
		return v
	}
	v.Status = fmt.Sprintf("%d %s", len(devices), plural(len(devices), "light", "lights"))
	for _, device := range devices {
		dv := DeviceView{DSN: device.DSN, Name: device.Name, Model: device.Model, Color: "#ffffff", Effects: []Effect{}}
		props, err := s.api.Properties(ctx, email, password, device.DSN)
		if err != nil {
			dv.Error = Friendly(err)
			v.Devices = append(v.Devices, dv)
			continue
		}
		v.Devices = append(v.Devices, deviceView(dv, props))
	}
	return v
}

// Apply resolves a saved cloud device by DSN, then writes one property group.
// The DSN must be on the account: a page request can never reach an arbitrary
// Ayla path.
func (s *Service) Apply(ctx context.Context, dsn, action, value string) error {
	email, password, ok := s.credentials()
	if !ok {
		return ErrNotConfigured
	}
	devices, err := s.api.Devices(ctx, email, password)
	if err != nil {
		return err
	}
	found := false
	for _, device := range devices {
		if device.DSN == dsn {
			found = true
			break
		}
	}
	if !found {
		return errors.New("choose a Monster light")
	}
	switch action {
	case "power":
		switch value {
		case "on":
			return s.api.Set(ctx, email, password, dsn, "power", 1)
		case "off":
			return s.api.Set(ctx, email, password, dsn, "power", 0)
		}
		return errors.New("power takes on or off")
	case "brightness":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 100 {
			return errors.New("brightness takes 0 to 100")
		}
		props, err := s.api.Properties(ctx, email, password, dsn)
		if err != nil {
			return err
		}
		// White mode drives the white dimmer; colour and scene modes drive the
		// colour dimmer. Writing both would desynchronise the two stored levels.
		name := "color_bright"
		if stringProp(props, "mode") == "white" {
			name = "brightness"
		}
		if _, ok := props[name]; !ok {
			name = "color_bright"
		}
		return s.api.Set(ctx, email, password, dsn, name, n)
	case "color":
		rgb, err := parseHexColor(value)
		if err != nil {
			return err
		}
		if err := s.api.Set(ctx, email, password, dsn, "mode", "color"); err != nil {
			return err
		}
		return s.api.Set(ctx, email, password, dsn, "color_select", rgb)
	case "white":
		kelvin, err := strconv.Atoi(value)
		if err != nil || kelvin < 2000 || kelvin > 7000 {
			return errors.New("white takes 2000 to 7000 kelvin")
		}
		if err := s.api.Set(ctx, email, password, dsn, "mode", "white"); err != nil {
			return err
		}
		return s.api.Set(ctx, email, password, dsn, "color_temp", kelvinToCT(kelvin))
	case "effect":
		mode, prop, index, err := parseEffectID(value)
		if err != nil {
			return err
		}
		if err := s.api.Set(ctx, email, password, dsn, "mode", mode); err != nil {
			return err
		}
		return s.api.Set(ctx, email, password, dsn, prop, index)
	}
	return fmt.Errorf("unknown Monster action %q", action)
}

// Health is the one row the Health page shows. It caches for a minute so a
// polling page does not re-authenticate on every render.
func (s *Service) Health(ctx context.Context) (configured, ok bool, detail string) {
	if s == nil {
		return false, false, "Monster lights are not wired into this assistant build."
	}
	email, password, has := s.credentials()
	if !has {
		return false, false, "Add MONSTER_EMAIL and MONSTER_PASSWORD on the Secrets page."
	}
	s.mu.Lock()
	cache := s.health
	s.mu.Unlock()
	if cache.configured && s.now().Sub(cache.at) < healthTTL {
		return true, cache.ok, cache.detail
	}
	devices, err := s.api.Devices(ctx, email, password)
	ok, detail = true, ""
	if err != nil {
		ok, detail = false, Friendly(err)
	} else {
		detail = fmt.Sprintf("%d %s", len(devices), plural(len(devices), "light", "lights"))
	}
	s.mu.Lock()
	s.health = healthSnapshot{at: s.now(), configured: true, ok: ok, detail: detail}
	s.mu.Unlock()
	return true, ok, detail
}

// Friendly turns an error into one page-sized sentence. It never includes an
// upstream body or the credentials.
func Friendly(err error) string {
	switch {
	case errors.Is(err, ErrCredentials):
		return "Monster rejected the email or password. Replace them on the Secrets page."
	case errors.Is(err, ErrNotFound):
		return "The Monster cloud does not know that light any more."
	case errors.Is(err, context.DeadlineExceeded):
		return "Monster did not answer in time."
	case errors.Is(err, ErrUpstream):
		var status *StatusError
		if errors.As(err, &status) {
			return fmt.Sprintf("Monster answered HTTP %d.", status.Code)
		}
		return "Monster did not answer."
	case errors.Is(err, ErrNotConfigured):
		return ErrNotConfigured.Error()
	}
	return "Monster request failed."
}

func (s *Service) credentials() (string, string, bool) {
	if s == nil || s.cred == nil {
		return "", "", false
	}
	email, password, ok := s.cred()
	email, password = strings.TrimSpace(email), strings.TrimSpace(password)
	return email, password, ok && email != "" && password != ""
}

func deviceView(d DeviceView, props map[string]any) DeviceView {
	d.On = intProp(props, "power") != 0
	d.Mode = stringProp(props, "mode")
	if n, ok := intPropOK(props, "color_bright"); ok {
		d.Brightness = clamp(n, 0, 100)
	} else if n, ok := intPropOK(props, "brightness"); ok {
		d.Brightness = clamp(n, 0, 100)
	}
	if packed, ok := intPropOK(props, "color_select"); ok {
		d.Color = fmt.Sprintf("#%02x%02x%02x", (packed>>16)&0xff, (packed>>8)&0xff, packed&0xff)
	}
	if ct, ok := intPropOK(props, "color_temp"); ok {
		d.Kelvin = ctToKelvin(ct)
	}
	d.Effects = effects(props)
	return d
}

// effectFamilies are the device's scene banks. The mode value is what the
// firmware accepts; DIY is uppercase in the app and on the device.
var effectFamilies = []struct {
	Mode  string
	Slot  string
	Pat   string
	Label string
}{
	{"static", "st", "st_pat", "Static"},
	{"dynamic", "dyn", "dyn_pat", "Dynamic"},
	{"music", "mus", "mus_pat", "Music"},
	{"DIY", "diy", "diy_pat", "DIY"},
	{"per_ic", "pic", "per_ic_pat", "Per-IC"},
}

// effects reads the device's own populated scene slots, so labels match what
// the Monster app shows and a renamed DIY slot keeps its name.
func effects(props map[string]any) []Effect {
	out := []Effect{}
	for _, family := range effectFamilies {
		for index := 0; index < 16; index++ {
			raw := stringProp(props, fmt.Sprintf("%s%02d", family.Slot, index))
			if raw == "" {
				continue
			}
			var slot struct {
				Name string `json:"n"`
			}
			if json.Unmarshal([]byte(raw), &slot) != nil || slot.Name == "" {
				continue
			}
			out = append(out, Effect{
				ID:   fmt.Sprintf("%s:%s:%d", family.Mode, family.Pat, index),
				Name: family.Label + ": " + slot.Name,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func parseEffectID(id string) (mode, prop string, index int, err error) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 {
		return "", "", 0, errors.New("choose a scene")
	}
	for _, family := range effectFamilies {
		if family.Mode == parts[0] && family.Pat == parts[1] {
			n, convErr := strconv.Atoi(parts[2])
			if convErr != nil || n < 0 || n > 15 {
				return "", "", 0, errors.New("choose a scene")
			}
			return family.Mode, family.Pat, n, nil
		}
	}
	return "", "", 0, errors.New("choose a scene")
}

func parseHexColor(value string) (int, error) {
	raw := strings.TrimPrefix(value, "#")
	if len(raw) != 6 {
		return 0, errors.New("colour takes #rrggbb")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return 0, errors.New("colour takes #rrggbb")
	}
	return int(b[0])<<16 | int(b[1])<<8 | int(b[2]), nil
}

// ctToKelvin maps the device's 0-100 white position onto the tunable-white
// span the Monster app exposes; 0 is the warmest end.
func ctToKelvin(ct int) int {
	ct = clamp(ct, 0, 100)
	return 2700 + ct*(6500-2700)/100
}

func kelvinToCT(kelvin int) int {
	kelvin = clamp(kelvin, 2700, 6500)
	return (kelvin - 2700) * 100 / (6500 - 2700)
}

func intProp(props map[string]any, name string) int {
	n, _ := intPropOK(props, name)
	return n
}

func intPropOK(props map[string]any, name string) (int, bool) {
	raw, ok := props[name]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "true" {
			return 1, true
		}
		if s == "false" {
			return 0, true
		}
		n, err := strconv.Atoi(s)
		return n, err == nil
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func stringProp(props map[string]any, name string) string {
	raw, ok := props[name]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.Itoa(int(v))
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
