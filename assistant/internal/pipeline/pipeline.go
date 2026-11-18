// Package pipeline turns a typed transcript into a reply. Requests that the
// Fast path does not handle go to the Agent path.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/briefing"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// PlaceholderReply is returned when no path exists yet for a transcript.
const PlaceholderReply = "I don't know how to do that yet."

// Preserve the owner's task text, including case and quoted names.
var hermesPrefix = regexp.MustCompile(`(?is)^\s*ask\s+hermes(?:[\s,!.?:]+|$)(.*)$`)

// reasonNoFocus reports a shared Control phrase that no Focus holder took, so
// it fell through to the semantic Fast path.
const reasonNoFocus = "no_focus"

// reasonAgentOnlyTool reports a live Control phrase that named an AgentOnly
// Tool, which the Fast path must never execute.
const reasonAgentOnlyTool = "agent_only_tool"

// AgentRunner is the Agent path as the pipeline sees it. The method set is
// kept small so routing work (#19) can swap in a stub.
type AgentRunner interface {
	Run(ctx context.Context, req tool.Request, utterance string, onDelta func(string)) agent.Result
	History() *agent.History
}

// Events are optional callbacks for one streaming request. They are called
// from the routing goroutine, so they must not block.
type Events struct {
	// StepReply synchronously finishes a chain step before the next Tool runs.
	StepReply func(string) error
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
	// LocalOnly reports that the reply text is Local-only and must not leave
	// the Pi. It follows the result's own provenance, so a repeat of a public
	// reply is public while a repeat of a private one stays Local-only.
	LocalOnly bool
	// Unavailable marks an Agent-path failure whose Text is the configured
	// Unavailable message. The reply session plays the error Cue before it.
	Unavailable bool
	// TextNotStreamed marks an Agent fallback line that no delta carried, so
	// the reply session must speak it even after a streamed preamble.
	TextNotStreamed bool
	// Ignored marks a turn the model decided was not addressed to the
	// Assistant. Text is empty, and the reply session speaks no words: it may
	// play the ignore Cue. The turn is not the Last reply.
	Ignored bool
	// Suspend marks a standalone "stop" that ends the session instead of
	// answering. Text may be the Focus holder's line; an empty Text speaks
	// nothing, and the reply session may play the same soft Cue as Ignored. The
	// caller closes the follow-up window and clears the shared history (#501).
	Suspend bool
	// SpeechRate is a per-utterance TTS multiplier for this reply. Zero means
	// "use the live voice.rate setting"; a non-zero value (set by a Tool whose
	// result implements speechRater, e.g. a faster/slower repeat) applies to
	// this one spoken reply only and is stored with it so a follow-up can step.
	SpeechRate float64
}

// speechRater is implemented by a Tool result that carries a one-utterance TTS
// rate override. It is a per-Out method, so no Tool interface or registry
// change is needed; a result that does not implement it keeps the setting.
type speechRater interface {
	SpeechRate() float64
}

