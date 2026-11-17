package monster_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/conformance"
	"github.com/Jairik/clanka-bot/assistant/tools/monster"
)

// TestModuleConformance is the one-line shared contract check; the real-model
// routing part runs when ROUTING_EVAL_EMBED_URL is set (docs/agents/adding-a-tool.md).
func TestModuleConformance(t *testing.T) {
	conformance.Run(t, monster.NewModule())
}
