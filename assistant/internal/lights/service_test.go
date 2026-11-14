// Transport outcome tests name the failing controller without a Bluetooth adapter.
package lights

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

func testSettings(t *testing.T) *settings.Service {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ss, err := settings.New(ctx, st, Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SetJSON(ctx, "lights.enabled", json.RawMessage("true")); err != nil {
		t.Fatal(err)
	}
	devices, _ := json.Marshal(`[{"address":"BE:68:F8:24:A7:2D","name":"Desk","protocol":"lotus"}]`)
	if _, err := ss.SetJSON(ctx, "lights.devices", devices); err != nil {
		t.Fatal(err)
	}
	return ss
}

// A write that fails keeps the controller's name and the transport reason, so
// Health can say which saved light is unavailable instead of "controller
// unavailable" for the whole feature.
func TestApplyKeepsTheControllerAndReason(t *testing.T) {
	ss := testSettings(t)
	s := New(func() *settings.Service { return ss })
	s.Run = func(context.Context, ...string) ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte("WARNING:root:Could not determine BlueZ version\nTraceback (most recent call last):\nbleak.exc.BleakError: failed to discover services, device disconnected\n")}
	}
	if err := s.Apply(context.Background(), "BE:68:F8:24:A7:2D", "power", "on"); err == nil {
		t.Fatal("a failed helper run must fail the write")
	}
	want := "Desk: failed to discover services, device disconnected"
	if s.Last() != want || !s.Failed() {
		t.Fatalf("Last() = %q, Failed() = %v, want %q and true", s.Last(), s.Failed(), want)
	}

	s.Run = func(context.Context, ...string) ([]byte, error) { return nil, nil }
	if err := s.Apply(context.Background(), "", "power", "on"); err != nil {
		t.Fatal(err)
	}
	if s.Failed() || s.Last() != "Last write accepted; physical state unverified" {
		t.Fatalf("a successful write did not clear the failure: %q", s.Last())
	}
}

// A helper killed by the shared budget says so instead of echoing "signal: killed".
func TestErrTextNamesTheBudget(t *testing.T) {
	if got := errText(context.DeadlineExceeded); got != "no answer within 14s" {
		t.Fatalf("errText = %q", got)
	}
}

// A disabled feature is a configuration answer, not a controller failure.
func TestApplyWhenDisabledKeepsHealthClean(t *testing.T) {
	ss := testSettings(t)
	if _, err := ss.SetJSON(context.Background(), "lights.enabled", json.RawMessage("false")); err != nil {
		t.Fatal(err)
	}
	s := New(func() *settings.Service { return ss })
	s.Run = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("a disabled feature must not run the helper")
		return nil, nil
	}
	if err := s.Apply(context.Background(), "", "power", "on"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
	if s.Failed() || s.Last() != "" {
		t.Fatalf("Failed() = %v, Last() = %q", s.Failed(), s.Last())
	}
}
