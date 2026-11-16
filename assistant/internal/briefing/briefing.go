// Package briefing composes a single spoken morning briefing from several
// Tools, above the Tool registry and without knowing the pipeline. A briefing
// is a pipeline stage, not a Tool: Tools never call Tools (GLOSSARY "Tool" and
// ADR-0008), so the composition lives here and the pipeline records the history
// and latency around it.
package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
	"github.com/Jairik/clanka-bot/assistant/tools/list"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

// Phrases is the normalised exact phrase set that opens a briefing. It is the
// one place to change which utterances route to a morning briefing; the
// pipeline matches a normalised transcript against it before the chain planner.
var Phrases = []string{
	"good morning",
	"what's my day",
	"what does my day look like",
	"brief me",
	"morning briefing",
}

// defaultMaxRunes is the spoken-length budget for one briefing reply. The
// composer keeps the joined clauses under it and asks for the rest when a
// clause would cross it.
const defaultMaxRunes = 600

// Registry is the narrow seam the composer needs: enough to execute a Tool,
// render its reply, and learn whether it is enabled and Local-only. The real
// *tool.Registry satisfies it, so the composer stays decoupled from the
// pipeline and the registry's own timeout and policy logic.
type Registry interface {
	Info(name string) (tool.Info, bool)
	Execute(ctx context.Context, name string, args json.RawMessage) (any, error)
	Speak(name string, result any) (string, bool)
}

// Part is one rendered section of a briefing and the Tool that produced it.
type Part struct {
	Name      string
	LocalOnly bool
	Text      string
}

// Result is the composed briefing: the single spoken reply and its parts.
type Result struct {
	Text  string
	Parts []Part
}

// partSpec names one fixed source the composer always tries, in priority order.
type partSpec struct {
	name string
	args json.RawMessage
}

// partSpecs is the briefing's fixed composition order. list.read always reads
// the to-do list; the shopping list is never read here.
var partSpecs = []partSpec{
	{name: weather.Name},
	{name: alarm.ListName},
	{name: timer.ListName},
	{name: reminder.ListName},
	{name: list.ReadName, args: json.RawMessage(`{"list":"to-do"}`)},
}

// Composer builds a spoken briefing from the fixed sources. It holds a clock and
// a logger for symmetry with the rest of the system and for honest skip logging;
// it must not import the pipeline.
type Composer struct {
	registry Registry
	now      func() time.Time
	logger   *slog.Logger
	phrases  []string
	maxRunes int
}

// New builds a Composer over the given registry seam.
func New(registry Registry, now func() time.Time, logger *slog.Logger) *Composer {
	return &Composer{
		registry: registry,
		now:      now,
		logger:   logger,
		phrases:  Phrases,
		maxRunes: defaultMaxRunes,
	}
}

// Matches reports whether a normalised transcript opens a briefing: an exact
// phrase, or a phrase that opens a compound utterance (so a "good morning and
// set a timer" still takes the briefing, not the chain planner).
func (c *Composer) Matches(normalized string) bool {
	for _, phrase := range c.phrases {
		if normalized == phrase {
			return true
		}
		tail := strings.TrimPrefix(normalized, phrase+" ")
		if tail != normalized && tail != "" && isJoiner(tail) {
			return true
		}
	}
	return false
}

// isJoiner reports whether s opens with a chain joiner word, so a briefing
// phrase followed by "and …" or "then …" is still a briefing.
func isJoiner(s string) bool {
	return strings.HasPrefix(s, "and ") ||
		strings.HasPrefix(s, "then ") ||
		strings.HasPrefix(s, "and then ")
}

// Compose runs every fixed source and joins the rendered clauses into one
// reply. A disabled or absent Tool is skipped; a Tool whose handler errors is
// skipped whole. The joined clauses stay under maxRunes runes; a clause that
// would cross the cap is dropped and "Ask me for the rest." is appended.
func (c *Composer) Compose(ctx context.Context) Result {
	var clauses []string
	var parts []Part
	skippedCap := false
	total := 0
	for _, spec := range partSpecs {
		text, localOnly, ok := c.render(ctx, spec)
		if !ok {
			continue
		}
		add := text
		if total > 0 {
			add = " " + text
		}
		if total+utf8.RuneCountInString(add) > c.maxRunes {
			skippedCap = true
			continue
		}
		total += utf8.RuneCountInString(add)
		clauses = append(clauses, text)
		parts = append(parts, Part{Name: spec.name, LocalOnly: localOnly, Text: text})
	}
	text := strings.Join(clauses, " ")
	if skippedCap {
		if text != "" {
			text += " "
		}
		text += "Ask me for the rest."
	}
	return Result{Text: text, Parts: parts}
}

// render executes one source and returns its spoken clause. ok is false when
// the Tool is disabled, absent, errors, or has no renderer.
func (c *Composer) render(ctx context.Context, spec partSpec) (text string, localOnly bool, ok bool) {
	info, found := c.registry.Info(spec.name)
	if !found || !info.Enabled {
		return "", false, false
	}
	localOnly = info.LocalOnly
	result, err := c.registry.Execute(ctx, spec.name, spec.args)
	if err != nil {
		if c.logger != nil {
			c.logger.Info("briefing: part skipped", "part", spec.name, "error", err.Error(), "at", c.now())
		}
		return "", localOnly, false
	}
	if spec.name == list.ReadName {
		out, converted := result.(list.ReadOut)
		if !converted {
			return "", localOnly, false
		}
		return RenderTodo(out), localOnly, true
	}
	spoken, have := c.registry.Speak(spec.name, result)
	if !have {
		return "", localOnly, false
	}
	return spoken, localOnly, true
}

// RenderTodo renders the to-do list for a briefing: at most three items, then
// "and N more", or "Your to-do list is empty." Only the to-do list is read, so
// the shopping list is never rendered.
func RenderTodo(out list.ReadOut) string {
	var items []string
	for _, l := range out.Lists {
		if l.Key != list.KeyTodo {
			continue
		}
		for _, it := range l.Items {
			items = append(items, it.Text)
		}
	}
	if len(items) == 0 {
		return "Your to-do list is empty."
	}
	if len(items) <= 3 {
		return fmt.Sprintf("%s on your to-do list: %s.", countWord(len(items)), joinItems(items))
	}
	return fmt.Sprintf("Three things on your to-do list: %s, and %d more.", joinItems(items[:3]), len(items)-3)
}

// countWord is the leading count phrase for a to-do list of n items.
func countWord(n int) string {
	switch n {
	case 1:
		return "One thing"
	case 2:
		return "Two things"
	default:
		return "Three things"
	}
}

// joinItems renders a list of item texts as "a, b and c".
func joinItems(items []string) string {
	switch len(items) {
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
