package tools_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

// stubControls satisfies the audio seam without touching a device.
type stubControls struct{}

func (stubControls) SetVolume(context.Context, int) error    { return nil }
func (stubControls) AdjustVolume(context.Context, int) error { return nil }
func (stubControls) Stop(context.Context) error              { return nil }

func TestRegisterAllRegistersPrototypeTools(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tools.RegisterAll(reg, tools.Options{Audio: stubControls{}}); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	for _, name := range []string{
		clock.Name, clock.DateName,
		timer.SetName, timer.CancelName, timer.ListName,
		audiocontrol.VolumeName, audiocontrol.StopName,
	} {
		if _, ok := reg.Info(name); !ok {
			t.Errorf("%s is not registered", name)
		}
	}
	info, ok := reg.Info(weather.Name)
	if !ok {
		t.Fatalf("%s is not registered", weather.Name)
	}
	if info.LocalOnly || info.Permission != tool.PermissionRead || !info.HasSpeak || len(info.Examples) == 0 {
		t.Fatalf("weather.current metadata = %+v, want a non-local read with Speak and Examples", info)
	}
}
