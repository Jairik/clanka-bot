package weather_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/conformance"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

// TestModuleConformance is the one-line contract check every Tool module runs.
func TestModuleConformance(t *testing.T) {
	conformance.Run(t, weather.NewModule())
}
