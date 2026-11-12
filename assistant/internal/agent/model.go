package agent

import (
	"fmt"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// ModelKey is the live Agent model-name setting. It is read before every
// provider request, so a Dashboard change applies to the next turn without
// rebuilding the client or restarting the process.
const ModelKey = "agent.model"

// ModelDefinition creates the model-name setting. defaultModel is the code
// default for the active provider; configModel is the config.yaml layer, so a
// reset returns to the model the deployment was configured with.
func ModelDefinition(defaultModel, configModel string) settings.Definition {
	return settings.String(ModelKey, defaultModel,
		settings.WithKind(settings.Live),
		settings.WithConfig(configModel),
		settings.WithDescription("Model name sent to the configured provider. A change applies to the next turn; there is no automatic failover."),
		settings.WithValidator(validateModelName),
	)
}

// validateModelName rejects an empty value before it reaches the provider.
// The provider still decides whether a non-empty name exists.
func validateModelName(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("want a model name string, got %T", value)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("must not be blank")
	}
	return nil
}
