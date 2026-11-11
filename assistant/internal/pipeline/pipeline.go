// Package pipeline turns a typed transcript into a reply using the Tool
// registry. The Agent path and semantic routing arrive in later tickets.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// PlaceholderReply is returned when no path exists yet for a transcript.
const PlaceholderReply = "I don't know how to do that yet."

// Reply is the text answer plus routing details for the log and response.
type Reply struct {
	Text      string
	Tool      string
	Path      tool.Path
	RequestID string
}

// Pipeline routes one transcript at a time. It is safe for concurrent use.
type Pipeline struct {
	registry *tool.Registry
	phrases  map[string]string
	logger   *slog.Logger
}

// New builds a pipeline and rejects a Control phrase table that points at
// unknown Tools or Tools without a Speak renderer.
func New(reg *tool.Registry, phrases map[string]string, logger *slog.Logger) (*Pipeline, error) {
	if reg == nil {
		return nil, fmt.Errorf("pipeline: registry is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	normalized := make(map[string]string, len(phrases))
	for phrase, name := range phrases {
		key := NormalizePhrase(phrase)
		if key == "" {
			return nil, fmt.Errorf("pipeline: control phrase %q is empty", phrase)
		}
		if prev, exists := normalized[key]; exists && prev != name {
			return nil, fmt.Errorf("pipeline: control phrase %q maps to both %q and %q", phrase, prev, name)
		}
		info, ok := reg.Info(name)
		if !ok {
			return nil, fmt.Errorf("pipeline: control phrase %q points at unknown tool %q", phrase, name)
		}
		if !info.HasSpeak {
			return nil, fmt.Errorf("pipeline: control phrase %q points at %q, which has no Speak renderer", phrase, name)
		}
		normalized[key] = name
	}
	return &Pipeline{registry: reg, phrases: normalized, logger: logger}, nil
}

// Handle resolves one transcript. Internal errors are logged in full and only
// a generic message reaches the caller.
func (p *Pipeline) Handle(ctx context.Context, req tool.Request, transcript string) Reply {
	started := time.Now()
	reply := Reply{Path: tool.PathFallthrough, RequestID: req.ID, Text: PlaceholderReply}
	var failure string

	name, matched := p.phrases[NormalizePhrase(transcript)]
	if matched {
		reply.Path = tool.PathControlPhrase
		reply.Tool = name
		reply.Text, failure = p.callTool(ctx, req, name)
	}

	attrs := []any{
		"request_id", req.ID,
		"path", string(reply.Path),
		"tool", reply.Tool,
		"transcript", transcript,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	if failure != "" {
		p.logger.Error("request", append(attrs, "error", failure)...)
	} else {
		p.logger.Info("request", attrs...)
	}
	return reply
}

func (p *Pipeline) callTool(ctx context.Context, req tool.Request, name string) (string, string) {
	req.Path = tool.PathControlPhrase
	result, err := p.registry.Execute(tool.WithRequest(ctx, req), name, nil)
	if err != nil {
		return tool.FailureMessage(err), err.Error()
	}
	spoken, ok := p.registry.Speak(name, result)
	if !ok {
		err := fmt.Errorf("tool %q has no Speak renderer", name)
		return tool.DefaultFailureMessage, err.Error()
	}
	return spoken, ""
}

// NormalizePhrase trims, lowercases, collapses whitespace and drops trailing
// sentence punctuation so the Control phrase table matches typed transcripts.
func NormalizePhrase(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.TrimRight(text, "?.!")
	return strings.Join(strings.Fields(text), " ")
}
