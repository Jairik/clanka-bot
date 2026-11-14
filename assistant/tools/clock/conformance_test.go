package clock_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/conformance"
)

// TestModuleConformance is the one-line contract check every Tool module runs.
func TestModuleConformance(t *testing.T) {
	conformance.Run(t, clock.NewModule())
}
