// Package agent implements the Agent path: the single prompt builder that is
// the only way data leaves the Pi, and the tool-calling loop behind it.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Spoken messages the Agent path owns. The Unavailable message comes from
// config; these are fixed because the user cannot act on anything more.
const (
	FirstTokenTimeoutMessage = "The agent isn't responding."
	MaxStepsMessage          = "That took too many steps, so I stopped."
	EmptyReplyMessage        = "I don't have an answer for that."
	ConfirmPrompt            = "I need your confirmation before I do that."
	EmptyRequestReply        = "What would you like to ask?"
)

// Settings is the Agent path's runtime configuration (non-secret).
type Settings struct {
	BaseURL string
	// APIKeyEnv is the environment bootstrap for the provider key. It is the
	// fallback when APIKey is nil and the variable name the secrets service
	// declares for the Dashboard write.
	APIKeyEnv string
	// APIKey, when set, resolves the key per request so a stored secret takes
	// effect without a restart. The stored value wins over APIKeyEnv.
	APIKey func() string
	Model  string
	// ModelFunc, when set, is read before every provider request so a live
	// settings override applies to the next turn. Model is the fallback.
	ModelFunc    func() string
	Headers      map[string]string
	Routing      map[string]any
	SystemPrompt string
	// SystemPromptFunc, when set, overrides SystemPrompt and is read per
	// request so refreshed location data is used.
	SystemPromptFunc func() string
	// PersonaFunc, when set, supplies the active personality prompt. It is read
	// once per turn when the labelled system part is built; it never adds a
	// second egress path (ADR-0002).
	PersonaFunc func() Persona
	// ClarifyMessage is the line a request addressed to the Assistant gets when
	// it cannot be understood. ClarifyMessageFunc, when set, is read per request
	// so the live agent.clarify_message setting applies to the next turn.
	ClarifyMessage     string
	ClarifyMessageFunc func() string
	UnavailableMessage string
	// UnavailableMessageFunc, when set, is read for each failure so a stored
	// wording override applies without rebuilding the Agent path.
	UnavailableMessageFunc func() string
	FirstTokenTimeout      time.Duration
	RequestTimeout         time.Duration
	MaxSteps               int
	HistoryTurns           int
	HistoryQuiet           time.Duration
	// History, when set, is the shared rolling history to use instead of one
	// built from HistoryTurns and HistoryQuiet. main builds it before Tools
	// register so assistant.forget can hold the same instance the Agent path
	// reads; a nil value keeps the original NewHistory construction.
	History *History
	// Limits are the static length and cost bounds. LimitsFunc, when set, is
	// read once per turn and overrides them; that is the live-setting seam and
	// the per-turn override point conversation mode uses.
	Limits     Limits
	LimitsFunc func() Limits
	// ConversationFunc, when set, reports how this turn may use the longer
	// conversation budget: "mode" while conversation mode is on, "cue" when
	// the utterance carries a cue word, or "" for the default budget. It is
	// read once per turn with the utterance, so the mode and idle clock live
	// outside the Agent path.
	ConversationFunc func(utterance string) string
	// ConversationLimits overrides Limits on a conversation turn. Zero fields
	// keep ConversationLimits().
	ConversationLimits Limits
}

// PendingConfirm is a confirm Tool call the model asked for, armed in the
// registry and never executed by the loop.
type PendingConfirm = tool.PendingConfirmation

