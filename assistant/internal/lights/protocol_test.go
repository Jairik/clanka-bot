package lights

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"testing"
	"time"
)

// TestKnownBytes pins the community protocol sequences independently of BLE.
func TestKnownBytes(t *testing.T) {
	for _, tc := range []struct{ p, op, v, want string }{
		{"lotus", "power", "on", "7e0004f00001ff00ef"}, {"lotus", "power", "off", "7e0004000000ff00ef"},
		{"lotus", "color", "#123456", "7e00050312345600ef"}, {"lotus", "brightness", "50", "7e000132ff00ff00ef"},
		{"lotus", "mode", "rainbow", "7e00038803000000ef"}, {"lotus", "mode", "pulse", "7e00038b03000000ef"}, {"lotus", "mode", "strobe", "7e00039603000000ef"},
		{"happy", "power", "on", "cc2333"}, {"happy", "power", "off", "cc2433"}, {"happy", "color", "#123456", "5612345600f0aa"},
		{"happy", "brightness", "0", "5600000000f0aa"}, {"happy", "brightness", "100", "56ffffff00f0aa"}, {"happy", "mode", "rainbow", "bb251044"}, {"happy", "mode", "pulse", "bb261044"}, {"happy", "mode", "strobe", "bb301044"},
	} {
		t.Run(tc.p+tc.op+tc.v, func(t *testing.T) {
			got, err := (BLEDriver{Protocol: tc.p}).Encode(tc.op, tc.v)
			if err != nil || hex.EncodeToString(got) != tc.want {
				t.Fatalf("%x %v want %s", got, err, tc.want)
			}
		})
	}
	for _, op := range []string{"power", "brightness", "color", "mode", "unknown"} {
		if _, err := (BLEDriver{Protocol: "lotus"}).Encode(op, "invalid"); err == nil {
			t.Fatal("accepted invalid", op)
		}
	}
}

// TestService uses the real settings store and a fake transport, including disable and failures.
func TestService(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/settings.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := settings.New(ctx, st, Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	s := New(func() *settings.Service { return ss })
	calls := 0
	s.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		deadline, ok := ctx.Deadline()
		// A cold controller needs more than a few seconds. Keep the budget
		// above that so a later edit cannot shrink it back to a failed connect.
		if !ok || time.Until(deadline) < 10*time.Second {
			t.Fatal("write budget too short for a cold controller")
		}
		return nil, nil
	}
	if _, err := s.Scan(ctx); err == nil || calls != 0 {
		t.Fatal("disabled scan reached transport")
	}
	if _, err := ss.SetJSON(ctx, "lights.enabled", []byte(`true`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SetJSON(ctx, "lights.devices", []byte(`"[{\"address\":\"AA:BB:CC:DD:EE:FF\",\"name\":\"Desk\",\"protocol\":\"happy\"}]"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, "", "power", "on"); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := s.Apply(ctx, "other", "power", "on"); err == nil || calls != 1 {
		t.Fatal("unknown device wrote")
	}
	s.Run = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }
	if _, err := s.Apply(ctx, "Desk", "power", "off"); err == nil {
		t.Fatal("offline succeeded")
	}
}

// TestHappyBrightnessKeepsColour pins the scaled RGB packet and the grey fallback.
func TestHappyBrightnessKeepsColour(t *testing.T) {
	for _, tc := range []struct{ colour, percent, want string }{
		{"#ff8000", "50", "5680400000f0aa"},
		{"#ff8000", "0", "5600000000f0aa"},
		// No colour keeps the grey packet today's encoder already sends.
		{"", "50", "567f7f7f00f0aa"},
		// 1% of a channel that would round to 0 stays 1, so it is not black.
		{"#010000", "1", "5601000000f0aa"},
	} {
		t.Run(tc.colour+"@"+tc.percent, func(t *testing.T) {
			got, err := (BLEDriver{Protocol: "happy", Color: tc.colour}).Encode("brightness", tc.percent)
			if err != nil || hex.EncodeToString(got) != tc.want {
				t.Fatalf("%x %v want %s", got, err, tc.want)
			}
		})
	}
	// Lotus brightness is its own command and ignores a remembered colour.
	got, err := (BLEDriver{Protocol: "lotus", Color: "#ff8000"}).Encode("brightness", "50")
	if err != nil || hex.EncodeToString(got) != "7e000132ff00ff00ef" {
		t.Fatalf("%x %v", got, err)
	}
}

// TestRememberedColor checks that only a successful color write is remembered,
// and that a later process still has it for HappyLighting brightness.
func TestRememberedColor(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/settings.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := settings.New(ctx, st, Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SetJSON(ctx, "lights.enabled", []byte(`true`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SetJSON(ctx, "lights.devices", []byte(`"[{\"address\":\"AA:BB:CC:DD:EE:FF\",\"name\":\"Desk\",\"protocol\":\"happy\"}]"`)); err != nil {
		t.Fatal(err)
	}
	s := New(func() *settings.Service { return ss })
	var packets []string
	s.Run = func(_ context.Context, args ...string) ([]byte, error) {
		packets = append(packets, args[len(args)-1])
		return nil, nil
	}
	const address = "AA:BB:CC:DD:EE:FF"

	if _, err := s.Apply(ctx, "Desk", "brightness", "50"); err != nil {
		t.Fatal(err)
	}
	if packets[len(packets)-1] != "567f7f7f00f0aa" {
		t.Fatalf("grey packet %s", packets[len(packets)-1])
	}
	if _, err := s.Apply(ctx, "Desk", "color", "#FF8000"); err != nil {
		t.Fatal(err)
	}
	got, err := s.colorFor(ctx, address)
	if err != nil || got != "#ff8000" {
		t.Fatalf("remembered %q %v", got, err)
	}
	if _, ok := ss.Get(colorKey(address)); ok {
		t.Fatal("remembered colour is a user setting")
	}
	if _, err := s.Apply(ctx, "Desk", "brightness", "50"); err != nil {
		t.Fatal(err)
	}
	if packets[len(packets)-1] != "5680400000f0aa" {
		t.Fatalf("scaled packet %s", packets[len(packets)-1])
	}

	s.Run = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }
	if _, err := s.Apply(ctx, "Desk", "color", "#0000ff"); err == nil {
		t.Fatal("offline color succeeded")
	}
	got, err = s.colorFor(ctx, address)
	if err != nil || got != "#ff8000" {
		t.Fatalf("failed write changed colour to %q %v", got, err)
	}
	stored, ok, err := ss.GetPrivate(ctx, colorKey(address))
	if err != nil || !ok || stored != "#ff8000" {
		t.Fatalf("stored %q ok=%v %v", stored, ok, err)
	}

	// A new service is a restart: the colour comes back from the settings store.
	again := New(func() *settings.Service { return ss })
	got, err = again.colorFor(ctx, address)
	if err != nil || got != "#ff8000" {
		t.Fatalf("reloaded %q %v", got, err)
	}
}
