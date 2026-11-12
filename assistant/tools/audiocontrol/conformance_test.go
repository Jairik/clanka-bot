package audiocontrol_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/conformance"
)

// TestModuleConformance is the one-line contract check every Tool module runs.
func TestModuleConformance(t *testing.T) {
	conformance.Run(t, audiocontrol.NewModule())
}