// Result is one Agent path answer.
type Result struct {
	Text  string
	Calls []ToolCall
	// Pending is a Confirmation question the caller must speak.
	Pending *PendingConfirm
	// TextNotStreamed marks a fixed fallback line that no delta carried. The
	// spoken session must say it even after an earlier step streamed a
	// preamble; it is false for a normal answer, whose deltas are the text.
	TextNotStreamed bool
	Unavailable     bool
	TimedOut        bool
	// Retries is the number of attempted provider retries in this turn (0 or
	// 1). It is published on the request line as agent_retries.
	Retries int
	// Token counts are summed across every model step in this turn and
	// published on the request line as prompt_tokens / completion_tokens and
	// friends. Zero means the provider omitted usage (unknown, not free).
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	// Steps is the number of model steps the turn took. It is published on
	// the request line as agent_steps.
	Steps int
	// MaxSteps is the step-limit stop, which is a failed request rather than a
	// successful answer.
	MaxSteps bool
	// Cancelled is a Barge-in or shutdown, not a provider failure. The caller
	// must not log it as unavailable or mark the request failed.
	Cancelled bool
	// Truncated marks a reply the spoken-length budget cut at a sentence
	// boundary. Text is then the complete spoken answer, not a failure.
	Truncated bool
	// Conversation is the kind of longer budget this turn used: "mode", "cue"
	// or empty for the default. It is display-only, carried into Activity.
	Conversation string
	// Ignored means the model decided the speech was not addressed to the
	// Assistant and replied with IgnoreToken. Text is empty, the turn is not in
	// history, and the caller speaks nothing but an optional soft Cue.
	Ignored bool
}

// tokenUsage is the summed model usage for one request across its steps.
type tokenUsage struct {
	prompt     int64
	completion int64
	total      int64
	cached     int64
	steps      int
}

func (u tokenUsage) add(c completion) tokenUsage {
	u.prompt += c.PromptTokens
	u.completion += c.CompletionTokens
	u.total += c.TotalTokens
	u.cached += c.CachedTokens
	return u
}

func (u tokenUsage) apply(r *Result) {
	r.PromptTokens = u.prompt
	r.CompletionTokens = u.completion
	r.TotalTokens = u.total
	r.CachedTokens = u.cached
	r.Steps = u.steps
}

// Agent runs the tool-calling loop against one registry.
type Agent struct {
	provider    *provider
	registry    *tool.Registry
	prompt      *PromptBuilder
	history     *History
	logger      *slog.Logger
	maxSteps    int
	unavailable func() string
	limits      Limits
	limitsFunc  func() Limits
	// conversation reports the per-turn budget kind, and conversationLimits is
	// the longer budget a non-empty kind uses.
	conversation       func(string) string
	conversationLimits Limits
}

