// Package pipeline turns a typed transcript into a reply. Requests that the
// Fast path does not handle go to the Agent path.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// PlaceholderReply is returned when no path exists yet for a transcript.
const PlaceholderReply = "I don't know how to do that yet."

// reasonNoFocus reports a shared Control phrase that no Focus holder took, so
// it fell through to the semantic Fast path.
const reasonNoFocus = "no_focus"

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
	// Unavailable marks an Agent-path failure whose Text is the configured
	// Unavailable message. The reply session plays the error Cue before it.
	Unavailable bool
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

// RoutingWords is the wording one request routes with. The fields come from
// one settings snapshot, so a batch update cannot pair an old Control phrase
// table with new prefixes.
type RoutingWords struct {
	// ControlPhrases maps a normalized phrase to a canonical Tool name.
	ControlPhrases map[string]string
	// IntentPhrases maps a normalized phrase to one shared Focus intent. A
	// phrase in both maps is dropped by the live wording source, so it falls
	// through instead of choosing an owner at random.
	IntentPhrases map[string]string
	// RequestPrefixes are the Agent request prefixes, in any order.
	RequestPrefixes []string
}

// Wording is the live wording source. It is read once per request, after the
// pending-Confirmation stage, so a changed phrase or prefix applies to the
// next request without rebuilding the pipeline.
type Wording interface {
	Routing() RoutingWords
}

// WithWording makes Control phrases and Agent request prefixes live. Without
// it the values passed to New are fixed for the process lifetime.
func WithWording(w Wording) Option {
	return func(p *Pipeline) { p.wording = w }
}

// WithConfirmationResolver wires the pending-Confirmation stage.
func WithConfirmationResolver(resolver ConfirmationResolver) Option {
	return func(p *Pipeline) { p.confirmation = resolver }
}

// WithFocus enables shared Control-phrase intents. A phrase mapped to an
// intent is offered to whatever holds Focus; when nothing does, the utterance
// falls through to the semantic Fast path like any other request.
func WithFocus(registry *focus.Registry) Option {
	return func(p *Pipeline) { p.focus = registry }
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
	registry      *tool.Registry
	phrases       map[string]string
	intentPhrases map[string]string
	logger        *slog.Logger
	agent         AgentRunner
	prefixes      []string
	history       *agent.History
	semantic      *SemanticRouter
	wording       Wording

	confirmation ConfirmationResolver
	focus        *focus.Registry
}

