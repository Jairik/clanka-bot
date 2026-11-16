package dashboard_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
)

// fakeWakewordServer answers every describe with the given model names.
func fakeWakewordServer(t *testing.T, models ...string) string {
	t.Helper()
	addr, _, _ := countingWakewordServer(t, models...)
	return addr
}

// countingWakewordServer also counts describes, so a test can prove one render
// asks the service once. The returned stop func closes the listener so a test
// can prove a stopped service is reported as an error rather than a cached ok.
func countingWakewordServer(t *testing.T, models ...string) (string, *atomic.Int32, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	advertised := make([]map[string]any, 0, len(models))
	for _, name := range models {
		advertised = append(advertised, map[string]any{"name": name, "phrase": name, "installed": true})
	}
	payload, err := json.Marshal(map[string]any{
		"type": "info",
		"data": map[string]any{
			"wake": []map[string]any{{"name": "openwakeword", "models": advertised}},
		},
	})
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}

	var describes atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				if _, err := r.ReadBytes('\n'); err != nil {
					return
				}
				describes.Add(1)
				_, _ = fmt.Fprintf(conn, "%s\n", payload)
			}(conn)
		}
	}()
	return ln.Addr().String(), &describes, func() { _ = ln.Close() }
}

func HealthWakewordRows(t *testing.T, handler *dashboard.Handler) []struct{ Name, Status, StatusLabel, Detail string } {
	t.Helper()
	var view struct {
		Wakeword []struct{ Name, Status, StatusLabel, Detail string }
	}
	rec := getJSON(t, handler, "/api/ui/health", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return view.Wakeword
}

func TestHealthShowsTheConfiguredWakeModels(t *testing.T) {
	addr := fakeWakewordServer(t, "clanker", "okay_nabu")
	handler := newHandler(t, dashboard.Options{
		Wakeword: dashboard.WakewordInfo{
			Addr: addr,
			Models: []dashboard.WakewordModel{
				{Name: "clanker", Threshold: 0.6},
				{Name: "okay_nabu", Threshold: 0.5},
			},
		},
	})

	rows := HealthWakewordRows(t, handler)
	if len(rows) != 2 {
		t.Fatalf("wakeword rows = %d, want 2: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Status != "ok" || row.StatusLabel != "served" {
			t.Errorf("%s = %s/%s, want ok/served", row.Name, row.Status, row.StatusLabel)
		}
	}
	if !strings.Contains(rows[0].Detail, "Threshold 0.60") {
		t.Errorf("clanker detail = %q, want its threshold", rows[0].Detail)
	}
	if !strings.Contains(rows[1].Detail, "Threshold 0.50") {
		t.Errorf("okay_nabu detail = %q, want its threshold", rows[1].Detail)
	}
}

func TestHealthFlagsAWakeModelTheServiceDoesNotServe(t *testing.T) {
	addr := fakeWakewordServer(t, "okay_nabu")
	handler := newHandler(t, dashboard.Options{
		Wakeword: dashboard.WakewordInfo{
			Addr:   addr,
			Models: []dashboard.WakewordModel{{Name: "clanker", Threshold: 0.5}},
		},
	})

	rows := HealthWakewordRows(t, handler)
	if len(rows) != 1 {
		t.Fatalf("wakeword rows = %d, want 1", len(rows))
	}
	if rows[0].Status != "error" || rows[0].StatusLabel != "not served" {
		t.Fatalf("row = %+v, want error/not served", rows[0])
	}
	if !strings.Contains(rows[0].Detail, "does not advertise") {
		t.Errorf("detail = %q", rows[0].Detail)
	}
}

func TestHealthReportsWakeModelProbeFailureAndDisabledVoice(t *testing.T) {
	// A listener that never answers leaves the describe call to the health
	// timeout; an address nobody listens on fails the dial immediately.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := dead.Addr().String()
	_ = dead.Close()

	handler := newHandler(t, dashboard.Options{
		HealthTimeout: 50 * time.Millisecond,
		Wakeword: dashboard.WakewordInfo{
			Addr:   addr,
			Models: []dashboard.WakewordModel{{Name: "clanker", Threshold: 0.5}},
		},
	})
	rows := HealthWakewordRows(t, handler)
	if len(rows) != 1 || rows[0].StatusLabel != "unknown" {
		t.Fatalf("rows = %+v, want one unknown row", rows)
	}

	off := newHandler(t, dashboard.Options{})
	rows = HealthWakewordRows(t, off)
	if len(rows) != 1 || rows[0].StatusLabel != "off" {
		t.Fatalf("rows with voice off = %+v, want one off row", rows)
	}
}

func TestSettingsShowsTheWakeModelsAndThresholds(t *testing.T) {
	handler := newHandler(t, dashboard.Options{
		Wakeword: dashboard.WakewordInfo{
			Addr: "wakeword:10400",
			Models: []dashboard.WakewordModel{
				{Name: "clanker", Threshold: 0.6, Enabled: true},
				{Name: "okay_nabu", Threshold: 0.5, Enabled: true},
				{Name: "volume_up", Threshold: 0.7, Command: true, Enabled: false},
			},
		},
	})

	var view struct {
		Wakeword struct {
			Enabled bool
			Addr    string
			Models  []struct {
				Name, Threshold  string
				Command, Enabled bool
			}
			Note string
		}
	}
	rec := getJSON(t, handler, "/api/ui/settings", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !view.Wakeword.Enabled || view.Wakeword.Addr != "wakeword:10400" {
		t.Fatalf("wakeword = %+v", view.Wakeword)
	}
	if len(view.Wakeword.Models) != 3 || view.Wakeword.Models[0].Name != "clanker" || view.Wakeword.Models[0].Threshold != "0.60" {
		t.Fatalf("models = %+v", view.Wakeword.Models)
	}
	if got := view.Wakeword.Models[2]; got.Name != "volume_up" || !got.Command || got.Enabled || got.Threshold != "0.70" {
		t.Errorf("command row = %+v, want volume_up command/off at 0.70", got)
	}
	if !strings.Contains(view.Wakeword.Note, "WAKEWORD_THRESHOLDS") {
		t.Errorf("note = %q, want the deployment knobs", view.Wakeword.Note)
	}
}

func TestWakewordFromConfigResolvesTheSameListTheVoiceLoopUses(t *testing.T) {
	cfg := config.Default()
	cfg.Wakeword.Addr = "wakeword:10400"
	cfg.Wakeword.Models = []string{"clanker", "okay_nabu"}
	cfg.Wakeword.Threshold = 0.5
	cfg.Wakeword.Thresholds = map[string]float64{"clanker": 0.6}

	info := dashboard.WakewordFromConfig(cfg)
	if info.Addr != "wakeword:10400" {
		t.Errorf("addr = %q", info.Addr)
	}
	if len(info.Models) != 4 {
		t.Fatalf("models = %+v, want two wake and two command models", info.Models)
	}
	if info.Models[0].Name != "clanker" || info.Models[0].Threshold != 0.6 || info.Models[0].Command || !info.Models[0].Enabled {
		t.Errorf("clanker = %+v", info.Models[0])
	}
	if info.Models[1].Name != "okay_nabu" || info.Models[1].Threshold != 0.5 {
		t.Errorf("okay_nabu = %+v", info.Models[1])
	}
	for _, model := range info.Models[2:] {
		if !model.Command || model.Enabled || model.Threshold != config.DefaultCommandThreshold {
			t.Errorf("command row = %+v, want disabled at the stricter default", model)
		}
	}
}

func TestWakewordFromConfigShowsEnabledCommandModels(t *testing.T) {
	cfg := config.Default()
	cfg.Wakeword.Addr = "wakeword:10400"
	cfg.Wakeword.Commands.Enabled = true
	cfg.Wakeword.Thresholds = map[string]float64{"volume_up": 0.8}

	info := dashboard.WakewordFromConfig(cfg)
	var up, down bool
	for _, model := range info.Models {
		switch model.Name {
		case "volume_up":
			up = model.Command && model.Enabled && model.Threshold == 0.8
		case "volume_down":
			down = model.Command && model.Enabled && model.Threshold == config.DefaultCommandThreshold
		}
	}
	if !up || !down {
		t.Fatalf("command rows = %+v, want both enabled with their thresholds", info.Models)
	}
}

func TestHealthShowsCommandModelsAndTheirEnabledState(t *testing.T) {
	addr := fakeWakewordServer(t, "clanker", "volume_up")
	handler := newHandler(t, dashboard.Options{
		Wakeword: dashboard.WakewordInfo{
			Addr: addr,
			Models: []dashboard.WakewordModel{
				{Name: "clanker", Threshold: 0.5, Enabled: true},
				{Name: "volume_up", Threshold: 0.7, Command: true, Enabled: true},
				{Name: "volume_down", Threshold: 0.7, Command: true, Enabled: false},
			},
		},
	})

	rows := HealthWakewordRows(t, handler)
	if len(rows) != 3 {
		t.Fatalf("wakeword rows = %d, want 3: %+v", len(rows), rows)
	}
	if rows[1].Status != "ok" || rows[1].StatusLabel != "served" || !strings.Contains(rows[1].Detail, "Wake-free command") {
		t.Errorf("enabled command row = %+v, want served and labelled", rows[1])
	}
	if rows[2].Status != "not-configured" || rows[2].StatusLabel != "off" || !strings.Contains(rows[2].Detail, "disabled") {
		t.Errorf("disabled command row = %+v, want off without a probe claim", rows[2])
	}
}

// TestOneHealthRenderDescribesTheWakewordServiceOnce is the #208 acceptance
// test: the Compose wakeword row and the model rows share one describe, so a
// page render asks the service once instead of twice.
func TestOneHealthRenderDescribesTheWakewordServiceOnce(t *testing.T) {
	addr, describes, _ := countingWakewordServer(t, "clanker")
	cfg := config.Default()
	cfg.Wakeword.Addr = addr
	cache := dashboard.NewWakeModelCache(addr, nil)

	var wakeCheck dashboard.ServiceCheck
	for _, link := range dashboard.ComposeServiceChecks(cfg, cache) {
		if link.Name == "wakeword" {
			wakeCheck = link
		}
	}
	if wakeCheck.Check == nil {
		t.Fatal("the wakeword Compose row is not wired for a configured address")
	}

	handler := newHandler(t, dashboard.Options{
		Services:   []dashboard.ServiceCheck{wakeCheck},
		WakeModels: cache,
		Wakeword: dashboard.WakewordInfo{
			Addr:   addr,
			Models: []dashboard.WakewordModel{{Name: "clanker", Threshold: 0.6}},
		},
	})

	rows := HealthWakewordRows(t, handler)
	if len(rows) != 1 || rows[0].Status != "ok" || rows[0].StatusLabel != "served" {
		t.Fatalf("wakeword rows = %+v, want one served row", rows)
	}
	if got := describes.Load(); got != 1 {
		t.Fatalf("describes = %d for one Health render, want 1", got)
	}
}

// manualClock is a controllable time source so a test can advance past the shared
// snapshot TTL without sleeping.
type manualClock struct {
	ptr atomic.Pointer[time.Time]
}

func newManualClock(start time.Time) *manualClock {
	c := &manualClock{}
	c.ptr.Store(&start)
	return c
}

func (c *manualClock) Now() time.Time { return *c.ptr.Load() }

func (c *manualClock) advance(d time.Duration) {
	t := c.Now().Add(d)
	c.ptr.Store(&t)
}

// healthServiceRow reads the Compose `Services` section of the Health JSON and
// returns the named row.
func healthServiceRow(t *testing.T, handler *dashboard.Handler, name string) struct {
	Name, Status, StatusLabel, Detail string
} {
	t.Helper()
	var view struct {
		Services []struct {
			Name, Status, StatusLabel, Detail string
		}
	}
	getJSON(t, handler, "/api/ui/health", &view)
	for _, s := range view.Services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("health Services has no row named %q: %+v", name, view.Services)
	return struct{ Name, Status, StatusLabel, Detail string }{}
}

// TestHealthWakewordRowProbesFreshNotCached is the #359 acceptance test: the
// Compose wakeword row is a fresh endpoint probe, so a stopped service is
// reported as an error on the next render (not a cached ok), and the row's
// Detail is the measured round trip, never a cache-hit time.
func TestHealthWakewordRowProbesFreshNotCached(t *testing.T) {
	addr, describes, stop := countingWakewordServer(t, "clanker")
	clock := newManualClock(time.Now())
	cfg := config.Default()
	cfg.Wakeword.Addr = addr
	cache := dashboard.NewWakeModelCache(addr, clock.Now)

	var wakeCheck dashboard.ServiceCheck
	for _, link := range dashboard.ComposeServiceChecks(cfg, cache) {
		if link.Name == "wakeword" {
			wakeCheck = link
		}
	}
	if wakeCheck.Check == nil {
		t.Fatal("the wakeword Compose row is not wired for a configured address")
	}
	handler := newHandler(t, dashboard.Options{
		Now:        clock.Now,
		Services:   []dashboard.ServiceCheck{wakeCheck},
		WakeModels: cache,
		Wakeword: dashboard.WakewordInfo{
			Addr:   addr,
			Models: []dashboard.WakewordModel{{Name: "clanker", Threshold: 0.6}},
		},
	})

	// First render with the service up: the row is ok, its Detail is the real
	// probe round trip, and exactly one describe happened.
	first := healthServiceRow(t, handler, "wakeword")
	if first.Status != "ok" {
		t.Fatalf("first wakeword row = %+v, want ok", first)
	}
	if !strings.Contains(first.Detail, "responded in") {
		t.Fatalf("first wakeword detail = %q, want the measured round trip", first.Detail)
	}
	if got := describes.Load(); got != 1 {
		t.Fatalf("describes = %d after one render, want 1", got)
	}

	// Stop the service and advance the clock past the shared snapshot TTL so the
	// next render re-probes instead of reusing the cached ok.
	stop()
	clock.advance(status.SharedSnapshotTTL + time.Second)

	second := healthServiceRow(t, handler, "wakeword")
	if second.Status == "ok" {
		t.Fatalf("second wakeword row = %+v, want an error after the service stopped", second)
	}
}
