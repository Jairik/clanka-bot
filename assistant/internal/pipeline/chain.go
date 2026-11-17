package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// chainJoiner requires a command-like suffix; names and duration conjunctions stay intact.
var chainJoiner = regexp.MustCompile(`(?i)\s+(?:and then|and|then)\s+`)

type chainStep struct {
	text, name string
	args       json.RawMessage
	intent     focus.Intent
}

// chainPlan validates every leaf before any side effect; uncertain requests stay with the Agent.
func (p *Pipeline) chainPlan(ctx context.Context, text string, phrases, intentPhrases map[string]string) []chainStep {
	spans := chainJoiner.FindAllStringIndex(text, -1)
	if len(spans) == 0 || len(spans) > 7 {
		return nil
	}
	var parts []string
	start := 0
	for _, span := range spans {
		suffix := text[span[1]:]
		norm := NormalizePhrase(suffix)
		_, control := phrases[norm]
		if !control {
			_, control = intentPhrases[norm]
		}
		if !control && (p.semantic == nil || !p.semantic.ClauseStartsCommand(suffix)) {
			continue
		}
		parts = append(parts, strings.TrimSpace(text[start:span[0]]))
		start = span[1]
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, strings.TrimSpace(text[start:]))
	steps := make([]chainStep, 0, len(parts))
	for _, part := range parts {
		norm := NormalizePhrase(part)
		if target, ok := intentPhrases[norm]; ok {
			intent, valid := focus.ParseIntent(target)
			if !valid {
				return nil
			}
			// An intent step resolves directly through Focus rather than the Tool registry.
			steps = append(steps, chainStep{text: part, intent: intent})
			continue
		}
		step := chainStep{text: part, name: phrases[norm]}
		if step.name != "" {
			if info, ok := p.registry.Info(step.name); ok && info.HasExtract {
				var valid bool
				step.args, valid = p.registry.Extract(step.name, part)
				if !valid {
					return nil
				}
			}
		} else if p.semantic != nil {
			match := p.semantic.Match(ctx, part)
			step.name, step.args = match.Tool, match.Args
		}
		info, ok := p.registry.Info(step.name)
		if !ok || !FastPathRoutable(info) {
			// One ineligible leaf disqualifies the whole plan: dropping just
			// that step would silently lose the intent and run the rest, so
			// the entire utterance goes to the Agent path. This is the same
			// floor the Control-phrase and deferred routes apply.
			return nil
		}
		steps = append(steps, step)
	}
	return steps
}

// runChain waits for each reply before starting the next Tool and aborts on the first failure.
func (p *Pipeline) runChain(ctx context.Context, req tool.Request, steps []chainStep, events Events, record *routeLog) Reply {
	reply := Reply{Path: tool.PathFast, RequestID: req.ID}
	var answers []string
	for i, step := range steps {
		if ctx.Err() != nil {
			break
		}
		var text string
		var failure string
		var pending *tool.PendingConfirmation
		if step.intent != "" {
			// An intent step dispatches through Focus rather than the Tool registry.
			var handled bool
			if p.focus != nil {
				text, handled = p.focus.HandleIntent(ctx, step.intent)
			}
			if !handled {
				failure = reasonNoFocus
			}
		} else {
			var localOnly bool
			text, failure, pending, localOnly, _ = p.callTool(ctx, req, step.text, step.name, tool.PathFast, step.args)
			reply.LocalOnly = reply.LocalOnly || localOnly
			reply.Tool = step.name
		}
		if failure != "" || pending != nil {
			record.failure = failure
			text = fmt.Sprintf("Step %d failed: %s Remaining steps cancelled.", i+1, text)
		}
		answers = append(answers, text)
		if events.StepReply != nil {
			if err := events.StepReply(text); err != nil {
				record.failure = err.Error()
				break
			}
		}
		if failure != "" || pending != nil {
			break
		}
	}
	reply.Text = strings.Join(answers, " ")
	return reply
}
