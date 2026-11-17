// Package wakethreshold turns the configured wake models into live per-model
// threshold settings. The map a detect stream sends is resolved from the one
// settings service, so a stored override applies on the next stream without a
// restart (ADR-0003). The model allowlist itself stays restart-required: which
// models the service must advertise is a startup decision.
package wakethreshold

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Prefix is the key prefix one setting per configured model shares. The
// Dashboard routes it to the Wake word section, as it does for quiet.
const Prefix = "wakeword.threshold."

// Key is one model's live threshold setting.
func Key(model string) string { return Prefix + model }

// Models is every configured model: the wake allowlist the loop listens for
// plus the configured command models. Command models are included even while
// commands are disabled, because config.yaml already accepts a threshold for
// one and the Settings page shows the switch rather than hiding the feature.
func Models(cfg config.Wakeword) []string {
	models := make([]string, 0, len(cfg.WakeModels())+len(cfg.Commands.ModelNames()))
	models = append(models, cfg.WakeModels()...)
	models = append(models, cfg.Commands.ModelNames()...)
	return models
}

// Definitions builds one Float setting per configured model. The code default
// stays what it is today (openWakeWord's 0.5 for a wake model, the stricter
// command default for a command model); an explicit environment or config.yaml
// value becomes the config layer, so Reset returns to it rather than to the
// code default.
func Definitions(cfg config.Wakeword) []settings.Definition {
	models := Models(cfg)
	defs := make([]settings.Definition, 0, len(models))
	for _, model := range models {
		opts := []settings.Option{
			settings.WithDescription("Detection threshold for " + model + "; higher is stricter. Applies to the next detection."),
			settings.WithValidator(validate),
		}
		if value, ok := configValue(cfg, model); ok {
			opts = append(opts, settings.WithConfig(value))
		}
		defs = append(defs, settings.Float(Key(model), codeDefault(cfg, model), opts...))
	}
	return defs
}

// codeDefault is the value in code when neither an override nor an explicit
// environment/config value applies.
func codeDefault(cfg config.Wakeword, model string) float64 {
	if cfg.IsCommandModel(model) {
		return config.DefaultCommandThreshold
	}
	return config.DefaultWakeThreshold
}

// configValue is the environment/config.yaml layer for one model, reported
// absent when the model inherits the code default. The environment is merged
// into cfg.Thresholds by config.Load, so this is the deployed map either way.
func configValue(cfg config.Wakeword, model string) (float64, bool) {
	if value, ok := cfg.Thresholds[model]; ok {
		return value, true
	}
	value := codeDefault(cfg, model)
	switch {
	case cfg.IsCommandModel(model) && cfg.Commands.Threshold > 0:
		value = cfg.Commands.Threshold
	case !cfg.IsCommandModel(model) && cfg.Threshold > 0:
		value = cfg.Threshold
	}
	// config.Default sets the global threshold to the code default, so an
	// untouched deployment reports the code layer rather than a config one.
	if value == codeDefault(cfg, model) {
		return 0, false
	}
	return value, true
}

// validate keeps a stored threshold inside the probability range, so a typo
// cannot silence a model (> 1) or make every room noise fire (> 0 is still
// required).
func validate(value any) error {
	threshold, ok := value.(float64)
	if !ok {
		return fmt.Errorf("want a number, got %T", value)
	}
	if !(threshold > 0 && threshold <= 1) {
		return fmt.Errorf("threshold must be greater than 0 and at most 1")
	}
	return nil
}

// Source resolves the live map and a cheap signature for the models the loop
// actually requests. It is the closure main hands the voice loop, so the
// Dashboard write and the next detect stream share one service.
type Source struct {
	service *settings.Service
	// models is the requested set in sorted order, so Signature is stable
	// whatever order config.yaml lists them in.
	models []string
}

// NewSource binds the configured detect models to the live settings service.
func NewSource(service *settings.Service, cfg config.Wakeword) *Source {
	models := cfg.DetectModels()
	sort.Strings(models)
	return &Source{service: service, models: models}
}

// Map is the complete per-model threshold map for one detect stream. It is
// rebuilt per Open, so a stored change reaches the next stream.
func (s *Source) Map() map[string]float64 {
	if s == nil || len(s.models) == 0 {
		return nil
	}
	thresholds := make(map[string]float64, len(s.models))
	for _, model := range s.models {
		thresholds[model] = s.service.Float(Key(model))
	}
	return thresholds
}

// Signature is the sorted model=threshold string the loop compares while a
// stream is open. It changes exactly when an effective threshold does, so an
// unrelated setting write never reopens a healthy stream.
func (s *Source) Signature() string {
	if s == nil || len(s.models) == 0 {
		return ""
	}
	var builder strings.Builder
	for index, model := range s.models {
		if index > 0 {
			builder.WriteByte(';')
		}
		builder.WriteString(model)
		builder.WriteByte('=')
		builder.WriteString(strconv.FormatFloat(s.service.Float(Key(model)), 'g', -1, 64))
	}
	return builder.String()
}
