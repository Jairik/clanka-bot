package system

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// longDetail exceeds the 80-rune spoken limit and contains a newline after
// the first sentence; only the first line, truncated, may be spoken.
const longDetail = "No response within 3s. internal/10.0.0.5:9000 refused the connection after a very long retry sequence that keeps going and going.\nsecond line must never be spoken"

func diagnoseSeam(report tool.StatusReport) func(context.Context) tool.StatusReport {
	return func(context.Context) tool.StatusReport { return report }
}

func TestDiagnoseExactlyThreeFailingChecksNamedAndTruncated(t *testing.T) {
	report := tool.StatusReport{
		Services: []tool.StatusCheck{
			{Name: "tts", State: tool.StatusError, Detail: longDetail},
			{Name: "embed", State: tool.StatusTimedOut, Detail: "responded in 12ms"},
			{Name: "assistant", State: tool.StatusOK},
		},
		Modules: []tool.StatusCheck{
			{Name: "weather", State: tool.StatusError, Detail: "upstream 10.0.0.5 refused"},
			{Name: "clock", State: tool.StatusNoCheck},
			{Name: "notes", State: tool.StatusError, Detail: "fourth failure must not be spoken"},
		},
		Location: tool.StatusCheck{Name: "location", State: tool.StatusOK},
	}
	out := diagnose(report)

	if len(out.Checks) != diagnoseMaxChecks {
		t.Fatalf("len(Checks) = %d, want %d: %+v", len(out.Checks), diagnoseMaxChecks, out.Checks)
	}
	for _, name := range []string{"tts", "embed", "weather"} {
		if !strings.Contains(out.Text, name) {
			t.Errorf("Text %q does not name failing check %q", out.Text, name)
		}
	}
	if strings.Contains(out.Text, "assistant") || strings.Contains(out.Text, "fourth failure") {
		t.Errorf("Text %q names a healthy or fourth check", out.Text)
	}
	first := out.Checks[0]
	if utf8.RuneCountInString(first.Detail) > diagnoseDetailRunes {
		t.Errorf("first Detail has %d runes, want at most %d: %q",
			utf8.RuneCountInString(first.Detail), diagnoseDetailRunes, first.Detail)
	}
	if strings.Contains(first.Detail, "\n") || strings.Contains(first.Detail, "second line") {
		t.Errorf("first Detail kept a second line: %q", first.Detail)
	}
	if !strings.HasPrefix(first.Detail, "No response within 3s.") {
		t.Errorf("first Detail lost its first line: %q", first.Detail)
	}
	if strings.Contains(out.Text, "second line") {
		t.Errorf("Text %q kept text after the first newline", out.Text)
	}
}

func TestDiagnoseOneFailingCheck(t *testing.T) {
	report := tool.StatusReport{
		Services: []tool.StatusCheck{{Name: "tts", State: tool.StatusError, Detail: "No response within 3s."}},
	}
	out := diagnose(report)
	if len(out.Checks) != 1 || out.Checks[0].State != string(tool.StatusError) {
		t.Fatalf("Checks = %+v, want one error check", out.Checks)
	}
	if !strings.Contains(out.Text, "tts") || !strings.Contains(out.Text, "No response within 3s") {
		t.Errorf("Text = %q, want the named failure and its detail", out.Text)
	}
}

func TestDiagnoseAllFineWording(t *testing.T) {
	report := tool.StatusReport{
		Services: []tool.StatusCheck{{Name: "embed", State: tool.StatusOK}},
		Location: tool.StatusCheck{Name: "location", State: tool.StatusOK},
	}
	out := diagnose(report)
	if out.Text != diagnoseAllFine {
		t.Errorf("Text = %q, want %q", out.Text, diagnoseAllFine)
	}
	if len(out.Checks) != 0 {
		t.Errorf("Checks = %+v, want none when nothing fails", out.Checks)
	}
}

func TestDiagnoseTimedOutSnapshotIsAFailure(t *testing.T) {
	report := tool.StatusReport{
		Services: []tool.StatusCheck{{Name: "embed", State: tool.StatusTimedOut, Detail: "No response within 3s."}},
	}
	out := diagnose(report)
	if len(out.Checks) != 1 || out.Checks[0].State != string(tool.StatusTimedOut) {
		t.Fatalf("Checks = %+v, want the timed-out check", out.Checks)
	}
	if !strings.Contains(out.Text, "embed") {
		t.Errorf("Text = %q, want the timed-out check named", out.Text)
	}
}

func TestDiagnoseUncountableSnapshotIsNotAHealthClaim(t *testing.T) {
	out := diagnose(tool.StatusReport{})
	if out.Text != diagnoseUnavailable {
		t.Errorf("Text = %q, want %q", out.Text, diagnoseUnavailable)
	}
	if len(out.Checks) != 0 {
		t.Errorf("Checks = %+v, want none for an empty snapshot", out.Checks)
	}
}

func TestDiagnoseNilSeamWording(t *testing.T) {
	tl := NewDiagnose(nil)
	if _, err := tl.Handler(context.Background(), DiagnoseIn{}); err == nil {
		t.Fatal("handler succeeded with a nil status seam")
	} else if got := tool.FailureMessage(err); got != diagnoseUnavailable {
		t.Errorf("FailureMessage = %q, want %q", got, diagnoseUnavailable)
	}

	// A module registered with no Deps.Status must fail the same way.
	reg := tool.NewRegistry(time.Second)
	if err := NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := reg.Execute(context.Background(), DiagnoseName, nil); err == nil {
		t.Fatal("Execute succeeded with no status seam")
	} else if got := tool.FailureMessage(err); got != diagnoseUnavailable {
		t.Errorf("FailureMessage = %q, want %q", got, diagnoseUnavailable)
	}
}

func TestDiagnoseTextRuneCap(t *testing.T) {
	const huge = "a detail that just keeps going with internal addresses and upstream errors and retries and more text " +
		"for far longer than the spoken budget allows so the whole answer must be cut at the rune limit"
	services := make([]tool.StatusCheck, 0, 10)
	for i := 0; i < 10; i++ {
		services = append(services, tool.StatusCheck{
			Name:   strings.Repeat("service", 4) + string(rune('a'+i)),
			State:  tool.StatusError,
			Detail: huge,
		})
	}
	out := diagnose(tool.StatusReport{Services: services})
	if got := utf8.RuneCountInString(out.Text); got > diagnoseTextRunes {
		t.Errorf("Text has %d runes, want at most %d: %q", got, diagnoseTextRunes, out.Text)
	}
}

func TestDiagnoseMetadata(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := NewModule().Register(reg, tool.Deps{Status: diagnoseSeam(tool.StatusReport{
		Services: []tool.StatusCheck{{Name: "embed", State: tool.StatusOK}},
	})}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	info, ok := reg.Info(DiagnoseName)
	if !ok {
		t.Fatal("system.diagnose is not registered")
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
	// LocalOnlyYes keeps the detail on the Pi: the Agent prompt skips tools
	// whose Info.LocalOnly is true, so it cannot see or call this Tool.
	if !info.LocalOnly {
		t.Error("LocalOnly = false, want true; diagnose details must never reach the model")
	}
	if info.Timeout != 5*time.Second {
		t.Errorf("Timeout = %s, want 5s", info.Timeout)
	}
	if !info.HasSpeak || len(info.Examples) == 0 {
		t.Errorf("metadata = %+v, want a spoken Tool with Examples", info)
	}
	if _, stillStatus := reg.Info(Name); !stillStatus {
		t.Error("registering system.diagnose removed system.status")
	}
}
