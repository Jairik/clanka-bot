// Package pipeline turns a typed transcript into a reply, routing through the
// fixed order from issue #10: Confirmation, Agent request, Control phrase,
// semantic Fast path, then the Agent path.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// PlaceholderReply is spoken until the Agent path exists.
const PlaceholderReply = "I don't know how to do that yet."

// EmptyAgentRequestReply is spoken when an Agent request has no question.
const EmptyAgentRequestReply = "What would you like to ask?"

// Reply is the text answer plus routing details for the log and response.
type Reply struct {
	Text      string
	Tool      string
	Path      tool.Path
	RequestID string
}

// AgentPath handles an utterance routed to the general-purpose agent. It is
// wired by the Agent path ticket; nil means the placeholder reply.
type AgentPath interface {
	Handle(ctx context.Context, req tool.Request, transcript string) Reply
}

// ConfirmationResult is the pending-Confirmation stage's decision.
type ConfirmationResult struct {
	Handled bool   // the utterance answered the Confirmation
	Reply   Reply  // spoken when Handled
	Prefix  string // spoken before the normal route when not Handled
}

// ConfirmationResolver settles a pending Confirmation before any other check.
// A non-matching utterance returns Handled=false and an optional Prefix such
// as "Cancelled." so the new request is still routed (issue #10).
type ConfirmationResolver interface {
	Resolve(ctx context.Context, req tool.Request, transcript string) ConfirmationResult
}

// Option configures optional routing stages.
type Option func(*Pipeline)

// WithAgentPath wires the Agent path.
func WithAgentPath(agent AgentPath) Option {
	return func(p *Pipeline) { p.agent = agent }
}

// WithConfirmationResolver wires the pending-Confirmation stage.
func WithConfirmationResolver(resolver ConfirmationResolver) Option {
	return func(p *Pipeline) { p.confirmation = resolver }
}

// WithSemanticRouter enables semantic Fast path matching.
func WithSemanticRouter(router *SemanticRouter) Option {
	return func(p *Pipeline) { p.semantic = router }
}

// WithAgentPrefixes sets the Agent request prefixes, matched after normalisation.
func WithAgentPrefixes(prefixes []string) Option {
	return func(p *Pipeline) { p.agentPrefixes = append([]string(nil), prefixes...) }
}

// Pipeline routes one transcript at a time. It is safe for concurrent use.
type Pipeline struct {
	registry *tool.Registry
	phrases  map[string]string
	logger   *slog.Logger

	agentPrefixes []string
	semantic      *SemanticRouter
	agent         AgentPath
	confirmation  ConfirmationResolver
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
	return p, nil
}

// routeLog carries the fields the single request log line reports.
type routeLog struct {
	top        []ToolScore
	margin     float64
	extraction string
	reason     string
	embedErr   error
	failure    string
}

// Handle resolves one transcript. Internal errors are logged in full and only
// a generic message reaches the caller.
func (p *Pipeline) Handle(ctx context.Context, req tool.Request, transcript string) Reply {
	started := time.Now()
	record := routeLog{top: []ToolScore{}, extraction: "none", reason: reasonSemanticDisabled}

	// 1. Pending Confirmation owns the utterance when one is outstanding.
	prefix := ""
	if p.confirmation != nil {
		res := p.confirmation.Resolve(ctx, req, transcript)
		if res.Handled {
			reply := res.Reply
			reply.RequestID = req.ID
			if reply.Path == "" {
				reply.Path = tool.PathConfirmation
			}
			record.reason = ""
			p.finish(started, req, transcript, reply, record)
			return reply
		}
		// Not a clear yes: say "cancelled", then route the new request.
		prefix = strings.TrimSpace(res.Prefix)
	}

	reply := p.route(ctx, req, transcript, &record)
	if prefix != "" {
		reply.Text = strings.TrimSpace(prefix + " " + reply.Text)
	}
	p.finish(started, req, transcript, reply, record)
	return reply
}