// New builds the Agent path. History defaults mirror the prototype settings so
// a zero Settings still runs.
func New(settings Settings, registry *tool.Registry, logger *slog.Logger) (*Agent, error) {
	if registry == nil {
		return nil, fmt.Errorf("agent: registry is required")
	}
	if settings.BaseURL == "" || settings.Model == "" {
		return nil, fmt.Errorf("agent: base url and model are required")
	}
	if settings.UnavailableMessage == "" {
		return nil, fmt.Errorf("agent: unavailable message is required")
	}
	if settings.FirstTokenTimeout <= 0 {
		settings.FirstTokenTimeout = 8 * time.Second
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = 60 * time.Second
	}
	if settings.MaxSteps <= 0 {
		settings.MaxSteps = 6
	}
	if settings.HistoryTurns <= 0 {
		settings.HistoryTurns = 10
	}
	if settings.HistoryQuiet <= 0 {
		settings.HistoryQuiet = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	prompt := NewPromptBuilder(settings.SystemPrompt)
	if settings.SystemPromptFunc != nil {
		prompt = NewPromptBuilderFunc(settings.SystemPromptFunc)
	}
	// The Persona prompt is part of the same labelled system message. A nil
	// source keeps the plain prompt, so a setup without Personas is unchanged.
	if settings.PersonaFunc != nil {
		prompt = prompt.WithPersona(settings.PersonaFunc)
	}
	// The Clarify line is live wording, read while the system part is built.
	// The static value and then the code default keep the rule present even
	// when a resolver returns nothing or config leaves the key empty.
	prompt = prompt.WithClarify(func() string {
		if settings.ClarifyMessageFunc != nil {
			if text := strings.TrimSpace(settings.ClarifyMessageFunc()); text != "" {
				return text
			}
		}
		if text := strings.TrimSpace(settings.ClarifyMessage); text != "" {
			return text
		}
		return DefaultClarifyMessage
	})
	// Resolve the Unavailable wording per failure. The static value stays the
	// required fallback, so a resolver that returns nothing cannot silence the
	// error path.
	unavailable := func() string {
		if settings.UnavailableMessageFunc != nil {
			if text := settings.UnavailableMessageFunc(); strings.TrimSpace(text) != "" {
				return text
			}
		}
		return settings.UnavailableMessage
	}
	history := settings.History
	if history == nil {
		history = NewHistory(settings.HistoryTurns, settings.HistoryQuiet, nil)
	}
	conversationLimits := settings.ConversationLimits
	if conversationLimits.SpokenMaxSentences <= 0 || conversationLimits.SpokenMaxChars <= 0 || conversationLimits.MaxCompletionTokens <= 0 {
		conversationLimits = ConversationLimits()
	}
	return &Agent{
		provider:           newProvider(settings),
		registry:           registry,
		prompt:             prompt,
		history:            history,
		logger:             logger,
		maxSteps:           settings.MaxSteps,
		unavailable:        unavailable,
		limits:             settings.Limits,
		limitsFunc:         settings.LimitsFunc,
		conversation:       settings.ConversationFunc,
		conversationLimits: conversationLimits,
	}, nil
}

// History is the rolling history shared with the Fast path.
func (a *Agent) History() *History { return a.history }

// Complete runs one short bounded completion with the same provider, key and
// privacy fields as a conversational turn. The Dashboard uses it to fill a
// Persona phrase book. It does not touch history or call Tools.
func (a *Agent) Complete(ctx context.Context, system, user string) (string, error) {
	if a == nil || a.provider == nil {
		return "", errors.New("agent: provider is not configured")
	}
	return a.provider.complete(ctx, system, user)
}

// Run answers one utterance. onDelta, when set, receives content deltas as
// they stream; #21 uses it for sentence-by-sentence TTS.
func (a *Agent) Run(ctx context.Context, req tool.Request, utterance string, onDelta func(string)) Result {
	started := time.Now()
	req.Path = tool.PathAgent
	// The turn's own words are the provenance a Tool can check against an
	// argument the model chose (#172). They are metadata for the handler, not
	// part of the prompt this function builds.
	req.UserText = utterance

	history := a.history.Recent()
	// The conversation decision is read once per turn, before the prompt is
	// built, so the mode line and the budget describe the same turn.
	kind := ""
	if a.conversation != nil {
		kind = a.conversation(utterance)
	}
	messages := a.prompt.MessagesFor(utterance, history, kind != "")
	tools := a.prompt.Tools(a.registry)

	var recorded []ToolCall
	var retries int
	var usage tokenUsage
	truncated := false
	// Limits are read once per turn, so a live setting or a conversation-mode
	// override applies to this request without rebuilding the Agent.
	limits := resolveLimits(a.limits, a.limitsFunc)
	if kind != "" {
		limits = a.conversationLimits
	}
	for step := 0; step < a.maxSteps; step++ {
		// stream returns an empty completion on every error, so the caller
		// cannot tell from the result whether anything was already spoken.
		// Track it at the only place that knows: the delta callback. A retry
		// is safe only while this flag is false, because repeating content
		// would speak the same sentence twice.
		streamed := false
		// Each model step gets its own budget. A preamble before a Tool call and
		// the final answer are separate spoken turns; capping both keeps every
		// streamed segment short without losing the answer after a Tool step.
		budget := speech.NewBudget(speech.Limits{
			MaxSentences: limits.SpokenMaxSentences,
			MaxChars:     limits.SpokenMaxChars,
		})
		stepTruncated := false
		trackedDelta := func(text string) bool {
			streamed = true
			chunk, reached := budget.Write(text)
			if chunk != "" && onDelta != nil {
				onDelta(chunk)
			}
			if reached {
				stepTruncated = true
				return false
			}
			return true
		}
		reply, err := a.provider.stream(ctx, messages, tools, limits.MaxCompletionTokens, trackedDelta)
		// One retry, only on the first step and only before any output: a
		// later step already spoke a preamble, and no Tool has run yet because
		// execute happens only after a successful stream. That makes a Tool
		// side effect impossible to duplicate here.
		if err != nil && step == 0 && !streamed && ctx.Err() == nil && retryableFailure(err) {
			retries++
			a.logger.Info("agent retry", "request_id", req.ID, "status", providerStatus(err))
			// Bounded by the caller's context; a cancellation during the
			// wait leaves the error to failure() as a normal cancellation.
			waitRetry(ctx)
			streamed = false
			reply, err = a.provider.stream(ctx, messages, tools, limits.MaxCompletionTokens, trackedDelta)
		}
		if err != nil {
			result := a.failure(ctx, req, err, streamed)
			result.Retries = retries
			result.Conversation = kind
			usage.steps = step + 1
			usage.apply(&result)
			return result
		}
		// Speak any cleaned text the cleaner held for a decision that never
		// came. A truncated stream has already spoken everything it will.
		if rest := budget.Flush(); rest != "" && onDelta != nil {
			onDelta(rest)
		}
		truncated = truncated || stepTruncated || reply.Truncated
		usage = usage.add(reply)
		usage.steps = step + 1
		// A step the spoken budget cut may have been streaming Tool calls when
		// the stream was cancelled. Their arguments are incomplete, so the turn
		// ends with the text the user heard instead of reporting half-written
		// calls as Tool errors and asking the model for another step.
		if stepTruncated || reply.Truncated || len(reply.Calls) == 0 {
			// Text is the cleaned and budgeted spoken text, so history, the
			// Last reply and Activity all keep exactly what was heard rather
			// than the raw markdown the model emitted.
			text := strings.TrimSpace(budget.Text())
			// The ignore signal is a decision about the turn, not an answer:
			// nothing is spoken, and the utterance and reply stay out of
			// history so a misheard fragment cannot steer later turns (#498).
			// It only applies before any Tool ran: a turn that already acted
			// must not be erased from history, and the token must still never
			// reach TTS, so it counts as an empty answer instead.
			if IsIgnoreReply(text) {
				if len(recorded) > 0 {
					text = ""
				} else {
					a.logger.Info("agent ignored",
						"request_id", req.ID,
						"steps", step+1,
						"tools", callNames(recorded),
						"duration_ms", time.Since(started).Milliseconds(),
					)
					result := Result{Ignored: true, Calls: recorded, Retries: retries, Conversation: kind}
					usage.apply(&result)
					return result
				}
			}
			// An empty answer is a fallback line no delta carried.
			notStreamed := text == ""
			if notStreamed {
				text = EmptyReplyMessage
			}
			a.history.Add(Turn{Utterance: utterance, Reply: text, Calls: recorded})
			a.logger.Info("agent reply",
				"request_id", req.ID,
				"steps", step+1,
				"tools", callNames(recorded),
				"truncated", truncated,
				"duration_ms", time.Since(started).Milliseconds(),
			)
			result := Result{Text: text, Calls: recorded, TextNotStreamed: notStreamed, Truncated: truncated, Retries: retries, Conversation: kind}
			usage.apply(&result)
			return result
		}

		// A confirm Tool never runs here. The registry arms the single pending
		// call and returns the question to ask, so enforcement cannot be
		// bypassed by the model. Sibling read/act calls in the same model step
		// still run: only the confirm call waits for the spoken yes (#20).
		if confirmIndex := a.firstConfirm(reply.Calls); confirmIndex >= 0 {
			runnable := withoutIndex(reply.Calls, confirmIndex)
			outcomes := a.execute(ctx, req, runnable)
			for i, call := range runnable {
				recorded = append(recorded, ToolCall{Name: outcomes[i].Name, Args: call.Args, Result: outcomes[i].Payload, LocalOnly: outcomes[i].LocalOnly})
			}
			pending, err := a.propose(reply.Calls[confirmIndex])
			if err == nil {
				a.logger.Info("agent confirmation pending", "request_id", req.ID, "tool", pending.Tool)
				text := pending.Summary
				if text == "" {
					text = ConfirmPrompt
				}
				result := Result{Text: text, Calls: recorded, Pending: &pending, Retries: retries, Conversation: kind}
				usage.apply(&result)
				return result
			}
			// The proposal was invalid (for example unparsable arguments).
			// Report every call back to the model and keep the loop honest
			// instead of arming something that cannot run.
			a.logger.Error("agent confirmation propose", "request_id", req.ID, "tool", reply.Calls[confirmIndex].Name, "error", err.Error())
			messages = append(messages, a.prompt.AssistantToolCalls(reply.Content, reply.Calls))
			for i, call := range runnable {
				messages = append(messages, a.prompt.ToolResult(call.ID, outcomes[i].Payload, outcomes[i].LocalOnly))
			}
			messages = append(messages, a.prompt.ToolResult(reply.Calls[confirmIndex].ID, errorPayload("confirmation required"), false))
			continue
		}

		messages = append(messages, a.prompt.AssistantToolCalls(reply.Content, reply.Calls))
		outcomes := a.execute(ctx, req, reply.Calls)
		for i, call := range reply.Calls {
			outcome := outcomes[i]
			messages = append(messages, a.prompt.ToolResult(call.ID, outcome.Payload, outcome.LocalOnly))
			recorded = append(recorded, ToolCall{Name: outcome.Name, Args: call.Args, Result: outcome.Payload, LocalOnly: outcome.LocalOnly})
		}
	}

	a.history.Add(Turn{Utterance: utterance, Reply: MaxStepsMessage, Calls: recorded})
	a.logger.Warn("agent stopped at step limit", "request_id", req.ID, "steps", a.maxSteps)
	// The step-limit line is a fixed fallback no delta carried.
	usage.steps = a.maxSteps
	result := Result{Text: MaxStepsMessage, Calls: recorded, MaxSteps: true, TextNotStreamed: true, Retries: retries, Conversation: kind}
	usage.apply(&result)
	return result
}

// firstConfirm returns the index of the first runnable confirm call in one
// model step, or -1. A call whose streamed arguments were invalid is not a
// runnable confirm: it is reported as a Tool error instead.
func (a *Agent) firstConfirm(calls []CallRef) int {
	for i, call := range calls {
		if call.ArgsError != "" {
			continue
		}
		info, ok := a.resolve(call)
		if ok && info.Permission == tool.PermissionConfirm {
			return i
		}
	}
	return -1
}

// propose asks the registry to arm one confirm call. It never runs the call.
func (a *Agent) propose(call CallRef) (tool.PendingConfirmation, error) {
	info, ok := a.resolve(call)
	if !ok || info.Permission != tool.PermissionConfirm {
		return tool.PendingConfirmation{}, errors.New("agent: call is not a confirm tool")
	}
	return a.registry.Propose(info.Name, call.Args)
}

// withoutIndex copies calls without one index, preserving model order.
func withoutIndex(calls []CallRef, skip int) []CallRef {
	out := make([]CallRef, 0, len(calls)-1)
	for i, call := range calls {
		if i != skip {
			out = append(out, call)
		}
	}
	return out
}

// resolve maps a wire name to a registered, enabled, non-Local-only Tool.
func (a *Agent) resolve(call CallRef) (tool.Info, bool) {
	name, err := tool.CanonicalName(call.Name)
	if err != nil {
		return tool.Info{}, false
	}
	info, ok := a.registry.Info(name)
	if !ok || info.LocalOnly || !info.Enabled {
		return tool.Info{}, false
	}
	return info, true
}

type outcome struct {
	Name      string
	Payload   json.RawMessage
	LocalOnly bool
}

// execute runs one batch of Tool calls: reads concurrently, then acts one at a
// time in the order the model gave them. Confirm calls never reach here.
func (a *Agent) execute(ctx context.Context, req tool.Request, calls []CallRef) []outcome {
	batchStarted := time.Now()
	defer func() { latency.AddStage(ctx, latency.StageToolExecute, time.Since(batchStarted)) }()

	out := make([]outcome, len(calls))
	infos := make([]tool.Info, len(calls))
	valid := make([]bool, len(calls))
	for i, call := range calls {
		if call.ArgsError != "" {
			out[i] = outcome{Name: call.Name, Payload: errorPayload(call.ArgsError)}
			a.logger.Warn("agent tool refused", "request_id", req.ID, "tool", call.Name, "reason", call.ArgsError)
			continue
		}
		info, ok := a.resolve(call)
		infos[i], valid[i] = info, ok
		if !ok {
			out[i] = outcome{Name: call.Name, Payload: errorPayload("unknown tool")}
			a.logger.Warn("agent tool refused", "request_id", req.ID, "tool", call.Name)
		}
	}

	var wg sync.WaitGroup
	for i := range calls {
		if valid[i] && infos[i].Permission == tool.PermissionRead {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				out[i] = a.runOne(ctx, req, calls[i], infos[i])
			}(i)
		}
	}
	wg.Wait()

	for i := range calls {
		if !valid[i] {
			continue
		}
		switch infos[i].Permission {
		case tool.PermissionAct:
			out[i] = a.runOne(ctx, req, calls[i], infos[i])
		case tool.PermissionConfirm:
			// Defensive: confirm Tools are refused before any batch runs.
			out[i] = outcome{Name: infos[i].Name, Payload: errorPayload("confirmation required")}
		}
	}
	return out
}

