// The BLE lights Health row names the saved controller whose last write failed.
package lights

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestAuxHealthNamesTheUnavailableController(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := settings.New(ctx, st, core.Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SetJSON(ctx, "lights.enabled", json.RawMessage("true")); err != nil {
		t.Fatal(err)
	}
	devices, _ := json.Marshal(`[{"address":"BE:67:00:26:15:E4","name":"OB04","protocol":"lotus"}]`)
	if _, err := ss.SetJSON(ctx, "lights.devices", devices); err != nil {
		t.Fatal(err)
	}

	m := &Module{Service: core.New(func() *settings.Service { return ss })}
	m.Service.Run = func(context.Context, ...string) ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte("bleak.exc.BleakError: failed to discover services, device disconnected\n")}
	}
	if err := m.Service.Apply(ctx, "", "power", "on"); err == nil {
		t.Fatal("a failed helper run must fail the write")
	}
	rows := m.AuxHealth(ctx)
	if len(rows) != 1 || rows[0].Name != "BLE lights" || rows[0].State != tool.StatusError {
		t.Fatalf("rows = %+v", rows)
	}
	if !strings.Contains(rows[0].Detail, "OB04") || !strings.Contains(rows[0].Detail, "device disconnected") {
		t.Fatalf("detail = %q", rows[0].Detail)
	}
}
