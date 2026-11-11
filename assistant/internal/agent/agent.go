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
	BaseURL            string
	APIKeyEnv          string
	Model              string
	Headers            map[string]string
	Routing            map[string]any
	SystemPrompt       string
	UnavailableMessage string
	FirstTokenTimeout  time.Duration
	RequestTimeout     time.Duration
	MaxSteps           int
	HistoryTurns       int
	HistoryQuiet       time.Duration
}

// PendingConfirm is a confirm Tool call the model asked for. It is never
// executed here; #24 adds the spoken Confirmation that runs it.
type PendingConfirm struct {
	Tool string
	Args json.RawMessage
}

// Result is one Agent path answer.
type Result struct {
	Text        string
	Calls       []ToolCall
	Pending     *PendingConfirm
	Unavailable bool
	TimedOut    bool
}

// Agent runs the tool-calling loop against one registry.
type Agent struct {
	provider    *provider
	registry    *tool.Registry
	prompt      *PromptBuilder
	history     *History
	logger      *slog.Logger
	maxSteps    int
	unavailable string
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
	return &Agent{
		provider:    newProvider(settings),
		registry:    registry,
		prompt:      NewPromptBuilder(settings.SystemPrompt),
		history:     NewHistory(settings.HistoryTurns, settings.HistoryQuiet, nil),
		logger:      logger,
		maxSteps:    settings.MaxSteps,
		unavailable: settings.UnavailableMessage,
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
			return a.failure(req, err)
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

		// A confirm Tool never runs here. Returning now keeps at most one
		// pending call per turn and leaves the spoken yes to #24.
		if pending, ok := a.confirmCall(reply.Calls); ok {
			a.logger.Info("agent confirmation pending", "request_id", req.ID, "tool", pending.Tool)
			return Result{Text: ConfirmPrompt, Calls: recorded, Pending: pending}
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

// confirmCall finds a confirm Tool call in one model step. It does not run it.
func (a *Agent) confirmCall(calls []CallRef) (*PendingConfirm, bool) {
	for _, call := range calls {
		info, ok := a.resolve(call)
		if !ok || info.Permission != tool.PermissionConfirm {
			continue
		}
		return &PendingConfirm{Tool: info.Name, Args: call.Args}, true
	}
	return nil, false
}

// resolve maps a wire name to a registered, non-Local-only Tool.
func (a *Agent) resolve(call CallRef) (tool.Info, bool) {
	name, err := tool.CanonicalName(call.Name)
	if err != nil {
		return tool.Info{}, false
	}
	info, ok := a.registry.Info(name)
	if !ok || info.LocalOnly {
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
	out := make([]outcome, len(calls))
	infos := make([]tool.Info, len(calls))
	valid := make([]bool, len(calls))
	for i, call := range calls {
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

func (a *Agent) failure(req tool.Request, err error) Result {
	switch {
	case errors.Is(err, errFirstTokenTimeout):
		a.logger.Error("agent first token timeout", "request_id", req.ID)
		return Result{Text: FirstTokenTimeoutMessage, TimedOut: true}
	case errors.Is(err, errRequestTimeout):
		a.logger.Error("agent request timeout", "request_id", req.ID)
		return Result{Text: a.unavailable, Unavailable: true}
	}
	status := 0
	var providerErr *providerError
	if errors.As(err, &providerErr) {
		status = providerErr.Status
	}
	// The status code is logged; the user hears only the configured message.
	a.logger.Error("agent request failed", "request_id", req.ID, "status", status, "error", err.Error())
	return Result{Text: a.unavailable, Unavailable: true}
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
// is normalised (lowercase, collapsed spaces), which suits STT transcripts that
// carry no casing.
func MatchRequestPrefix(text string, prefixes []string) (string, bool) {
	normalised := normalisePrefixText(text)
	matched := ""
	for _, prefix := range prefixes {
		candidate := normalisePrefixText(prefix)
		if candidate == "" || len(candidate) <= len(matched) {
			continue
		}
		if normalised != candidate && !strings.HasPrefix(normalised, candidate+" ") && !strings.HasPrefix(normalised, candidate+",") {
			continue
		}
		matched = candidate
	}
	if matched == "" {
		return "", false
	}
	rest := strings.TrimLeft(normalised[len(matched):], " ,")
	return strings.TrimSpace(rest), true
}

func normalisePrefixText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.Trim(text, ",?!.")
	return strings.Join(strings.Fields(text), " ")
}