func (a *Agent) runOne(ctx context.Context, req tool.Request, call CallRef, info tool.Info) outcome {
	callCtx := tool.WithRequest(ctx, tool.Request{
		ID: req.ID, Path: tool.PathAgent, Device: req.Device, UserText: req.UserText,
	})
	result, err := a.registry.Execute(callCtx, info.Name, call.Args)
	if err != nil {
		// The model only ever sees the user-facing message; details stay local.
		a.logger.Error("agent tool failed", "request_id", req.ID, "tool", info.Name, "error", err.Error())
		return outcome{Name: info.Name, Payload: errorPayload(tool.FailureMessage(err)), LocalOnly: info.LocalOnly}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		a.logger.Error("agent tool result", "request_id", req.ID, "tool", info.Name, "error", err.Error())
		return outcome{Name: info.Name, Payload: errorPayload(tool.DefaultFailureMessage), LocalOnly: info.LocalOnly}
	}
	return outcome{Name: info.Name, Payload: payload, LocalOnly: info.LocalOnly}
}

// retryBase is the base wait before the one provider retry. A small jitter
// keeps simultaneous retries from landing on the provider together.
const retryBase = 250 * time.Millisecond

// waitRetry waits the bounded backoff, or returns early when the caller's
// context is cancelled. The retry never outlives the request budget.
func waitRetry(ctx context.Context) {
	wait := retryBase + time.Duration(rand.Int64N(int64(retryBase/2)))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// retryableFailure reports whether err is a transient provider failure worth
// one retry. Only a transport error (no HTTP status) or status 408/425/429/5xx
// qualifies. Timeouts we own, caller cancellations and every other 4xx are
// terminal: a timeout means the user already waited, a 4xx will not change on
// a second identical request.
func retryableFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errFirstTokenTimeout) || errors.Is(err, errRequestTimeout) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	status := providerStatus(err)
	if status == 0 {
		var providerErr *providerError
		return errors.As(err, &providerErr)
	}
	return status == 408 || status == 425 || status == 429 || (status >= 500 && status < 600)
}

