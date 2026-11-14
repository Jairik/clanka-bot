package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var (
	// errFirstTokenTimeout means the model produced nothing within the budget.
	errFirstTokenTimeout = errors.New("agent: first token timeout")
	// errRequestTimeout means the stream stalled after it started.
	errRequestTimeout = errors.New("agent: request timeout")
)

// providerError is a failed provider call, with the HTTP status when there was
// one. The status is logged, never spoken.
type providerError struct {
	Status int
	Err    error
}

func (e *providerError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("agent: provider status %d: %v", e.Status, e.Err)
	}
	return fmt.Sprintf("agent: provider: %v", e.Err)
}

func (e *providerError) Unwrap() error { return e.Err }

// provider is the single OpenAI-compatible client. Only one is active at a
// time; there is no failover.
type provider struct {
	client openai.Client
	// apiKey is read before every request, so a secret stored through the
	// Dashboard applies to the next turn without rebuilding the client.
	apiKey func() string
	// model is resolved per request; fallbackModel keeps the construction-time
	// value when a resolver returns nothing.
	model          func() string
	fallbackModel  string
	routing        map[string]any
	firstToken     time.Duration
	requestTimeout time.Duration
}

func newProvider(settings Settings) *provider {
	opts := []option.RequestOption{
		option.WithBaseURL(settings.BaseURL),
		option.WithMaxRetries(0),
	}
	for name, value := range settings.Headers {
		opts = append(opts, option.WithHeader(name, value))
	}
	// The key is not a client option: it is applied per request below, after
	// the secrets store has had a chance to win over the environment.
	apiKey := settings.APIKey
	if apiKey == nil {
		env := settings.APIKeyEnv
		apiKey = func() string { return os.Getenv(env) }
	}
	// The model is resolved per request, like the key. A nil resolver keeps the
	// static construction-time model.
	model := settings.ModelFunc
	if model == nil {
		model = func() string { return settings.Model }
	}
	return &provider{
		client:         openai.NewClient(opts...),
		apiKey:         apiKey,
		model:          model,
		fallbackModel:  settings.Model,
		routing:        settings.Routing,
		firstToken:     settings.FirstTokenTimeout,
		requestTimeout: settings.RequestTimeout,
	}
}

// completion is the accumulated result of one model call.
type completion struct {
	Content string
	Calls   []CallRef
}

// stream sends one chat-completion request and streams the reply. onDelta, when
// set, receives content deltas as they arrive (the hook #21's sentence
// streaming uses).
func (p *provider) stream(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam, onDelta func(string)) (completion, error) {
	model := strings.TrimSpace(p.model())
	if model == "" {
		model = p.fallbackModel
	}
	params := openai.ChatCompletionNewParams{Model: model, Messages: messages}
	if len(tools) > 0 {
		params.Tools = tools
	}
	// ADR-0002: ZDR and data-collection denial are written here, after any
	// configured routing, so no config value can loosen them.
	params.SetExtraFields(map[string]any{"provider": providerFields(p.routing)})

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(p.firstToken, func() { cancel(errFirstTokenTimeout) })
	defer func() { timer.Stop() }()

	started := time.Now()
	// Apply the current key per request. An empty key is left for the provider
	// to reject, so the Fast path still works and a key stored later needs no
	// restart.
	var requestOpts []option.RequestOption
	if key := p.apiKey(); key != "" {
		requestOpts = append(requestOpts, option.WithAPIKey(key))
	}
	stream := p.client.Chat.Completions.NewStreaming(runCtx, params, requestOpts...)
	defer stream.Close()

	var out completion
	var order []int64
	partial := make(map[int64]*partialCall)
	firstToken := true

	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if firstToken && (delta.Content != "" || len(delta.ToolCalls) > 0) {
			firstToken = false
			// The first-token budget ends only on real content or a Tool call.
			// A role-only prologue must not replace it with the longer request
			// budget, or a provider could stall past FirstTokenTimeout. The
			// request timer then covers the rest of the stream.
			timer.Stop()
			timer = time.AfterFunc(p.requestTimeout, func() { cancel(errRequestTimeout) })
			// Agent first token is the first token with content or a Tool
			// call, not a role-only prologue. First-wins across Agent steps.
			latency.SetStage(ctx, latency.StageAgentFirstToken, time.Since(started))
		}
		if delta.Content != "" {
			out.Content += delta.Content
			if onDelta != nil {
				onDelta(delta.Content)
			}
		}
		for _, call := range delta.ToolCalls {
			part, ok := partial[call.Index]
			if !ok {
				part = &partialCall{}
				partial[call.Index] = part
				order = append(order, call.Index)
			}
			if call.ID != "" {
				part.id = call.ID
			}
			if call.Function.Name != "" {
				part.name = call.Function.Name
			}
			part.args.WriteString(call.Function.Arguments)
		}
	}

	if cause := context.Cause(runCtx); errors.Is(cause, errFirstTokenTimeout) || errors.Is(cause, errRequestTimeout) {
		return completion{}, cause
	}
	if cause := context.Cause(runCtx); cause != nil && ctx.Err() != nil {
		return completion{}, cause
	}
	if err := stream.Err(); err != nil {
		return completion{}, wrapProviderError(err)
	}

	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, index := range order {
		part := partial[index]
		if part.name == "" {
			continue
		}
		args := json.RawMessage(part.args.String())
		argsError := ""
		if !json.Valid(args) {
			// Never run a handler on a truncated/aborted stream. Keep the
			// Tool-call protocol JSON-valid (null), but mark the call so the
			// loop returns a Tool error instead of an act with zero values.
			args = json.RawMessage("null")
			argsError = "arguments were not valid JSON"
		}
		out.Calls = append(out.Calls, CallRef{ID: part.id, Name: part.name, Args: args, ArgsError: argsError})
	}
	return out, nil
}

type partialCall struct {
	id   string
	name string
	args strings.Builder
}

func wrapProviderError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &providerError{Status: apiErr.StatusCode, Err: err}
	}
	return &providerError{Err: err}
}

// complete runs one non-streaming request with the same provider, key, model
// and forced privacy fields as the conversational path. It is for small
// bounded jobs such as generating a Persona phrase book; it adds no Tools,
// history or persona context of its own.
func (p *provider) complete(ctx context.Context, system, user string) (string, error) {
	model := strings.TrimSpace(p.model())
	if model == "" {
		model = p.fallbackModel
	}
	params := openai.ChatCompletionNewParams{
		Model: model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		// A phrase book is a small JSON object. Bound the reply so a hostile or
		// confused Persona prompt cannot turn this helper into a long, paid
		// completion; the caller validates every value again.
		MaxCompletionTokens: openai.Int(4000),
	}
	// ADR-0002: ZDR and data-collection denial are forced here too, so a
	// generated phrase book leaves the device under the same rules as a turn.
	params.SetExtraFields(map[string]any{"provider": providerFields(p.routing)})
	var opts []option.RequestOption
	if key := strings.TrimSpace(p.apiKey()); key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	runCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()
	resp, err := p.client.Chat.Completions.New(runCtx, params, opts...)
	if err != nil {
		return "", wrapProviderError(err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("agent: provider returned no choices")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// providerFields merges the configured routing and then forces the privacy
// fields, so config can add options (such as sort) but can never turn ZDR or
// data-collection denial off.
func providerFields(routing map[string]any) map[string]any {
	fields := make(map[string]any, len(routing)+2)
	for key, value := range routing {
		fields[key] = value
	}
	fields["zdr"] = true
	fields["data_collection"] = "deny"
	return fields
}