// route applies steps 2-5 of the fixed order and returns the reply.
func (p *Pipeline) route(ctx context.Context, req tool.Request, transcript string, record *routeLog) Reply {
	reply := Reply{Path: tool.PathFallthrough, RequestID: req.ID, Text: PlaceholderReply}
	normalized := NormalizePhrase(transcript)

	// 2. Agent request prefix goes straight to the Agent path.
	if rest, ok := stripAgentPrefix(normalized, p.agentPrefixes); ok {
		record.reason = ""
		reply.Path = tool.PathAgent
		switch {
		case rest == "":
			reply.Text = EmptyAgentRequestReply
		case p.agent != nil:
			reply = p.agent.Handle(ctx, req, rest)
			if reply.Path == "" {
				reply.Path = tool.PathAgent
			}
			if reply.RequestID == "" {
				reply.RequestID = req.ID
			}
		default:
			reply.Text = PlaceholderReply
		}
		return reply
	}

	// 3. Control phrase is an exact match and must not wait on embeddings.
	if name, ok := p.phrases[normalized]; ok {
		record.reason = ""
		reply.Path = tool.PathControlPhrase
		reply.Tool = name
		reply.Text, record.failure = p.execute(ctx, req, name, tool.PathControlPhrase, nil)
		return reply
	}

	// 4. Semantic match, then extraction, then the Fast path.
	if p.semantic != nil {
		match := p.semantic.Match(ctx, transcript)
		record.top = match.Top
		record.margin = match.Margin
		record.extraction = match.Extraction
		record.reason = match.Reason
		record.embedErr = match.Err
		if match.Tool != "" {
			record.reason = ""
			reply.Path = tool.PathFast
			reply.Tool = match.Tool
			reply.Text, record.failure = p.execute(ctx, req, match.Tool, tool.PathFast, match.Args)
			return reply
		}
	}

	// 5. Agent path, or the placeholder until it lands.
	if p.agent != nil {
		agentReply := p.agent.Handle(ctx, req, transcript)
		if agentReply.Path == "" {
			agentReply.Path = tool.PathAgent
		}
		if agentReply.RequestID == "" {
			agentReply.RequestID = req.ID
		}
		return agentReply
	}
	if record.reason == "" {
		record.reason = reasonSemanticNoMatch
	}
	return reply
}

// execute runs one Tool with the path that chose it.
func (p *Pipeline) execute(ctx context.Context, req tool.Request, name string, path tool.Path, args json.RawMessage) (string, string) {
	req.Path = path
	result, err := p.registry.Execute(tool.WithRequest(ctx, req), name, args)
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

// finish writes the single structured log line for a request.
func (p *Pipeline) finish(started time.Time, req tool.Request, transcript string, reply Reply, record routeLog) {
	attrs := []any{
		"request_id", req.ID,
		"path", string(reply.Path),
		"tool", reply.Tool,
		"transcript", transcript,
		"duration_ms", time.Since(started).Milliseconds(),
		"top_tools", record.top,
		"margin", record.margin,
		"extract", record.extraction,
		"fallthrough_reason", record.reason,
	}
	if record.embedErr != nil {
		attrs = append(attrs, "embed_error", record.embedErr.Error())
	}
	if record.failure != "" {
		p.logger.Error("request", append(attrs, "error", record.failure)...)
		return
	}
	p.logger.Info("request", attrs...)
}

// stripAgentPrefix removes a configured prefix and returns the remaining
// request. A prefix must end at a word or punctuation boundary.
func stripAgentPrefix(normalized string, prefixes []string) (string, bool) {
	for _, prefix := range prefixes {
		p := NormalizePhrase(prefix)
		if p == "" || len(normalized) < len(p) {
			continue
		}
		if !strings.HasPrefix(normalized, p) {
			continue
		}
		rest := normalized[len(p):]
		if rest != "" {
			switch rest[0] {
			case ' ', ',', ';', ':':
			default:
				continue
			}
		}
		return strings.TrimSpace(strings.TrimLeft(rest, " ,;:")), true
	}
	return "", false
}

// NormalizePhrase trims, lowercases, collapses whitespace and drops trailing
// sentence punctuation so the Control phrase table matches typed transcripts.
func NormalizePhrase(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.TrimRight(text, "?.!")
	return strings.Join(strings.Fields(text), " ")
}