// providerStatus returns the HTTP status carried by a providerError, or 0 for
// a transport error that never produced an HTTP response.
func providerStatus(err error) int {
	var providerErr *providerError
	if errors.As(err, &providerErr) {
		return providerErr.Status
	}
	return 0
}

// failure classifies a provider/stream error into the spoken Result. streamed
// is whether the failing step already delivered content deltas: a request that
// stalls or drops after it started speaking must not be followed by an
// apology, so a final failure that streamed text leaves TextNotStreamed false
// (#342). A preamble carried by an earlier completed step still sets the flag,
// because the failing step itself delivered nothing (the #193 case).
func (a *Agent) failure(ctx context.Context, req tool.Request, err error, streamed bool) Result {
	// A Barge-in or shutdown cancels the request context. That is the caller's
	// intention, not a provider failure: no error log, no Unavailable flag and
	// no agent_error/MarkFailed on the request line (F5 / #33-36 follow-up).
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return Result{Cancelled: true}
	}
	switch {
	case errors.Is(err, errFirstTokenTimeout):
		a.logger.Error("agent first token timeout", "request_id", req.ID)
		// The timeout line is a fixed fallback no delta carried: by
		// definition no first token arrived.
		return Result{Text: FirstTokenTimeoutMessage, TimedOut: true, TextNotStreamed: true}
	case errors.Is(err, errRequestTimeout):
		a.logger.Error("agent request timeout", "request_id", req.ID)
		// The Unavailable line is a fixed fallback only when the failing step
		// never spoke; a stream that dropped after its text already reached
		// the user is not followed by an apology (#342).
		return Result{Text: a.unavailable(), Unavailable: true, TextNotStreamed: !streamed}
	}
	status := 0
	var providerErr *providerError
	if errors.As(err, &providerErr) {
		status = providerErr.Status
	}
	// The status code is logged; the user hears only the configured message.
	// The Unavailable line is a fixed fallback only when the failing step
	// streamed nothing; a stream that errored after delivering its text keeps
	// the text and stays silent (#342).
	a.logger.Error("agent request failed", "request_id", req.ID, "status", status, "error", err.Error())
	return Result{Text: a.unavailable(), Unavailable: true, TextNotStreamed: !streamed}
}

