// Package pipeline turns a typed transcript into a reply. Requests that the
// Fast path does not handle go to the Agent path.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// PlaceholderReply is returned when no path exists yet for a transcript.
const PlaceholderReply = "I don't know how to do that yet."

// AgentRunner is the Agent path as the pipeline sees it. The method set is
// kept small so routing work (#19) can swap in a stub.
type AgentRunner interface {
	Run(ctx context.Context, req tool.Request, utterance string, onDelta func(string)) agent.Result
	History() *agent.History
}

// Reply is the text answer plus routing details for the log and response.
type Reply struct {
	Text      string
	Tool      string
	Path      tool.Path
	RequestID string
	Pending   *agent.PendingConfirm
}

// Option configures a Pipeline without changing the constructor signature.
type Option func(*Pipeline)

// WithAgent adds the Agent path and the Agent request prefixes that route
// straight to it.
func WithAgent(runner AgentRunner, requestPrefixes []string) Option {
	return func(p *Pipeline) {
		p.agent = runner
		p.prefixes = append([]string(nil), requestPrefixes...)
	}
}

// WithHistory records Fast path turns in the Agent path's shared history.
func WithHistory(history *agent.History) Option {
	return func(p *Pipeline) { p.history = history }
}

// Pipeline routes one transcript at a time. It is safe for concurrent use.
type Pipeline struct {
	registry *tool.Registry
	phrases  map[string]string
	logger   *slog.Logger
	agent    AgentRunner
	prefixes []string
	history  *agent.History
}

// New builds a pipeline and rejects a Control phrase table that points at
// unknown Tools or Tools without a Speak renderer.
func New(reg *tool.Registry, phrases map[string]string, logger *slog.Logger, opts ...Option) (*Pipeline, error) {
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
	p := &Pipeline{registry: reg, phrases: normalized, logger: logger}
	for _, opt := range opts {
		opt(p)
	}
	if p.agent != nil && p.history == nil {
		p.history = p.agent.History()
	}
	return p, nil
}

// Handle resolves one transcript. Internal errors are logged in full and only
// a generic message reaches the caller.
func (p *Pipeline) Handle(ctx context.Context, req tool.Request, transcript string) Reply {
	return p.HandleStream(ctx, req, transcript, nil)
}

// HandleStream is Handle with a streaming hook for the Agent path's content
// deltas. The hook is ignored on the Fast path.
func (p *Pipeline) HandleStream(ctx context.Context, req tool.Request, transcript string, onDelta func(string)) Reply {
	started := time.Now()
	reply := Reply{Path: tool.PathFallthrough, RequestID: req.ID, Text: PlaceholderReply}
	var failure string

	// Order matches #10. A pending Confirmation comes first once #24 lands.
	if p.agent == nil {
		if name, matched := p.phrases[NormalizePhrase(transcript)]; matched {
			reply.Path = tool.PathControlPhrase
			reply.Tool = name
			reply.Text, failure = p.callTool(ctx, req, transcript, name)
		}
	} else if rest, ok := agent.MatchRequestPrefix(transcript, p.prefixes); ok {
		reply.Path = tool.PathAgent
		if rest == "" {
			reply.Text = agent.EmptyRequestReply
			p.recordTurn(transcript, reply.Text, nil)
		} else {
			p.callAgent(ctx, req, rest, onDelta, &reply)
		}
	} else if name, matched := p.phrases[NormalizePhrase(transcript)]; matched {
		reply.Path = tool.PathControlPhrase
		reply.Tool = name
		reply.Text, failure = p.callTool(ctx, req, transcript, name)
	} else {
		reply.Path = tool.PathAgent
		p.callAgent(ctx, req, transcript, onDelta, &reply)
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

func (p *Pipeline) callAgent(ctx context.Context, req tool.Request, transcript string, onDelta func(string), reply *Reply) {
	result := p.agent.Run(ctx, req, transcript, onDelta)
	reply.Text = result.Text
	reply.Pending = result.Pending
	names := make([]string, 0, len(result.Calls))
	for _, call := range result.Calls {
		names = append(names, call.Name)
	}
	reply.Tool = strings.Join(names, ",")
}

func (p *Pipeline) callTool(ctx context.Context, req tool.Request, transcript, name string) (string, string) {
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
	p.recordToolTurn(transcript, spoken, name, result)
	return spoken, ""
}

// recordToolTurn keeps a successful Fast path turn in the shared history,
// including the compact Tool call and its result.
func (p *Pipeline) recordToolTurn(transcript, spoken, name string, result any) {
	payload, err := json.Marshal(result)
	if err != nil {
		payload = nil
	}
	info, _ := p.registry.Info(name)
	p.recordTurn(transcript, spoken, []agent.ToolCall{{
		Name:      name,
		Result:    payload,
		LocalOnly: info.LocalOnly,
	}})
}

func (p *Pipeline) recordTurn(transcript, spoken string, calls []agent.ToolCall) {
	if p.history == nil {
		return
	}
	p.history.Add(agent.Turn{Utterance: transcript, Reply: spoken, Calls: calls})
}

// NormalizePhrase trims, lowercases, collapses whitespace and drops trailing
// sentence punctuation so the Control phrase table matches typed transcripts.
func NormalizePhrase(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.TrimRight(text, "?.!")
	return strings.Join(strings.Fields(text), " ")
}
