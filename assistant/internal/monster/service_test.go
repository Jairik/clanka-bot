package monster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type setCall struct {
	dsn, name string
	value     any
}

type fakeAPI struct {
	devices    []Device
	properties map[string]map[string]any
	devicesErr error
	propsErr   map[string]error
	setErr     error

	mu       sync.Mutex
	setCalls []setCall
	devCalls int
}

func (f *fakeAPI) Devices(context.Context, string, string) ([]Device, error) {
	f.mu.Lock()
	f.devCalls++
	f.mu.Unlock()
	return f.devices, f.devicesErr
}

func (f *fakeAPI) Properties(_ context.Context, _, _, dsn string) (map[string]any, error) {
	if err := f.propsErr[dsn]; err != nil {
		return nil, err
	}
	return f.properties[dsn], nil
}

func (f *fakeAPI) Set(_ context.Context, _, _, dsn, name string, value any) error {
	f.mu.Lock()
	f.setCalls = append(f.setCalls, setCall{dsn: dsn, name: name, value: value})
	f.mu.Unlock()
	return f.setErr
}

func (f *fakeAPI) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setCall(nil), f.setCalls...)
}

func (f *fakeAPI) deviceCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devCalls
}

func configured(api API) *Service {
	return NewService(Options{
		API:         api,
		Credentials: func() (string, string, bool) { return "owner@example.com", "secret", true },
	})
}

func TestViewWithoutCredentialsPointsAtSecrets(t *testing.T) {
	v := NewService(Options{API: &fakeAPI{}}).View(context.Background())
	if v.Configured || len(v.Devices) != 0 || !strings.Contains(v.Status, "MONSTER_EMAIL") {
		t.Fatalf("view = %+v", v)
	}
}

func TestViewMapsTheDeviceState(t *testing.T) {
	api := &fakeAPI{
		devices: []Device{{DSN: "DSN1", Name: "Duo+", Model: "MNW7-2027-ICW"}},
		properties: map[string]map[string]any{"DSN1": {
			"power":        1,
			"mode":         "white",
			"brightness":   40,
			"color_bright": 35,
			"color_temp":   50,
			"color_select": 0xff0000,
			"st00":         `{"n":"Candle"}`,
			"dyn01":        `{"n":"Rainbow"}`,
			"diy00":        `not json`,
			"pic03":        `{"n":"Stripes"}`,
		}},
	}
	v := configured(api).View(context.Background())
	if !v.Configured || v.Status != "1 light" || len(v.Devices) != 1 {
		t.Fatalf("view = %+v", v)
	}
	d := v.Devices[0]
	if !d.On || d.Mode != "white" || d.Brightness != 35 || d.Color != "#ff0000" {
		t.Fatalf("device = %+v", d)
	}
	if d.Kelvin != 4600 { // 2700 + 50% of the 2700..6500 span
		t.Fatalf("kelvin = %d", d.Kelvin)
	}
	if len(d.Effects) != 3 {
		t.Fatalf("effects = %+v", d.Effects)
	}
	if d.Effects[0].Name != "Dynamic: Rainbow" || d.Effects[2].Name != "Static: Candle" {
		t.Fatalf("effects are not sorted by label: %+v", d.Effects)
	}
}

func TestViewKeepsOneBrokenDevice(t *testing.T) {
	api := &fakeAPI{
		devices: []Device{{DSN: "DSN1", Name: "Duo+"}, {DSN: "DSN2", Name: "Strip"}},
		properties: map[string]map[string]any{
			"DSN1": {"power": 1},
		},
		propsErr: map[string]error{"DSN2": &StatusError{Code: 503}},
	}
	v := configured(api).View(context.Background())
	if len(v.Devices) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if v.Devices[0].Error != "" || v.Devices[1].Error == "" {
		t.Fatalf("per-device errors = %+v", v.Devices)
	}
}

