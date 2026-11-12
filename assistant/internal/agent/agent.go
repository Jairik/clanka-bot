// Package agent implements the Agent path: the single prompt builder that is
// the only way data leaves the Pi, and the tool-calling loop behind it.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
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
	APIKey       func() string
	Model        string
	Headers      map[string]string
	Routing      map[string]any
	SystemPrompt string
	// SystemPromptFunc, when set, overrides SystemPrompt and is read per
	// request so refreshed location data is used.
	SystemPromptFunc   func() string
	UnavailableMessage string
	// UnavailableMessageFunc, when set, is read for each failure so a stored
	// wording override applies without rebuilding the Agent path.
	UnavailableMessageFunc func() string
	FirstTokenTimeout      time.Duration
	RequestTimeout         time.Duration
	MaxSteps               int
	HistoryTurns           int
	HistoryQuiet           time.Duration
}

// PendingConfirm is a confirm Tool call the model asked for, armed in the
// registry and never executed by the loop.
type PendingConfirm = tool.PendingConfirmation

// Result is one Agent path answer.
type Result struct {
	Text        string
	Calls       []ToolCall
	Pending     *PendingConfirm
	Unavailable bool
	TimedOut    bool
	// Cancelled is a Barge-in or shutdown, not a provider failure. The caller
	// must not log it as unavailable or mark the request failed.
	Cancelled bool
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
	return &Agent{
		provider:    newProvider(settings),
		registry:    registry,
		prompt:      prompt,
		history:     NewHistory(settings.HistoryTurns, settings.HistoryQuiet, nil),
		logger:      logger,
		maxSteps:    settings.MaxSteps,
		unavailable: unavailable,
	}, nil
}

// History is the rolling history shared with the Fast path.
func (a *Agent) History() *History { return a.history }

// Run answers one utterance. onDelta, when set, receives content deltas as
// they stream; #21 uses it for sentence-by-sentence TTS.
func (a *Agent) Run(ctx context.Context, req tool.Request, utterance string, onDelta func(string)) Result {
	started := time.Now()
	req.Path = tool.PathAgent

	history := a.history.Recent()
	messages := a.prompt.Messages(utterance, history)
	tools := a.prompt.Tools(a.registry)

	var recorded []ToolCall
	for step := 0; step < a.maxSteps; step++ {
		reply, err := a.provider.stream(ctx, messages, tools, onDelta)
		if err != nil {
			return a.failure(ctx, req, err)
		}
		if len(reply.Calls) == 0 {
			text := strings.TrimSpace(reply.Content)
			if text == "" {
				text = EmptyReplyMessage
			}
			a.history.Add(Turn{Utterance: utterance, Reply: text, Calls: recorded})
			a.logger.Info("agent reply",
				"request_id", req.ID,
				"steps", step+1,
				"tools", callNames(recorded),
				"duration_ms", time.Since(started).Milliseconds(),
			)
			return Result{Text: text, Calls: recorded}
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
				return Result{Text: text, Calls: recorded, Pending: &pending}
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
	return Result{Text: MaxStepsMessage, Calls: recorded}
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
	callCtx := tool.WithRequest(ctx, tool.Request{ID: req.ID, Path: tool.PathAgent, Device: req.Device})
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

func (a *Agent) failure(ctx context.Context, req tool.Request, err error) Result {
	// A Barge-in or shutdown cancels the request context. That is the caller's
	// intention, not a provider failure: no error log, no Unavailable flag and
	// no agent_error/MarkFailed on the request line (F5 / #33-36 follow-up).
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return Result{Cancelled: true}
	}
	switch {
	case errors.Is(err, errFirstTokenTimeout):
		a.logger.Error("agent first token timeout", "request_id", req.ID)
		return Result{Text: FirstTokenTimeoutMessage, TimedOut: true}
	case errors.Is(err, errRequestTimeout):
		a.logger.Error("agent request timeout", "request_id", req.ID)
		return Result{Text: a.unavailable(), Unavailable: true}
	}
	status := 0
	var providerErr *providerError
	if errors.As(err, &providerErr) {
		status = providerErr.Status
	}
	// The status code is logged; the user hears only the configured message.
	a.logger.Error("agent request failed", "request_id", req.ID, "status", status, "error", err.Error())
	return Result{Text: a.unavailable(), Unavailable: true}
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
