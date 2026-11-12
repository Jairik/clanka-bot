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
