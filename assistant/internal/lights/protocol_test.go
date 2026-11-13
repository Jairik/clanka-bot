package lights

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"testing"
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
			got, err := (BLEDriver{tc.p}).Encode(tc.op, tc.v)
			if err != nil || hex.EncodeToString(got) != tc.want {
				t.Fatalf("%x %v want %s", got, err, tc.want)
			}
		})
	}
	for _, op := range []string{"power", "brightness", "color", "mode", "unknown"} {
		if _, err := (BLEDriver{"lotus"}).Encode(op, "invalid"); err == nil {
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
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing deadline")
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
	if err := s.Apply(ctx, "", "power", "on"); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if err := s.Apply(ctx, "other", "power", "on"); err == nil || calls != 1 {
		t.Fatal("unknown device wrote")
	}
	s.Run = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }
	if err := s.Apply(ctx, "Desk", "power", "off"); err == nil {
		t.Fatal("offline succeeded")
	}
}