func errorPayload(message string) json.RawMessage {
	payload, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return json.RawMessage(`{"error":"failed"}`)
	}
	return payload
}

func callNames(calls []ToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return names
}

// MatchRequestPrefix strips a configured Agent request prefix. The comparison
// is normalised (lowercase, collapsed spaces, trailing sentence punctuation
// dropped), which suits STT transcripts that carry no casing and add a full
// stop. A separator after the prefix may be a space, comma, full stop, question
// mark or exclamation mark; anything else ("agentic") does not match (#33).
func MatchRequestPrefix(text string, prefixes []string) (string, bool) {
	normalised := phrase.Normalize(text)
	matched := ""
	for _, prefix := range prefixes {
		candidate := phrase.Normalize(prefix)
		if candidate == "" || len(candidate) <= len(matched) {
			continue
		}
		if normalised == candidate {
			matched = candidate
			continue
		}
		if !strings.HasPrefix(normalised, candidate) {
			continue
		}
		rest := normalised[len(candidate):]
		if !strings.ContainsRune(" ,.!?", rune(rest[0])) {
			continue
		}
		matched = candidate
	}
	if matched == "" {
		return "", false
	}
	rest := strings.TrimLeft(normalised[len(matched):], " ,.!?")
	return strings.TrimSpace(rest), true
}
