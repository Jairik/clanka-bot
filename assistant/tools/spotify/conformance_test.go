package spotify_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/conformance"
	spotifytools "github.com/Jairik/clanka-bot/assistant/tools/spotify"
)

// TestModuleConformance is the one-line shared contract check; the real-model
// routing part runs when ROUTING_EVAL_EMBED_URL is set.
func TestModuleConformance(t *testing.T) {
	conformance.Run(t, spotifytools.NewModule())
}
