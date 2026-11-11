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

// Events are optional callbacks for one streaming request. They are called
// from the routing goroutine, so they must not block.
type Events struct {
	// AgentPath runs once when routing picks the Agent path, before the model
	// call. The Agent path Filler plays here, while the first tokens are pending.
	AgentPath func()
	// Prefix runs before routing when a dropped Confirmation prepends
	// "Cancelled." so the spoken session can say it before any streamed text.
	Prefix func(string)
	// Delta receives content deltas as the model generates them.
	Delta func(string)
}

// ConfirmationResult is the pending-Confirmation stage's decision.
type ConfirmationResult struct {
	Handled bool  // the utterance answered the Confirmation
	Reply   Reply // spoken when Handled
	Prefix  string
	Call    *agent.ToolCall // the executed call, for the shared history
}

// ConfirmationResolver settles a pending Confirmation before any other check.
// A non-matching utterance returns Handled=false and an optional Prefix such
// as "Cancelled." so the new request is still routed (issue #10).
type ConfirmationResolver interface {
	Resolve(ctx context.Context, req tool.Request, transcript string) ConfirmationResult
}

// PendingDropper is the optional Barge-in seam (#25): it drops a waiting
// Confirmation so a stale yes cannot run later.
type PendingDropper interface {
	DropPending() (tool.PendingConfirmation, bool)
}

// Reply is the text answer plus routing details for the log and response.
// Prefix is the "Cancelled." lead-in when a dropped Confirmation was followed
// by another request; Text already includes it for text-only callers.
type Reply struct {
	Text      string
	Prefix    string
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

// WithSemanticRouter enables semantic Fast path matching.
func WithSemanticRouter(router *SemanticRouter) Option {
	return func(p *Pipeline) { p.semantic = router }
}

// WithConfirmationResolver wires the pending-Confirmation stage.
func WithConfirmationResolver(resolver ConfirmationResolver) Option {
	return func(p *Pipeline) { p.confirmation = resolver }
}

// DropPending discards a waiting Confirmation. Barge-in (#25) calls it when
// the wake word interrupts a reply so a stale yes cannot run later.
func (p *Pipeline) DropPending() (tool.PendingConfirmation, bool) {
	dropper, ok := p.confirmation.(PendingDropper)
	if !ok {
		return tool.PendingConfirmation{}, false
	}
	return dropper.DropPending()
}

// Pipeline routes one transcript at a time. It is safe for concurrent use.
type Pipeline struct {
	registry *tool.Registry
	phrases  map[string]string
	logger   *slog.Logger
	agent    AgentRunner
	prefixes []string
	history  *agent.History
	semantic *SemanticRouter

	confirmation ConfirmationResolver
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
		if info.Permission == tool.PermissionConfirm {
			return nil, fmt.Errorf("pipeline: control phrase %q points at confirm tool %q", phrase, name)
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
	return p.HandleEvents(ctx, req, transcript, Events{})
}

// HandleStream is Handle with a delta hook, kept for the plain streaming case.
func (p *Pipeline) HandleStream(ctx context.Context, req tool.Request, transcript string, onDelta func(string)) Reply {
	return p.HandleEvents(ctx, req, transcript, Events{Delta: onDelta})
}

// HandleEvents is Handle with routing and generation callbacks for one
// request. The callbacks are ignored on the Fast path.
func (p *Pipeline) HandleEvents(ctx context.Context, req tool.Request, transcript string, events Events) Reply {
	started := time.Now()
	record := routeLog{top: []ToolScore{}, extraction: "none", reason: reasonSemanticDisabled}

	// Order matches #10. A pending Confirmation comes first.
	prefix := ""
	if p.confirmation != nil {
		res := p.confirmation.Resolve(ctx, req, transcript)
		if res.Handled {
			reply := res.Reply
			reply.RequestID = req.ID
			if reply.Path == "" {
				reply.Path = tool.PathConfirmation
			}
			if res.Call != nil {
				p.recordTurn(transcript, reply.Text, []agent.ToolCall{*res.Call})
			}
			record.reason = ""
			p.finish(started, req, transcript, reply, record)
			return reply
		}
		// Not a clear yes: say "cancelled", then route the new request.
		prefix = strings.TrimSpace(res.Prefix)
	}

	// Say the cancellation lead-in before any routed reply is streamed.
	if prefix != "" && events.Prefix != nil {
		events.Prefix(prefix)
	}

	// Silence has nothing to route after it cancels the waiting call.
	if prefix != "" && strings.TrimSpace(transcript) == "" {
		reply := Reply{Text: prefix, Prefix: prefix, Path: tool.PathConfirmation, RequestID: req.ID}
		record.reason = ""
		p.finish(started, req, transcript, reply, record)
		return reply
	}

	reply := p.route(ctx, req, transcript, events, &record)
	if prefix != "" {
		reply.Prefix = prefix
		reply.Text = strings.TrimSpace(prefix + " " + reply.Text)
	}
	p.finish(started, req, transcript, reply, record)
	return reply
}

// route applies the post-Confirmation steps in the fixed order: Agent request
// prefix, Control phrase, semantic Fast path, then the Agent path.
func (p *Pipeline) route(ctx context.Context, req tool.Request, transcript string, events Events, record *routeLog) Reply {
	reply := Reply{Path: tool.PathFallthrough, RequestID: req.ID, Text: PlaceholderReply}
	normalized := NormalizePhrase(transcript)

	// 2. Agent request prefix goes straight to the Agent path.
	if rest, ok := agent.MatchRequestPrefix(transcript, p.prefixes); ok {
		record.reason = ""
		reply.Path = tool.PathAgent
		// An empty request makes no model call, so no Agent path cue plays.
		if rest == "" {
			reply.Text = agent.EmptyRequestReply
			p.recordTurn(transcript, reply.Text, nil)
		} else {
			p.callAgent(ctx, req, rest, events, &reply)
		}
		return reply
	}

	// 3. Control phrase is an exact match and must not wait on embeddings.
	if name, ok := p.phrases[normalized]; ok {
		record.reason = ""
		reply.Path = tool.PathControlPhrase
		reply.Tool = name
		reply.Text, record.failure = p.callTool(ctx, req, transcript, name, tool.PathControlPhrase, nil)
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
			reply.Text, record.failure = p.callTool(ctx, req, transcript, match.Tool, tool.PathFast, match.Args)
			return reply
		}
	}

	// 5. Agent path, or the placeholder until it lands.
	if p.agent != nil {
		p.callAgent(ctx, req, transcript, events, &reply)
		return reply
	}
	if record.reason == "" {
		record.reason = reasonSemanticNoMatch
	}
	return reply
}

func (p *Pipeline) callAgent(ctx context.Context, req tool.Request, transcript string, events Events, reply *Reply) {
	// The path is known before the model call, so the filler can start now.
	if events.AgentPath != nil {
		events.AgentPath()
	}
	result := p.agent.Run(ctx, req, transcript, events.Delta)
	reply.Path = tool.PathAgent
	reply.Text = result.Text
	reply.Pending = result.Pending
	names := make([]string, 0, len(result.Calls))
	for _, call := range result.Calls {
		names = append(names, call.Name)
	}
	reply.Tool = strings.Join(names, ",")
}

func (p *Pipeline) callTool(ctx context.Context, req tool.Request, transcript, name string, path tool.Path, args json.RawMessage) (string, string) {
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

// NormalizePhrase trims, lowercases, collapses whitespace and drops trailing
// sentence punctuation so the Control phrase table matches typed transcripts.
func NormalizePhrase(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.TrimRight(text, "?.!")
	return strings.Join(strings.Fields(text), " ")
}
