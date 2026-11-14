package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// chainJoiner requires a command-like suffix; names and duration conjunctions stay intact.
var chainJoiner = regexp.MustCompile(`(?i)\s+(?:and then|and|then)\s+`)

type chainStep struct {
	text, name string
	args       json.RawMessage
}

// chainPlan validates every leaf before any side effect; uncertain requests stay with the Agent.
func (p *Pipeline) chainPlan(ctx context.Context, text string, phrases map[string]string) []chainStep {
	spans := chainJoiner.FindAllStringIndex(text, -1)
	if len(spans) == 0 || len(spans) > 7 {
		return nil
	}
	var parts []string
	start := 0
	for _, span := range spans {
		suffix := text[span[1]:]
		_, control := phrases[NormalizePhrase(suffix)]
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
		step := chainStep{text: part, name: phrases[NormalizePhrase(part)]}
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
		if !ok || info.Permission == tool.PermissionConfirm {
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
		text, failure, pending := p.callTool(ctx, req, step.text, step.name, tool.PathFast, step.args)
		reply.LocalOnly = reply.LocalOnly || p.localOnlyTool(step.name)
		reply.Tool = step.name
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
