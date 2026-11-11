package tools_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func TestRegisterAllRegistersPrototypeTools(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tools.RegisterAll(reg, nil); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if _, ok := reg.Info(clock.Name); !ok {
		t.Fatalf("%s is not registered", clock.Name)
	}
}
