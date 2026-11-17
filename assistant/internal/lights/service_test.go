// Transport outcome tests name the failing controller without a Bluetooth adapter.
package lights

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
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
	if _, err := s.Apply(context.Background(), "BE:68:F8:24:A7:2D", "power", "on"); err == nil {
		t.Fatal("a failed helper run must fail the write")
	}
	want := "Desk: failed to discover services, device disconnected"
	if s.Last() != want || !s.Failed() {
		t.Fatalf("Last() = %q, Failed() = %v, want %q and true", s.Last(), s.Failed(), want)
	}

	s.Run = func(context.Context, ...string) ([]byte, error) { return nil, nil }
	if _, err := s.Apply(context.Background(), "", "power", "on"); err != nil {
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
	if _, err := s.Apply(context.Background(), "", "power", "on"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
	if s.Failed() || s.Last() != "" {
		t.Fatalf("Failed() = %v, Last() = %q", s.Failed(), s.Last())
	}
}

// settingsWith saves exactly the given lights with the feature enabled.
func settingsWith(t *testing.T, devices ...Device) *settings.Service {
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
	raw, err := json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := json.Marshal(string(raw))
	if _, err := ss.SetJSON(ctx, "lights.devices", value); err != nil {
		t.Fatal(err)
	}
	return ss
}

// A saved name resolves through the shared matcher regardless of case or
// punctuation, and only that one address is written.
func TestApplyResolvesASavedName(t *testing.T) {
	ss := settingsWith(t, Device{Address: "AA:BB:CC:DD:EE:01", Name: "Desk Lamp", Protocol: "lotus"})
	s := New(func() *settings.Service { return ss })
	s.Names = namematch.New(nil, namematch.Config{})
	var writes []string
	s.Run = func(_ context.Context, args ...string) ([]byte, error) {
		writes = append(writes, args[0])
		return nil, nil
	}
	res, err := s.Apply(context.Background(), "desk lamp", "power", "off")
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 || res.Total != 1 {
		t.Fatalf("result = %+v, want one of one", res)
	}
	if len(writes) != 1 || writes[0] != "AA:BB:CC:DD:EE:01" {
		t.Fatalf("writes = %v, want the Desk Lamp address once", writes)
	}
}

// tiesEmbedder returns one vector for every text, so each candidate scores
// equally and the matcher must refuse to pick one.
type tiesEmbedder struct{}

func (tiesEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

// A fuzzy name that fits two saved lights equally is ambiguous, and no
// controller is written.
func TestApplyRefusesAnAmbiguousFuzzyName(t *testing.T) {
	ss := settingsWith(t,
		Device{Address: "AA:BB:CC:DD:EE:01", Name: "Desk Lamp", Protocol: "lotus"},
		Device{Address: "AA:BB:CC:DD:EE:02", Name: "Floor Lamp", Protocol: "lotus"},
	)
	s := New(func() *settings.Service { return ss })
	s.Names = namematch.New(tiesEmbedder{}, namematch.Config{})
	s.Run = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("an ambiguous name must not write")
		return nil, nil
	}
	if _, err := s.Apply(context.Background(), "desk lam", "power", "on"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguity refusal", err)
	}
}

// "all" writes each saved light once, in saved order.
func TestApplyAllWritesEachSavedLightOnceInOrder(t *testing.T) {
	ss := settingsWith(t,
		Device{Address: "AA:BB:CC:DD:EE:01", Name: "Desk Lamp", Protocol: "lotus"},
		Device{Address: "AA:BB:CC:DD:EE:02", Name: "OB04", Protocol: "happy"},
		Device{Address: "AA:BB:CC:DD:EE:03", Name: "Floor", Protocol: "lotus"},
	)
	s := New(func() *settings.Service { return ss })
	var writes []string
	s.Run = func(_ context.Context, args ...string) ([]byte, error) {
		writes = append(writes, args[0])
		return nil, nil
	}
	res, err := s.Apply(context.Background(), AllTarget, "power", "on")
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 3 || res.Total != 3 {
		t.Fatalf("result = %+v, want three of three", res)
	}
	want := []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02", "AA:BB:CC:DD:EE:03"}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	// An omitted device on several saved lights is the same all-target.
	writes = nil
	if _, err := s.Apply(context.Background(), "", "power", "off"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want every saved light", writes)
	}
}

// One unavailable light leaves the others written, reports the partial count
// and names that controller in Health.
func TestApplyAllReportsAPartialSuccess(t *testing.T) {
	ss := settingsWith(t,
		Device{Address: "AA:BB:CC:DD:EE:01", Name: "Desk Lamp", Protocol: "lotus"},
		Device{Address: "AA:BB:CC:DD:EE:02", Name: "OB04", Protocol: "happy"},
		Device{Address: "AA:BB:CC:DD:EE:03", Name: "Floor", Protocol: "lotus"},
	)
	s := New(func() *settings.Service { return ss })
	s.Run = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "AA:BB:CC:DD:EE:02" {
			return nil, &exec.ExitError{Stderr: []byte("bleak.exc.BleakError: failed to discover services, device disconnected\n")}
		}
		return nil, nil
	}
	res, err := s.Apply(context.Background(), AllTarget, "power", "on")
	if err != nil {
		t.Fatalf("a partial batch is not a spoken error: %v", err)
	}
	if res.Done != 2 || res.Total != 3 {
		t.Fatalf("result = %+v, want two of three", res)
	}
	if got := res.Reply(); got != "Done for 2 of 3 lights" {
		t.Fatalf("Reply() = %q", got)
	}
	if !s.Failed() {
		t.Fatal("a failed controller must mark Health")
	}
	for _, want := range []string{"OB04", "device disconnected", "2 of 3 lights answered"} {
		if !strings.Contains(s.Last(), want) {
			t.Fatalf("Last() = %q, want it to name %q", s.Last(), want)
		}
	}
}