func TestApplyPower(t *testing.T) {
	api := &fakeAPI{devices: []Device{{DSN: "DSN1"}}}
	if err := configured(api).Apply(context.Background(), "DSN1", "power", "on"); err != nil {
		t.Fatal(err)
	}
	calls := api.calls()
	if len(calls) != 1 || calls[0].name != "power" || calls[0].value != 1 {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestApplyBrightnessUsesTheModeDimmer(t *testing.T) {
	api := &fakeAPI{
		devices:    []Device{{DSN: "DSN1"}},
		properties: map[string]map[string]any{"DSN1": {"mode": "white", "brightness": 10, "color_bright": 90}},
	}
	if err := configured(api).Apply(context.Background(), "DSN1", "brightness", "55"); err != nil {
		t.Fatal(err)
	}
	calls := api.calls()
	if len(calls) != 1 || calls[0].name != "brightness" || calls[0].value != 55 {
		t.Fatalf("white dimmer calls = %+v", calls)
	}

	api = &fakeAPI{
		devices:    []Device{{DSN: "DSN1"}},
		properties: map[string]map[string]any{"DSN1": {"mode": "color", "brightness": 10, "color_bright": 90}},
	}
	if err := configured(api).Apply(context.Background(), "DSN1", "brightness", "55"); err != nil {
		t.Fatal(err)
	}
	if calls := api.calls(); calls[len(calls)-1].name != "color_bright" {
		t.Fatalf("colour dimmer calls = %+v", calls)
	}
}

func TestApplyColorWhiteAndEffect(t *testing.T) {
	api := &fakeAPI{devices: []Device{{DSN: "DSN1"}}}
	svc := configured(api)
	if err := svc.Apply(context.Background(), "DSN1", "color", "#00ff00"); err != nil {
		t.Fatal(err)
	}
	if calls := api.calls(); calls[0].name != "mode" || calls[1].name != "color_select" || calls[1].value != 0x00ff00 {
		t.Fatalf("colour calls = %+v", calls)
	}

	api.setCalls = nil
	if err := svc.Apply(context.Background(), "DSN1", "white", "4000"); err != nil {
		t.Fatal(err)
	}
	calls := api.calls()
	if calls[0].name != "mode" || calls[0].value != "white" || calls[1].name != "color_temp" {
		t.Fatalf("white calls = %+v", calls)
	}
	wantCT := kelvinToCT(4000)
	if calls[1].value != wantCT {
		t.Fatalf("color_temp = %v, want %d", calls[1].value, wantCT)
	}

	api.setCalls = nil
	if err := svc.Apply(context.Background(), "DSN1", "effect", "dynamic:dyn_pat:2"); err != nil {
		t.Fatal(err)
	}
	calls = api.calls()
	if calls[0].value != "dynamic" || calls[1].name != "dyn_pat" || calls[1].value != 2 {
		t.Fatalf("effect calls = %+v", calls)
	}
}

func TestViewReportsACloudErrorInTheErrorField(t *testing.T) {
	v := configured(&fakeAPI{devicesErr: ErrCredentials}).View(context.Background())
	if !v.Configured || v.Error == "" || v.Status != "" || len(v.Devices) != 0 {
		t.Fatalf("view = %+v", v)
	}
	if !strings.Contains(v.Error, "rejected") {
		t.Fatalf("error = %q", v.Error)
	}
}

func TestApplyRejectsForeignDeviceAndBadValues(t *testing.T) {
	svc := configured(&fakeAPI{devices: []Device{{DSN: "DSN1"}}})
	for _, tc := range []struct{ dsn, action, value string }{
		{"OTHER", "power", "on"},
		{"DSN1", "power", "maybe"},
		{"DSN1", "brightness", "101"},
		{"DSN1", "color", "red"},
		{"DSN1", "white", "1000"},
		{"DSN1", "effect", "nope"},
		{"DSN1", "dance", "now"},
	} {
		if err := svc.Apply(context.Background(), tc.dsn, tc.action, tc.value); err == nil {
			t.Errorf("%+v was accepted", tc)
		}
	}
}

func TestApplyWithoutCredentials(t *testing.T) {
	svc := NewService(Options{API: &fakeAPI{}})
	if err := svc.Apply(context.Background(), "DSN1", "power", "on"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestHealthCachesTheCloudLookup(t *testing.T) {
	api := &fakeAPI{devices: []Device{{DSN: "DSN1"}, {DSN: "DSN2"}}}
	now := time.Unix(0, 0)
	svc := NewService(Options{
		API:         api,
		Credentials: func() (string, string, bool) { return "owner@example.com", "secret", true },
		Now:         func() time.Time { return now },
	})
	configured, ok, detail := svc.Health(context.Background())
	if !configured || !ok || detail != "2 lights" {
		t.Fatalf("health = %v %v %q", configured, ok, detail)
	}
	now = now.Add(30 * time.Second)
	svc.Health(context.Background())
	if got := api.deviceCalls(); got != 1 {
		t.Fatalf("device calls = %d, want the cached result", got)
	}
	now = now.Add(31 * time.Second)
	svc.Health(context.Background())
	if got := api.deviceCalls(); got != 2 {
		t.Fatalf("device calls = %d, want a refresh after the TTL", got)
	}
}

func TestHealthReportsACloudError(t *testing.T) {
	svc := configured(&fakeAPI{devicesErr: ErrCredentials})
	configured, ok, detail := svc.Health(context.Background())
	if !configured || ok || !strings.Contains(detail, "rejected") {
		t.Fatalf("health = %v %v %q", configured, ok, detail)
	}
}

func TestHealthWithoutSecrets(t *testing.T) {
	configured, ok, detail := NewService(Options{API: &fakeAPI{}}).Health(context.Background())
	if configured || ok || !strings.Contains(detail, "MONSTER_EMAIL") {
		t.Fatalf("health = %v %v %q", configured, ok, detail)
	}
}
