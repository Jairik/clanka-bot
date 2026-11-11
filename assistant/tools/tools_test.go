package tools_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

func TestRegisterAllRegistersPrototypeTools(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tools.RegisterAll(reg, tools.Options{}); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if _, ok := reg.Info(clock.Name); !ok {
		t.Fatalf("%s is not registered", clock.Name)
	}
	info, ok := reg.Info(weather.Name)
	if !ok {
		t.Fatalf("%s is not registered", weather.Name)
	}
	if info.LocalOnly || info.Permission != tool.PermissionRead || !info.HasSpeak || len(info.Examples) == 0 {
		t.Fatalf("weather.current metadata = %+v, want a non-local read with Speak and Examples", info)
	}
}