// rateOf returns the per-utterance rate a Tool result requests, or 0 for none.
func rateOf(result any) float64 {
	rater, ok := result.(speechRater)
	if !ok {
		return 0
	}
	return rater.SpeechRate()
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

// WithHermes receives explicit Hermes requests without using Agent history.
func WithHermes(handle func(context.Context, string) string) Option {
	return func(p *Pipeline) { p.hermes = handle }
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
// intent is offered to whatever holds Focus; when nothing does, a bare "stop"
// ends the session and every other intent falls through to the semantic Fast
// path like any other request.
func WithFocus(registry *focus.Registry) Option {
	return func(p *Pipeline) { p.focus = registry }
}

// WithLastReply records every completed final reply with its provenance, so a
// follow-up Tool such as assistant.repeat or notify.phone can resolve "that"
// without re-routing the previous turn. The store is in-memory; nil disables
// the seam. A cancelled or blank turn does not replace the previous record.
func WithLastReply(store *lastreply.Store) Option {
	return func(p *Pipeline) { p.lastReply = store }
}

// WithConversationTurn records every request while conversation mode is on, so
// its idle window measures all turns, not only Agent turns. touch must be a
// no-op when the mode is off; a nil source keeps the pipeline unchanged.
func WithConversationTurn(touch func()) Option {
	return func(p *Pipeline) { p.conversationTurn = touch }
}

// WithBriefing wires the morning-briefing stage. When set, route matches a
// normalised exact phrase against the composer before the deferred and chain
// stages, and replies with one composed, Local-only reply.
func WithBriefing(composer *briefing.Composer) Option {
	return func(p *Pipeline) { p.briefing = composer }
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
	hermes        func(context.Context, string) string
	prefixes      []string
	history       *agent.History
	semantic      *SemanticRouter
	wording       Wording

	confirmation ConfirmationResolver
	focus        *focus.Registry
	lastReply    *lastreply.Store
	chainEnabled func() bool
	deferred     *DeferredQueue
	briefing     *briefing.Composer
	// conversationTurn records a request while conversation mode is on, so the
	// mode's idle clock covers every turn rather than only Agent turns.
	conversationTurn func()
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
	// Reject conflicting spellings before target validation, so map order
	// cannot choose between an ambiguity error and an unknown-target error.
	targets := make(map[string]string, len(phrases))
	for phrase, name := range phrases {
		key := NormalizePhrase(phrase)
		if key == "" {
			return nil, fmt.Errorf("pipeline: control phrase %q is empty", phrase)
		}
		target := strings.TrimSpace(name)
		if target == "" {
			return nil, fmt.Errorf("pipeline: control phrase %q has no target", phrase)
		}
		if prev, exists := targets[key]; exists && prev != target {
			return nil, fmt.Errorf("pipeline: control phrase %q maps to both %q and %q", phrase, prev, target)
		}
		targets[key] = target
	}
	for phrase, name := range phrases {
		key := NormalizePhrase(phrase)
		target := strings.TrimSpace(name)
		if focus.IsIntentTarget(target) {
			if _, ok := focus.ParseIntent(target); !ok {
				return nil, fmt.Errorf("pipeline: control phrase %q points at unknown intent %q", phrase, target)
			}
			intents[key] = target
			continue
		}
		info, ok := reg.Info(target)
		if !ok {
			return nil, fmt.Errorf("pipeline: control phrase %q points at unknown tool %q", phrase, target)
		}
		if !info.HasSpeak {
			return nil, fmt.Errorf("pipeline: control phrase %q points at %q, which has no Speak renderer", phrase, target)
		}
		if info.AgentOnly {
			// A Control phrase is a Fast-path route; an AgentOnly Tool must
			// never be reachable that way, even if it declares a renderer.
			return nil, fmt.Errorf("pipeline: control phrase %q points at AgentOnly tool %q", phrase, target)
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
	// Every request is a turn for conversation mode, whatever path it takes.
	// The hook is a no-op while the mode is off.
	if p.conversationTurn != nil {
		p.conversationTurn()
	}
	record := routeLog{top: []ToolScore{}, extraction: "none", reason: reasonSemanticDisabled}

	// Order matches #10. A pending Confirmation comes first. Smoke turns never
	// resolve or cancel an existing confirmation.
	prefix := ""
	var isSmoke bool
	if tl := latency.FromContext(ctx); tl != nil && tl.Source() == latency.SourceSmoke {
		isSmoke = true
	}
	if p.confirmation != nil && !isSmoke {
		res := p.confirmation.Resolve(ctx, req, transcript)
		if res.Handled {
			// Settling the Confirmation is this request's routing decision.
			latency.SetStage(ctx, latency.StageRouting, time.Since(started))
			reply := res.Reply
			reply.RequestID = req.ID
			if reply.Path == "" {
				reply.Path = tool.PathConfirmation
			}
			// Preserve provenance: a confirmed Local-only Tool's result must
			// not be forwarded off the Pi by a later follow-up.
			if res.Call != nil && res.Call.LocalOnly {
				reply.LocalOnly = true
			}
			if res.Call != nil {
				// The Dashboard's Activity view gets the executed call and its
				// spoken result. Local-only results stay in memory and on screen.
				latency.AddToolCall(ctx, latency.ToolCallDetail{
					Name:      res.Call.Name,
					LocalOnly: res.Call.LocalOnly,
					Result:    reply.Text,
				})
				p.recordTurn(ctx, transcript, reply.Text, []agent.ToolCall{*res.Call})
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
// routeKind classifies the routing decision made by planRoute.
type routeKind int

const (
	routeKindConfirmation routeKind = iota
	routeKindHermes
	routeKindPrefix
	routeKindBriefing
	routeKindControlPhrase
	routeKindFocusIntent
	routeKindDeferred
	routeKindChain
	routeKindSemantic
	routeKindAgent
	routeKindFallthrough
)

// RoutingDecision carries the side-effect-free routing decision and diagnostics.
type RoutingDecision struct {
	routeKind         routeKind
	Path              tool.Path       `json:"path"`
	Tool              string          `json:"tool,omitempty"`
	Args              json.RawMessage `json:"args,omitempty"`
	Top               []ToolScore     `json:"top"`
	Margin            float64         `json:"margin"`
	Extraction        string          `json:"extract"`
	Reason            string          `json:"fallthrough_reason,omitempty"`
	ChainSplit        bool            `json:"chain_split"`
	FocusOwner        string          `json:"focus_owner,omitempty"`
	Decision          string          `json:"decision,omitempty"`
	prefixRest        string
	intent            focus.Intent
	deferredEvent     string
	deferredAction    string
	deferredCancel    bool
	deferredFallback  string
	chainSteps        []chainStep
	embedErr          error
}

// focusOwner reports the name of the currently held Focus priority, or "" if none.
func (p *Pipeline) focusOwner() string {
	if p.focus == nil {
		return ""
	}
	for _, prio := range []focus.Priority{focus.Ring, focus.Reply, focus.Music} {
		if p.focus.Held(prio) {
			return prio.String()
		}
	}
	return ""
}

// wordingRouting returns the current snapshot of control phrases, intent phrases, and request prefixes.
func (p *Pipeline) wordingRouting() (map[string]string, map[string]string, []string) {
	phrases, intentPhrases, prefixes := p.phrases, p.intentPhrases, p.prefixes
	if p.wording != nil {
		words := p.wording.Routing()
		phrases, intentPhrases, prefixes = words.ControlPhrases, words.IntentPhrases, words.RequestPrefixes
	}
	return phrases, intentPhrases, prefixes
}

// planRoute resolves the routing decision order without executing any side effects.
func (p *Pipeline) planRoute(ctx context.Context, transcript string) RoutingDecision {
	// A pending Confirmation is checked first: a clear yes settles it.
	if p.confirmation != nil && p.registry != nil {
		if pending, ok := p.registry.Pending(); ok && isClearYes(transcript) {
			return RoutingDecision{
				routeKind:  routeKindConfirmation,
				Path:       tool.PathConfirmation,
				Tool:       pending.Tool,
				Args:       pending.Args,
				FocusOwner: p.focusOwner(),
				Decision:   "would settle a pending Confirmation for " + pending.Tool,
			}
		}
	}

	normalized := NormalizePhrase(transcript)

	// A disabled Hermes still owns its prefix so it never falls through to a model.
	if match := hermesPrefix.FindStringSubmatch(transcript); match != nil {
		return RoutingDecision{
			routeKind:  routeKindHermes,
			Path:       tool.PathHermes,
			prefixRest: strings.TrimSpace(match[1]),
			FocusOwner: p.focusOwner(),
			Decision:   "explicit Hermes request",
		}
	}

	// Live wording is read once, from one snapshot, after the Confirmation
	// stage and before either routing step that uses it.
	phrases, intentPhrases, prefixes := p.wordingRouting()

	// Agent request prefix goes straight to the Agent path.
	if rest, ok := agent.MatchRequestPrefix(transcript, prefixes); ok {
		return RoutingDecision{
			routeKind:  routeKindPrefix,
			Path:       tool.PathAgent,
			prefixRest: rest,
			FocusOwner: p.focusOwner(),
			Decision:   "agent request prefix",
		}
	}

	// Morning briefing: a fixed phrase composes several Tools into one spoken
	// reply, before the deferred, chain and Agent stages can claim it.
	if p.briefing != nil && p.briefing.Matches(normalized) {
		return RoutingDecision{
			routeKind:  routeKindBriefing,
			Path:       tool.PathBriefing,
			FocusOwner: p.focusOwner(),
			Decision:   "morning briefing",
		}
	}

	var recordReason string

	// Control phrase: an exact phrase match for a registered tool.
	if name, ok := phrases[normalized]; ok {
		info, found := p.registry.Info(name)
		if found && info.AgentOnly {
			recordReason = reasonAgentOnlyTool
		} else {
			var args json.RawMessage
			if found && info.HasExtract {
				if extracted, extractedOK := p.registry.Extract(name, transcript); extractedOK {
					args = extracted
				}
			}
			return RoutingDecision{
				routeKind:  routeKindControlPhrase,
				Path:       tool.PathControlPhrase,
				Tool:       name,
				Args:       args,
				FocusOwner: p.focusOwner(),
				Decision:   "control phrase: " + name,
			}
		}
	}

	// Focus intent: an exact phrase mapped to a shared focus action.
	if target, ok := intentPhrases[normalized]; ok {
		intent, valid := focus.ParseIntent(target)
		if valid {
			owner := p.focusOwner()
			if owner != "" {
				return RoutingDecision{
					routeKind:  routeKindFocusIntent,
					Path:       tool.PathFocusIntent,
					intent:     intent,
					FocusOwner: owner,
					Decision:   "focus intent: " + string(intent) + " (owner: " + owner + ")",
				}
			}
			// A bare "stop" still ends the session when nothing holds Focus.
			// It must not reach the semantic router or the Agent path, so it
			// keeps the Focus route and the execution below suspends (#501).
			// Every other shared intent keeps its fall-through.
			if intent == focus.Stop {
				return RoutingDecision{
					routeKind: routeKindFocusIntent,
					Path:      tool.PathFocusIntent,
					intent:    intent,
					Decision:  "stop with nothing in focus",
				}
			}
			recordReason = reasonNoFocus
		}
	}

	// Explicit deferred requests precede ordinary Tool routing.
	if event, action, cancel, fallback, ok := p.deferredPlan(ctx, transcript, phrases, intentPhrases); ok {
		if cancel {
			return RoutingDecision{
				routeKind:      routeKindDeferred,
				Path:           tool.PathControlPhrase,
				deferredCancel: true,
				FocusOwner:     p.focusOwner(),
				Decision:       "would cancel queued actions",
			}
		}
		if fallback != "" {
			return RoutingDecision{
				routeKind:        routeKindDeferred,
				Path:             tool.PathAgent,
				deferredFallback: fallback,
				FocusOwner:       p.focusOwner(),
				Decision:         "agent fallback (ambiguous deferred)",
			}
		}
		return RoutingDecision{
			routeKind:      routeKindDeferred,
			Path:           tool.PathControlPhrase,
			deferredEvent:  event,
			deferredAction: action,
			FocusOwner:     p.focusOwner(),
			Decision:       "would defer to " + event,
		}
	}

	// Validate chain before semantic matching.
	if p.chainEnabled != nil && p.chainEnabled() {
		if steps := p.chainPlan(ctx, transcript, phrases, intentPhrases); len(steps) > 1 {
			names := make([]string, 0, len(steps))
			for _, s := range steps {
				if s.name != "" {
					names = append(names, s.name)
				} else if s.intent != "" {
					names = append(names, string(s.intent))
				}
			}
			return RoutingDecision{
				routeKind:  routeKindChain,
				Path:       tool.PathFast,
				Tool:       strings.Join(names, ","),
				ChainSplit: true,
				chainSteps: steps,
				FocusOwner: p.focusOwner(),
				Decision:   "chain split: " + strings.Join(names, " -> "),
			}
		}
	}

	// Semantic match, then extraction, then the Fast path.
	var top []ToolScore
	var margin float64
	var extraction string
	reason := recordReason
	if reason == "" {
		reason = reasonSemanticDisabled
	}
	var embedErr error

	if p.semantic != nil {
		match := p.semantic.Match(ctx, transcript)
		top = match.Top
		margin = match.Margin
		extraction = match.Extraction
		reason = match.Reason
		embedErr = match.Err

		if match.Tool != "" {
			if p.secondCommandClause(transcript, phrases, intentPhrases) {
				reason = reasonCompound
			} else {
				return RoutingDecision{
					routeKind:  routeKindSemantic,
					Path:       tool.PathFast,
					Tool:       match.Tool,
					Args:       match.Args,
					Top:        top,
					Margin:     margin,
					Extraction: extraction,
					Reason:     "",
					FocusOwner: p.focusOwner(),
					Decision:   "fast path: " + match.Tool,
					embedErr:   embedErr,
				}
			}
		}
	}

	// Agent fallback is the final step when no earlier route matches.
	if reason == "" {
		reason = reasonSemanticNoMatch
	}
	return RoutingDecision{
		routeKind:  routeKindAgent,
		Path:       tool.PathAgent,
		Top:        top,
		Margin:     margin,
		Extraction: extraction,
		Reason:     reason,
		FocusOwner: p.focusOwner(),
		Decision:   "agent fallback",
		embedErr:   embedErr,
	}
}

// Explain computes the pure, side-effect-free routing decision for transcript.
func (p *Pipeline) Explain(ctx context.Context, transcript string) RoutingDecision {
	// The plan is deliberately not executed: no tools are called, confirmations
	// are not settled, actions are not queued, and chains are not run.
	return p.planRoute(ctx, transcript)
}

// route executes the decision made by planRoute.
func (p *Pipeline) route(ctx context.Context, req tool.Request, transcript string, events Events, record *routeLog) Reply {
	var isSmoke bool
	if tl := latency.FromContext(ctx); tl != nil && tl.Source() == latency.SourceSmoke {
		isSmoke = true
	}
	routingStarted := time.Now()
	var routingWork time.Duration
	markRouting := func() {
		latency.SetStage(ctx, latency.StageRouting, time.Since(routingStarted)-routingWork)
	}

	reply := Reply{Path: tool.PathFallthrough, RequestID: req.ID, Text: PlaceholderReply}

	decision := p.planRoute(ctx, transcript)
	record.top = decision.Top
	record.margin = decision.Margin
	record.extraction = decision.Extraction
	record.reason = decision.Reason
	record.embedErr = decision.embedErr

	// Smoke may inspect any route, but only the clock read may execute.
	if isSmoke && (decision.Tool != "time.now" || (decision.routeKind != routeKindControlPhrase && decision.routeKind != routeKindSemantic)) {
		markRouting()
		reply.Path = decision.Path
		reply.Tool = decision.Tool
		reply.Text = "Unexpected route skipped for smoke."
		return reply
	}

	switch decision.routeKind {
	case routeKindConfirmation:
		markRouting()
		record.reason = ""
		if p.confirmation != nil {
			res := p.confirmation.Resolve(ctx, req, transcript)
			if res.Handled {
				reply = res.Reply
				reply.RequestID = req.ID
				if reply.Path == "" {
					reply.Path = tool.PathConfirmation
				}
				if res.Call != nil && res.Call.LocalOnly {
					reply.LocalOnly = true
				}
				if res.Call != nil {
					latency.AddToolCall(ctx, latency.ToolCallDetail{
						Name:      res.Call.Name,
						LocalOnly: res.Call.LocalOnly,
						Result:    reply.Text,
					})
					p.recordTurn(ctx, transcript, reply.Text, []agent.ToolCall{*res.Call})
				}
				return reply
			}
		}
		return reply

	case routeKindHermes:
		markRouting()
		record.reason = ""
		reply.Path, reply.LocalOnly = tool.PathHermes, true
		reply.Text = "Hermes is disabled."
		if p.hermes != nil {
			reply.Text = p.hermes(ctx, decision.prefixRest)
		}
		return reply

	case routeKindPrefix:
		markRouting()
		record.reason = ""
		reply.Path = tool.PathAgent
		if decision.prefixRest == "" {
			reply.Text = agent.EmptyRequestReply
			p.recordTurn(ctx, transcript, reply.Text, nil)
		} else if isSmoke {
			reply.Text = "Agent call skipped for smoke."
		} else {
			p.callAgent(ctx, req, decision.prefixRest, events, &reply)
		}
		return reply

	case routeKindBriefing:
		markRouting()
		record.reason = ""
		reply.Path = tool.PathBriefing
		result := p.briefing.Compose(ctx)
		localOnly := false
		calls := make([]agent.ToolCall, 0, len(result.Parts))
		for _, part := range result.Parts {
			if part.LocalOnly {
				localOnly = true
			}
			payload, _ := json.Marshal(part.Text)
			calls = append(calls, agent.ToolCall{Name: part.Name, Result: payload, LocalOnly: part.LocalOnly})
			latency.AddToolCall(ctx, latency.ToolCallDetail{Name: part.Name, LocalOnly: part.LocalOnly, Result: part.Text})
		}
		reply.LocalOnly = localOnly
		reply.Text = result.Text
		p.recordTurn(ctx, transcript, reply.Text, calls)
		return reply

	case routeKindControlPhrase:
		markRouting()
		record.reason = ""
		reply.Path = tool.PathControlPhrase
		reply.Tool = decision.Tool
		reply.Text, record.failure, reply.Pending, reply.LocalOnly, reply.SpeechRate = p.callTool(ctx, req, transcript, decision.Tool, tool.PathControlPhrase, decision.Args)
		return reply

	case routeKindFocusIntent:
		text, intentHandled, intentTook := p.focusIntent(ctx, decision.intent)
		routingWork += intentTook
		if intentHandled {
			markRouting()
			record.reason = ""
			reply.Path = tool.PathFocusIntent
			reply.Text = text
			// A standalone "stop" ends the session even when Focus handled it,
			// so the next wake word starts with a fresh history (#501).
			reply.Suspend = decision.intent == focus.Stop
			return reply
		}
		// A bare "stop" with nothing in Focus must not reach the semantic
		// router or the Agent path. Nothing is spoken; the optional soft Cue is
		// the only sound, and the caller then ends the session (#501). Any other
		// shared intent keeps its existing fall-through.
		if decision.intent == focus.Stop {
			markRouting()
			record.reason = ""
			reply.Path = tool.PathFocusIntent
			reply.Text = ""
			reply.Suspend = true
			return reply
		}
		record.reason = reasonNoFocus
		if p.semantic != nil {
			match := p.semantic.Match(ctx, transcript)
			record.top = match.Top
			record.margin = match.Margin
			record.extraction = match.Extraction
			record.reason = match.Reason
			record.embedErr = match.Err
			if match.Tool != "" {
				phrases, intentPhrases, _ := p.wordingRouting()
				if p.secondCommandClause(transcript, phrases, intentPhrases) {
					record.reason = reasonCompound
				} else {
					markRouting()
					record.reason = ""
					reply.Path = tool.PathFast
					reply.Tool = match.Tool
					reply.Text, record.failure, reply.Pending, reply.LocalOnly, reply.SpeechRate = p.callTool(ctx, req, transcript, match.Tool, tool.PathFast, match.Args)
					return reply
				}
			}
		}
		markRouting()
		if p.agent != nil {
			if isSmoke {
				reply.Path = tool.PathAgent
				reply.Text = "Agent call skipped for smoke."
				return reply
			}
			p.callAgent(ctx, req, transcript, events, &reply)
			return reply
		}
		return reply

	case routeKindDeferred:
		markRouting()
		record.reason = ""
		if decision.deferredCancel {
			p.deferred.Cancel(0)
			reply.Path = tool.PathControlPhrase
			reply.Text = "Cancelled all queued actions."
			return reply
		}
		if decision.deferredFallback != "" {
			p.callAgent(ctx, req, decision.deferredFallback, events, &reply)
			return reply
		}
		reply.Path = tool.PathControlPhrase
		if p.deferred.Add(decision.deferredEvent, decision.deferredAction) {
			reply.Text = "Queued for the end of " + decision.deferredEvent + ". Say cancel queued actions to cancel. It expires in one hour."
		} else {
			reply.Text = "The action queue is full. Cancel a queued action first."
		}
		return reply

	case routeKindChain:
		markRouting()
		record.reason = ""
		return p.runChain(ctx, req, decision.chainSteps, events, record)

	case routeKindSemantic:
		markRouting()
		record.reason = ""
		reply.Path = tool.PathFast
		reply.Tool = decision.Tool
		reply.Text, record.failure, reply.Pending, reply.LocalOnly, reply.SpeechRate = p.callTool(ctx, req, transcript, decision.Tool, tool.PathFast, decision.Args)
		return reply

	case routeKindAgent:
		markRouting()
		if isSmoke {
			reply.Path = tool.PathAgent
			reply.Text = "Agent call skipped for smoke."
			return reply
		}
		if p.agent != nil {
			p.callAgent(ctx, req, transcript, events, &reply)
			return reply
		}
		return reply

	case routeKindFallthrough:
		markRouting()
		return reply

	default:
		markRouting()
		return reply
	}
}

// focusIntent offers one shared intent to whatever holds Focus. It reports
// false when the seam is absent or no holder handles the intent, and how long
// the offer took so the caller can keep that action out of routing. The offer
// is recorded as tool_execute because the user waits on it exactly like a Tool
// call, even though a Focus holder is not one.
func (p *Pipeline) focusIntent(ctx context.Context, intent focus.Intent) (string, bool, time.Duration) {
	if p.focus == nil {
		return "", false, 0
	}
	intentStarted := time.Now()
	text, handled := p.focus.HandleIntent(ctx, intent)
	took := time.Since(intentStarted)
	latency.AddStage(ctx, latency.StageToolExecute, took)
	return text, handled, took
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
	reply.TextNotStreamed = result.TextNotStreamed
	reply.Ignored = result.Ignored
	// The conversation kind is display-only, but Activity renders it per turn
	// from the same completion as the route and the token counts.
	latency.SetConversation(ctx, result.Conversation)
	// The single provider retry, when it happened, is visible on the request
	// line so a slow turn can be explained without reading the debug log.
	latency.SetAttr(ctx, "agent_retries", result.Retries)
	// The spoken-length budget cut this reply at a sentence boundary. It is a
	// successful answer, but the raw request line says why it was short.
	if result.Truncated {
		latency.SetAttr(ctx, "agent_truncated", true)
	}
	// Per-turn model token counts, summed across steps by the Agent path.
	// Counts and step count only: no prompt or reply text is logged here.
	latency.SetTokens(ctx, result.PromptTokens, result.CompletionTokens, result.TotalTokens, result.CachedTokens, result.Steps)
	// A spoken Unavailable message is still a reply, but the model call
	// failed; the raw request line must say so rather than look like success.
	switch {
	case result.Cancelled:
		// Barge-in or shutdown: the reply session classifies the request as
		// cancelled. Never label it a provider failure.
	case result.MaxSteps:
		// The step-limit line is spoken, but the request did not complete.
		latency.SetAttr(ctx, "agent_error", "max_steps")
		latency.MarkFailed(ctx)
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
		// The Agent path never calls a Local-only Tool; the flag is still
		// carried so the Activity display has one provenance rule everywhere.
		latency.AddToolCall(ctx, latency.ToolCallDetail{
			Name:      call.Name,
			LocalOnly: call.LocalOnly,
			Result:    string(call.Result),
		})
	}
	if result.Pending != nil {
		info, _ := p.registry.Info(result.Pending.Tool)
		latency.AddToolCall(ctx, latency.ToolCallDetail{
			Name:      result.Pending.Tool,
			LocalOnly: info.LocalOnly,
			Result:    "Confirmation required.",
		})
	}
	reply.Tool = strings.Join(names, ",")
}

func (p *Pipeline) callTool(ctx context.Context, req tool.Request, transcript, name string, path tool.Path, args json.RawMessage) (string, string, *tool.PendingConfirmation, bool, float64) {
	toolStarted := time.Now()
	defer func() { latency.AddStage(ctx, latency.StageToolExecute, time.Since(toolStarted)) }()

	info, _ := p.registry.Info(name)
	req.Path = path
	result, err := p.registry.Execute(tool.WithRequest(ctx, req), name, args)
	if err != nil {
		// A live Permission override can raise a Fast path Tool to confirm.
		// Arm the same one-turn Confirmation the Agent path uses, so the next
		// "yes" runs it instead of the user hearing a generic failure.
		var required *tool.ConfirmationRequiredError
		if errors.As(err, &required) {
			var isSmoke bool
			if tl := latency.FromContext(ctx); tl != nil && tl.Source() == latency.SourceSmoke {
				isSmoke = true
			}
			if !isSmoke {
				pending, proposeErr := p.registry.Propose(name, args)
				if proposeErr == nil {
					// The Tool did not run; still show which call is waiting.
					latency.AddToolCall(ctx, latency.ToolCallDetail{
						Name:      name,
						LocalOnly: info.LocalOnly,
						Result:    "Confirmation required.",
					})
					return pending.Summary, "", &pending, info.LocalOnly, 0
				}
				err = proposeErr
			}
		}
		message := tool.FailureMessage(err)
		latency.AddToolCall(ctx, latency.ToolCallDetail{
			Name:      name,
			LocalOnly: info.LocalOnly,
			Result:    message,
		})
		return message, err.Error(), nil, info.LocalOnly, 0
	}
	// A declared Provenance hook can give one replayed text different provenance
	// from its Tool's static flag: assistant.repeat is Local-only so the Agent
	// never sees it, yet the text it repeats keeps the flag it had when spoken.
	// Only a Tool that declared the hook at registration can change the flag.
	localOnly := p.registry.Provenance(name, result)
	spoken, ok := p.registry.Speak(name, result)
	if !ok {
		err := fmt.Errorf("tool %q has no Speak renderer", name)
		latency.AddToolCall(ctx, latency.ToolCallDetail{
			Name:      name,
			LocalOnly: localOnly,
			Result:    tool.DefaultFailureMessage,
		})
		return tool.DefaultFailureMessage, err.Error(), nil, localOnly, 0
	}
	latency.AddToolCall(ctx, latency.ToolCallDetail{
		Name:      name,
		LocalOnly: localOnly,
		Result:    spoken,
	})
	p.recordToolTurn(ctx, transcript, spoken, name, result, localOnly)
	return spoken, "", nil, localOnly, rateOf(result)
}

// recordToolTurn keeps a successful Fast path turn in the shared history,
// including the compact Tool call and its result.
func (p *Pipeline) recordToolTurn(ctx context.Context, transcript, spoken, name string, result any, localOnly bool) {
	payload, err := json.Marshal(result)
	if err != nil {
		payload = nil
	}
	p.recordTurn(ctx, transcript, spoken, []agent.ToolCall{{
		Name:      name,
		Result:    payload,
		LocalOnly: localOnly,
	}})
}

func (p *Pipeline) recordTurn(ctx context.Context, transcript, spoken string, calls []agent.ToolCall) {
	if tl := latency.FromContext(ctx); tl != nil && tl.Source() == latency.SourceSmoke {
		return
	}
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
	// Keep the last completed reply for follow-ups. A cancelled turn (Barge-in
	// or shutdown), an ignored turn and a blank answer must not replace the last
	// thing the user actually heard; an error or Unavailable reply was spoken,
	// so it counts.
	var isSmoke bool
	if tl := latency.FromContext(ctx); tl != nil && tl.Source() == latency.SourceSmoke {
		isSmoke = true
	}
	if p.lastReply != nil && ctx.Err() == nil && !reply.Ignored && strings.TrimSpace(reply.Text) != "" && !isSmoke {
		var source string
		if tl := latency.FromContext(ctx); tl != nil {
			source = string(tl.Source())
		}
		p.lastReply.Record(lastreply.Reply{
			Text:       reply.Text,
			Tool:       reply.Tool,
			Path:       string(reply.Path),
			LocalOnly:  reply.LocalOnly,
			SpeechRate: reply.SpeechRate,
			Utterance:  transcript,
			Source:     source,
		})
	}
	// The reply text and its provenance are display-only: the timeline keeps
	// them for the authenticated Activity page, and the request log line never
	// includes them. This is the one place every route records the answer.
	latency.SetSummary(ctx, reply.Text, reply.LocalOnly, reply.Pending != nil)
	if latency.FromContext(ctx) != nil {
		latency.SetRoute(ctx, string(reply.Path), reply.Tool, transcript)
		latency.SetAttr(ctx, "top_tools", record.top)
		latency.SetAttr(ctx, "margin", record.margin)
		latency.SetAttr(ctx, "extract", record.extraction)
		// A handled path has no fall-through reason: it is absent, not an
		// empty string that reads like a stage something forgot to set.
		if record.reason != "" {
			latency.SetAttr(ctx, "fallthrough_reason", record.reason)
		}
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
	}
	// An empty reason is omitted rather than logged as "", for the same reason
	// a missing stage is absent: nothing fell through on a handled path.
	if record.reason != "" {
		attrs = append(attrs, "fallthrough_reason", record.reason)
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

// BindChains sets the live chain switch before serving requests.
func (p *Pipeline) BindChains(enabled func() bool) { p.chainEnabled = enabled }
