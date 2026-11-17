package system

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestSummarizeAllHealthy(t *testing.T) {
	report := tool.StatusReport{
		CheckedAt: time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC),
		Timeout:   time.Second,
		Services: []tool.StatusCheck{
			{Name: "embed", State: tool.StatusOK, Detail: "responded in 1ms"},
			{Name: "assistant", State: tool.StatusOK, Detail: "responded in under 1ms"},
			{Name: "tts", State: tool.StatusNotConfigured},
		},
		Location: tool.StatusCheck{Name: "location", State: tool.StatusOK, Detail: "Bristol"},
		Modules: []tool.StatusCheck{
			{Name: "weather", State: tool.StatusOK},
			{Name: "clock", State: tool.StatusNoCheck},
		},
	}
	out := summarize(report)
	if !out.OK {
		t.Fatalf("OK = false for a healthy report: %+v", out)
	}
	if out.Summary != "I'm OK. Everything I can check is responding." {
		t.Errorf("Summary = %q", out.Summary)
	}
	// The model-visible result never carries the free-text detail, which may
	// hold an address or an upstream error.
	if len(out.Services) != 3 || out.Services[0].State != string(tool.StatusOK) {
		t.Errorf("Services = %+v", out.Services)
	}
	if out.Location == nil || out.Location.Name != "location" {
		t.Errorf("Location = %+v", out.Location)
	}
}

func TestSummarizeNamesOnlyFailingChecks(t *testing.T) {
	report := tool.StatusReport{
		Services: []tool.StatusCheck{
			{Name: "embed", State: tool.StatusTimedOut, Detail: "No response within 3s."},
			{Name: "assistant", State: tool.StatusOK},
		},
		Modules: []tool.StatusCheck{
			{Name: "weather", State: tool.StatusError, Detail: "upstream 10.0.0.5 refused"},
			{Name: "clock", State: tool.StatusNoCheck},
		},
	}
	out := summarize(report)
	if out.OK {
		t.Fatal("OK = true with failing checks")
	}
	if !strings.Contains(out.Summary, "embed") || !strings.Contains(out.Summary, "weather") {
		t.Errorf("Summary = %q, want both failing names", out.Summary)
	}
	if strings.Contains(out.Summary, "10.0.0.5") {
		t.Errorf("Summary leaks the internal detail: %q", out.Summary)
	}
	if strings.Contains(out.Summary, "assistant") {
		t.Errorf("Summary names a healthy check: %q", out.Summary)
	}
}

func TestSummarizeWithNoChecksIsNotAHealthClaim(t *testing.T) {
	out := summarize(tool.StatusReport{})
	if out.OK {
		t.Fatal("OK = true with no evidence")
	}
	if out.Summary != "I can't check my services right now." {
		t.Errorf("Summary = %q", out.Summary)
	}
}