// New builds a pipeline and rejects a Control phrase table that points at
// unknown Tools, Tools without a Speak renderer, or unknown Focus intents. A
// phrase value without a dot names one of the fixed intents; a canonical
// "module.tool" value names one Tool.
func New(reg *tool.Registry, phrases map[string]string, logger *slog.Logger, opts ...Option) (*Pipeline, error) {
	if reg == nil {
		return nil, fmt.Errorf("pipeline: registry is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	normalized := make(map[string]string, len(phrases))
	intents := make(map[string]string, len(phrases))
	for phrase, name := range phrases {
		key := NormalizePhrase(phrase)
		if key == "" {
			return nil, fmt.Errorf("pipeline: control phrase %q is empty", phrase)
		}
		target := strings.TrimSpace(name)
		if target == "" {
			return nil, fmt.Errorf("pipeline: control phrase %q has no target", phrase)
		}
		if prev, exists := normalized[key]; exists && prev != target {
			return nil, fmt.Errorf("pipeline: control phrase %q maps to both %q and %q", phrase, prev, target)
		}
		if prev, exists := intents[key]; exists && prev != target {
			return nil, fmt.Errorf("pipeline: control phrase %q maps to both %q and %q", phrase, prev, target)
		}
		if focus.IsIntentTarget(target) {
			if _, ok := focus.ParseIntent(target); !ok {
				return nil, fmt.Errorf("pipeline: control phrase %q points at unknown intent %q", phrase, target)
			}
			if prev, exists := normalized[key]; exists && prev != target {
				return nil, fmt.Errorf("pipeline: control phrase %q maps to both %q and %q", phrase, prev, target)
			}
			intents[key] = target
			continue
		}
		if prev, exists := intents[key]; exists && prev != target {
			return nil, fmt.Errorf("pipeline: control phrase %q maps to both %q and %q", phrase, prev, target)
		}
		info, ok := reg.Info(target)
		if !ok {
			return nil, fmt.Errorf("pipeline: control phrase %q points at unknown tool %q", phrase, target)
		}
		if !info.HasSpeak {
			return nil, fmt.Errorf("pipeline: control phrase %q points at %q, which has no Speak renderer", phrase, target)
		}
		if info.Permission == tool.PermissionConfirm {
			return nil, fmt.Errorf("pipeline: control phrase %q points at confirm tool %q", phrase, target)
		}
		normalized[key] = target
	}
	p := &Pipeline{registry: reg, phrases: normalized, intentPhrases: intents, logger: logger}
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
			// Settling the Confirmation is this request's routing decision.
			latency.SetStage(ctx, latency.StageRouting, time.Since(started))
			reply := res.Reply
			reply.RequestID = req.ID
			if reply.Path == "" {
				reply.Path = tool.PathConfirmation
			}
			if res.Call != nil {
				p.recordTurn(transcript, reply.Text, []agent.ToolCall{*res.Call})
			}
			record.reason = ""
			p.finish(ctx, started, req, transcript, reply, record)
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
		latency.SetStage(ctx, latency.StageRouting, time.Since(started))
		reply := Reply{Text: prefix, Prefix: prefix, Path: tool.PathConfirmation, RequestID: req.ID}
		record.reason = ""
		p.finish(ctx, started, req, transcript, reply, record)
		return reply
	}

	reply := p.route(ctx, req, transcript, events, &record)
	if prefix != "" {
		reply.Prefix = prefix
		reply.Text = strings.TrimSpace(prefix + " " + reply.Text)
	}
	p.finish(ctx, started, req, transcript, reply, record)
	return reply
}

// route applies the post-Confirmation steps in the fixed order: Agent request
// prefix, Control phrase, semantic Fast path, then the Agent path.
func (p *Pipeline) route(ctx context.Context, req tool.Request, transcript string, events Events, record *routeLog) Reply {
	routingStarted := time.Now()
	// Routing ends when the path is chosen. Marking is first-wins, so the
	// fall-through branches reach it once and only once per request.
	markRouting := func() { latency.SetStage(ctx, latency.StageRouting, time.Since(routingStarted)) }

	reply := Reply{Path: tool.PathFallthrough, RequestID: req.ID, Text: PlaceholderReply}
	normalized := NormalizePhrase(transcript)

	// Live wording is read once, from one snapshot, after the Confirmation
	// stage and before either routing step that uses it.
	phrases, intentPhrases, prefixes := p.phrases, p.intentPhrases, p.prefixes
	if p.wording != nil {
		words := p.wording.Routing()
		phrases, intentPhrases, prefixes = words.ControlPhrases, words.IntentPhrases, words.RequestPrefixes
	}

	// 2. Agent request prefix goes straight to the Agent path.
	if rest, ok := agent.MatchRequestPrefix(transcript, prefixes); ok {
		markRouting()
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

	// 3. Control phrase is an exact match and must not wait on embeddings. A
	// phrase may name one Tool, or a shared Focus intent that acts on whatever
	// currently holds Focus. With nothing in Focus the intent falls through to
	// the semantic Fast path below, exactly like an unmatched utterance.
	if target, ok := intentPhrases[normalized]; ok {
		intent, valid := focus.ParseIntent(target)
		if valid {
			if text, handled := p.focusIntent(ctx, intent); handled {
				markRouting()
				record.reason = ""
				reply.Path = tool.PathFocusIntent
				reply.Text = text
				return reply
			}
			// Keep the honest reason unless the semantic router replaces it.
			record.reason = reasonNoFocus
		}
	} else if name, ok := phrases[normalized]; ok {
		markRouting()
		record.reason = ""
		reply.Path = tool.PathControlPhrase
		reply.Tool = name
		reply.Text, record.failure, reply.Pending = p.callTool(ctx, req, transcript, name, tool.PathControlPhrase, nil)
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
			markRouting()
			record.reason = ""
			reply.Path = tool.PathFast
			reply.Tool = match.Tool
			reply.Text, record.failure, reply.Pending = p.callTool(ctx, req, transcript, match.Tool, tool.PathFast, match.Args)
			return reply
		}
	}

	// 5. Agent path, or the placeholder when no Agent runner is wired.
	if p.agent != nil {
		markRouting()
		p.callAgent(ctx, req, transcript, events, &reply)
		return reply
	}
	markRouting()
	if record.reason == "" {
		record.reason = reasonSemanticNoMatch
	}
	return reply
}

// focusIntent offers one shared intent to whatever holds Focus. It reports
// false when the seam is absent or no holder handles the intent.
func (p *Pipeline) focusIntent(ctx context.Context, intent focus.Intent) (string, bool) {
	if p.focus == nil {
		return "", false
	}
	return p.focus.HandleIntent(ctx, intent)
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
	reply.Unavailable = result.Unavailable
	// A spoken Unavailable message is still a reply, but the model call
	// failed; the raw request line must say so rather than look like success.
	switch {
	case result.Cancelled:
		// Barge-in or shutdown: the reply session classifies the request as
		// cancelled. Never label it a provider failure.
	case result.TimedOut:
		latency.SetAttr(ctx, "agent_error", "timeout")
		latency.MarkFailed(ctx)
	case result.Unavailable:
		latency.SetAttr(ctx, "agent_error", "unavailable")
		latency.MarkFailed(ctx)
	}
	names := make([]string, 0, len(result.Calls))
	for _, call := range result.Calls {
		names = append(names, call.Name)
	}
	reply.Tool = strings.Join(names, ",")
}

func (p *Pipeline) callTool(ctx context.Context, req tool.Request, transcript, name string, path tool.Path, args json.RawMessage) (string, string, *tool.PendingConfirmation) {
	toolStarted := time.Now()
	defer func() { latency.AddStage(ctx, latency.StageToolExecute, time.Since(toolStarted)) }()

	req.Path = path
	result, err := p.registry.Execute(tool.WithRequest(ctx, req), name, args)
	if err != nil {
		// A live Permission override can raise a Fast path Tool to confirm.
		// Arm the same one-turn Confirmation the Agent path uses, so the next
		// "yes" runs it instead of the user hearing a generic failure.
		var required *tool.ConfirmationRequiredError
		if errors.As(err, &required) {
			pending, proposeErr := p.registry.Propose(name, args)
			if proposeErr == nil {
				return pending.Summary, "", &pending
			}
			err = proposeErr
		}
		return tool.FailureMessage(err), err.Error(), nil
	}
	spoken, ok := p.registry.Speak(name, result)
	if !ok {
		err := fmt.Errorf("tool %q has no Speak renderer", name)
		return tool.DefaultFailureMessage, err.Error(), nil
	}
	p.recordToolTurn(transcript, spoken, name, result)
	return spoken, "", nil
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

// finish writes the single structured request log line. A request timeline
// owns that line when one is attached, so the stages the spoken session adds
// after playback are logged together; without one, as in a text-only caller,
// the pipeline logs the routing-only line itself.
func (p *Pipeline) finish(ctx context.Context, started time.Time, req tool.Request, transcript string, reply Reply, record routeLog) {
	if latency.FromContext(ctx) != nil {
		latency.SetRoute(ctx, string(reply.Path), reply.Tool, transcript)
		latency.SetAttr(ctx, "top_tools", record.top)
		latency.SetAttr(ctx, "margin", record.margin)
		latency.SetAttr(ctx, "extract", record.extraction)
		latency.SetAttr(ctx, "fallthrough_reason", record.reason)
		if record.embedErr != nil {
			latency.SetAttr(ctx, "embed_error", record.embedErr.Error())
		}
		if record.failure != "" {
			latency.SetFailure(ctx, record.failure)
			latency.MarkFailed(ctx)
		}
		return
	}

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
	return phrase.Normalize(text)
}
