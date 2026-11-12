package agent

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// DefaultSystemPrompt is used when config sets no system prompt. It is static
// on purpose: no personal data is loaded ahead of a request (ADR-0002).
const DefaultSystemPrompt = "You are a concise voice assistant. Answer briefly and naturally, and use the provided tools when they help. If a request needs information you do not have, say so."

// PromptBuilder assembles every Agent path request. It is the only code that
// turns history and Tool results into messages, so privacy filtering lives in
// one place (ADR-0002).
type PromptBuilder struct {
	system func() string
}

// NewPromptBuilder builds a prompt builder. An empty system prompt falls back
// to DefaultSystemPrompt.
func NewPromptBuilder(system string) *PromptBuilder {
	return NewPromptBuilderFunc(func() string { return system })
}

// NewPromptBuilderFunc builds a prompt builder whose system prompt is read per
// request, so a refreshed location reaches later requests. An empty result
// falls back to DefaultSystemPrompt.
func NewPromptBuilderFunc(system func() string) *PromptBuilder {
	return &PromptBuilder{system: func() string {
		if system != nil {
			if text := strings.TrimSpace(system()); text != "" {
				return text
			}
		}
		return DefaultSystemPrompt
	}}
}

// Messages builds the labelled parts of a request: the system prompt, the
// shared history, and the current utterance. A turn that touched a Local-only
// Tool is dropped whole, utterance included.
func (b *PromptBuilder) Messages(utterance string, history []Turn) []openai.ChatCompletionMessageParamUnion {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, 1+2*len(history)+1)
	messages = append(messages, openai.SystemMessage(b.system()))
	for _, turn := range history {
		if turn.localOnly() {
			continue
		}
		messages = append(messages, openai.UserMessage(turn.Utterance))
		messages = append(messages, openai.AssistantMessage(turnContent(turn)))
	}
	messages = append(messages, openai.UserMessage(utterance))
	return messages
}

// turnContent renders a recorded turn: the spoken reply plus compact Tool-call
// JSON, so follow-ups can see what was called.
func turnContent(turn Turn) string {
	if len(turn.Calls) == 0 {
		return turn.Reply
	}
	compact, err := json.Marshal(turn.Calls)
	if err != nil {
		return turn.Reply
	}
	return turn.Reply + "\n" + string(compact)
}

// Tools offers every Tool except Local-only ones. Local-only Tools are never
// shown to the model, so it cannot call them.
func (b *PromptBuilder) Tools(reg *tool.Registry) []openai.ChatCompletionToolUnionParam {
	names := reg.Names()
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(names))
	for _, name := range names {
		info, ok := reg.Info(name)
		if !ok || info.LocalOnly {
			continue
		}
		var parameters shared.FunctionParameters
		if len(info.InputSchema) > 0 {
			if err := json.Unmarshal(info.InputSchema, &parameters); err != nil {
				continue
			}
		}
		tools = append(tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        info.WireName,
					Description: openai.String(info.Description),
					Parameters:  parameters,
				},
			},
		})
	}
	return tools
}

// CallRef is one Tool call the model asked for, before it runs.
type CallRef struct {
	ID   string
	Name string // wire name, as sent by the model
	Args json.RawMessage
	// ArgsError is set when the streamed arguments were not valid JSON. The
	// call is reported to the model as a Tool error and never reaches a
	// handler; Args itself is the JSON-valid placeholder null.
	ArgsError string
}

// AssistantToolCalls renders the model's tool-call turn back into the request.
func (b *PromptBuilder) AssistantToolCalls(content string, calls []CallRef) openai.ChatCompletionMessageParamUnion {
	message := openai.AssistantMessage(content)
	for _, call := range calls {
		message.OfAssistant.ToolCalls = append(message.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: call.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      call.Name,
					Arguments: string(call.Args),
				},
			},
		})
	}
	return message
}

// ToolResult renders one Tool result. A Local-only result is replaced by a
// generic refusal even if a caller reaches this by mistake.
func (b *PromptBuilder) ToolResult(callID string, payload json.RawMessage, localOnly bool) openai.ChatCompletionMessageParamUnion {
	if localOnly || len(payload) == 0 {
		return openai.ToolMessage(`{"error":"unavailable"}`, callID)
	}
	return openai.ToolMessage(string(payload), callID)
}
