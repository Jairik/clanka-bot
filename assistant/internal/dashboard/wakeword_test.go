package dashboard_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

// fakeWakewordServer answers every describe with the given model names.
func fakeWakewordServer(t *testing.T, models ...string) string {
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
				_, _ = fmt.Fprintf(conn, "%s\n", payload)
			}(conn)
		}
	}()
	return ln.Addr().String()
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
				{Name: "clanker", Threshold: 0.6},
				{Name: "okay_nabu", Threshold: 0.5},
			},
		},
	})

	var view struct {
		Wakeword struct {
			Enabled bool
			Addr    string
			Models  []struct{ Name, Threshold string }
			Note    string
		}
	}
	rec := getJSON(t, handler, "/api/ui/settings", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !view.Wakeword.Enabled || view.Wakeword.Addr != "wakeword:10400" {
		t.Fatalf("wakeword = %+v", view.Wakeword)
	}
	if len(view.Wakeword.Models) != 2 || view.Wakeword.Models[0].Name != "clanker" || view.Wakeword.Models[0].Threshold != "0.60" {
		t.Fatalf("models = %+v", view.Wakeword.Models)
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
	if len(info.Models) != 2 || info.Models[0].Name != "clanker" || info.Models[0].Threshold != 0.6 || info.Models[1].Threshold != 0.5 {
		t.Fatalf("models = %+v", info.Models)
	}
}