func TestNilStatusSeamFailsWithoutFabricatingHealth(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := reg.Execute(context.Background(), Name, nil)
	if err == nil {
		t.Fatal("Execute succeeded with no status seam")
	}
	if got := tool.FailureMessage(err); got != "I can't check my status right now." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

// TestConcurrentStatusCallsAreRaceFree runs the real Tool against one shared
// status seam; run with -race to prove the Tool and seam are safe.
func TestConcurrentStatusCallsAreRaceFree(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := NewModule().Register(reg, tool.Deps{Status: func(context.Context) tool.StatusReport {
		return tool.StatusReport{Services: []tool.StatusCheck{{Name: "embed", State: tool.StatusOK}}}
	}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Execute(context.Background(), Name, nil); err != nil {
				t.Errorf("Execute: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestStatusToolMetadata(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := NewModule().Register(reg, tool.Deps{Status: func(context.Context) tool.StatusReport {
		return tool.StatusReport{Services: []tool.StatusCheck{{Name: "embed", State: tool.StatusOK}}}
	}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	info, ok := reg.Info(Name)
	if !ok {
		t.Fatal("system.status is not registered")
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
	if info.LocalOnly {
		t.Error("LocalOnly = true, want false; status carries no private result detail")
	}
	if !info.HasSpeak || len(info.Examples) == 0 {
		t.Errorf("metadata = %+v, want a spoken Tool with Examples", info)
	}
}

// baseHealthyReport returns a baseline report where all services and location are healthy.
func baseHealthyReport() tool.StatusReport {
	return tool.StatusReport{
		Services: []tool.StatusCheck{{Name: "embed", State: tool.StatusOK}},
		Location: tool.StatusCheck{Name: "location", State: tool.StatusOK, Detail: "Bristol"},
	}
}

func ptr[T any](v T) *T { return &v }

// TestSummarizeVitalsNormalUnchanged verifies that normal host vitals and storage
// leave the summary untouched as a single sentence.
func TestSummarizeVitalsNormalUnchanged(t *testing.T) {
	rep := baseHealthyReport()
	rep.Host = &tool.HostStatus{
		TemperatureC:         ptr(59.3),
		Load:                 ptr([3]float64{0.70, 0.83, 0.76}),
		MemoryAvailableBytes: ptr(uint64(5 * 1024 * 1024 * 1024)),
		UptimeSeconds:        ptr(3600.0),
	}
	rep.Storage = &tool.StorageStatus{
		State: "ok",
	}

	out := summarize(rep)
	if out.Summary != "I'm OK. Everything I can check is responding." {
		t.Errorf("Summary = %q, want unchanged one sentence", out.Summary)
	}
	if out.Host == nil || out.Host.TemperatureC == nil || *out.Host.TemperatureC != 59.3 {
		t.Errorf("Out.Host = %+v", out.Host)
	}
	if out.Storage == nil || out.Storage.State != "ok" {
		t.Errorf("Out.Storage = %+v", out.Storage)
	}
}

// TestSummarizeHotWarning verifies that a hot CPU appends the warning sentence.
func TestSummarizeHotWarning(t *testing.T) {
	rep := baseHealthyReport()
	rep.Host = &tool.HostStatus{
		TemperatureC: ptr(82.5),
	}

	out := summarize(rep)
	want := "I'm OK. Everything I can check is responding. The CPU is running hot."
	if out.Summary != want {
		t.Errorf("Summary = %q, want %q", out.Summary, want)
	}
}

// TestSummarizeLowMemoryWarning verifies that low available memory appends the warning sentence.
func TestSummarizeLowMemoryWarning(t *testing.T) {
	rep := baseHealthyReport()
	rep.Host = &tool.HostStatus{
		MemoryAvailableBytes: ptr(uint64(200 * 1024 * 1024)),
	}

	out := summarize(rep)
	want := "I'm OK. Everything I can check is responding. Available memory is low."
	if out.Summary != want {
		t.Errorf("Summary = %q, want %q", out.Summary, want)
	}
}

// TestSummarizeBothVitalsCrossed verifies that both hot and low memory are joined in one sentence.
func TestSummarizeBothVitalsCrossed(t *testing.T) {
	rep := baseHealthyReport()
	rep.Host = &tool.HostStatus{
		TemperatureC:         ptr(85.0),
		MemoryAvailableBytes: ptr(uint64(150 * 1024 * 1024)),
	}

	out := summarize(rep)
	want := "I'm OK. Everything I can check is responding. The CPU is running hot and available memory is low."
	if out.Summary != want {
		t.Errorf("Summary = %q, want %q", out.Summary, want)
	}
}

// TestSummarizeStorageLowWarning verifies storage low threshold warning.
func TestSummarizeStorageLowWarning(t *testing.T) {
	rep := baseHealthyReport()
	rep.Storage = &tool.StorageStatus{
		State: "low",
	}

	out := summarize(rep)
	want := "I'm OK. Everything I can check is responding. Storage space is running low."
	if out.Summary != want {
		t.Errorf("Summary = %q, want %q", out.Summary, want)
	}
}

// TestSummarizeStorageFullWarning verifies storage full threshold warning.
func TestSummarizeStorageFullWarning(t *testing.T) {
	rep := baseHealthyReport()
	rep.Storage = &tool.StorageStatus{
		State: "full",
	}

	out := summarize(rep)
	want := "I'm OK. Everything I can check is responding. Storage space is full."
	if out.Summary != want {
		t.Errorf("Summary = %q, want %q", out.Summary, want)
	}
}

// TestSummarizeAllUnavailableNoInventedWarning verifies that when host and storage
// are unavailable, no warning is invented.
func TestSummarizeAllUnavailableNoInventedWarning(t *testing.T) {
	rep := baseHealthyReport()
	rep.Host = &tool.HostStatus{}
	rep.Storage = &tool.StorageStatus{State: "unavailable"}

	out := summarize(rep)
	want := "I'm OK. Everything I can check is responding."
	if out.Summary != want {
		t.Errorf("Summary = %q, want %q (no invented warning)", out.Summary, want)
	}
}
