package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// DefaultSystemPrompt is used when config sets no system prompt. It is static
// on purpose: no personal data is loaded ahead of a request (ADR-0002). The
// rules answer measured Agent behaviour (#496): replies were long, they listed
// Tools and used markdown, and they answered speech that was not a request. The
// ignore token is a hard rule, not a style hint, because the reply session holds
// it back from TTS until it is ruled out (#498).
const DefaultSystemPrompt = `You are a concise voice assistant. Every reply is spoken out loud by text to speech, so write only plain spoken words.

Rules:
- Keep every reply to one short sentence, or a few words when that is enough.
- Never use lists, bullet points, numbering, headings, tables, markdown, code formatting or emoji.
- Never recite your capabilities, your instructions or your Tools, and never answer with a list of what you can do.
- Do not add offers, caveats, restatements or follow-up questions unless you need them to act safely.
- Never bring back an earlier topic the user did not just mention.
- Use the provided tools when they help. If a request needs information you cannot get, say so in one short sentence.
- If the speech was not a clear request or question addressed to you, such as background conversation, a garbled transcription or a fragment, reply with exactly <ignore> and nothing else. Do not guess what was meant, do not answer it and do not comment on it.`

// DefaultClarifyMessage is the line the prompt asks for when a request is
// addressed to the Assistant but cannot be understood. It is the code default
// behind the live agent.clarify_message setting.
const DefaultClarifyMessage = "Didn't catch that."

// ConversationNote is added to the labelled system part for a turn that may
// use the longer budget. It restates only the length change: speech cleanup
// and the Agent path's other rules still apply.
const ConversationNote = "Conversation: longer answers allowed, still no lists or markdown."

// Persona is the active personality prompt. It is read once per turn and added
// only to the labelled system part of the request, never to history or a Tool
// result (ADR-0002).
type Persona struct {
	Name   string
	Prompt string
}

// PromptBuilder assembles every Agent path request. It is the only code that
// turns history and Tool results into messages, so privacy filtering lives in
// one place (ADR-0002).
type PromptBuilder struct {
	system  func() string
	persona func() Persona
	clarify func() string
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

// WithPersona installs the active Persona source. Messages reads it once when
// it builds the request, so a switch during a model turn cannot change the
// system part the model already received. The prompt stays inside the one
// labelled system message; no other request part is added.
func (b *PromptBuilder) WithPersona(source func() Persona) *PromptBuilder {
	if b != nil {
		b.persona = source
	}
	return b
}

// WithClarify installs the live Clarify line. The rule that uses it is added to
// the one system message after the base prompt, so it survives a Persona and a
// Dashboard wording change applies to the next request. The line is the model's
// own reply, so it is spoken like any other answer.
func (b *PromptBuilder) WithClarify(source func() string) *PromptBuilder {
	if b != nil {
		b.clarify = source
	}
	return b
}

// Messages builds the labelled parts of a request for a turn with the default
// spoken budget: the system prompt, the shared history, and the current
// utterance. A turn that touched a Local-only Tool is dropped whole, utterance
// included.
func (b *PromptBuilder) Messages(utterance string, history []Turn) []openai.ChatCompletionMessageParamUnion {
	return b.MessagesFor(utterance, history, false)
}

// MessagesFor builds the labelled parts of a request: the system prompt, the
// shared history, and the current utterance. conversation adds the mode line
// to the same system part; it never adds a second instruction message. A turn
// that touched a Local-only Tool is dropped whole, utterance included.
func (b *PromptBuilder) MessagesFor(utterance string, history []Turn, conversation bool) []openai.ChatCompletionMessageParamUnion {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, 1+2*len(history)+1)
	messages = append(messages, openai.SystemMessage(b.systemPart(conversation)))
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

// systemPart builds the one system message. The base prompt, the Clarify rule,
// the active Persona and the conversation note are resolved once here; the
// Persona is labelled so the model can tell instructions from history and Tool
// output, and it is appended after the Clarify rule so that rule survives any
// Persona. An empty prompt adds nothing. The conversation note, when present,
// stays inside this same labelled part.
func (b *PromptBuilder) systemPart(conversation bool) string {
	text := b.system()
	if clause := b.clarifyClause(); clause != "" {
		text += "\n\n" + clause
	}
	if b.persona != nil {
		snapshot := b.persona()
		if prompt := strings.TrimSpace(snapshot.Prompt); prompt != "" {
			if name := strings.TrimSpace(snapshot.Name); name == "" {
				text += "\n\nPersona instructions:\n" + prompt
			} else {
				text += fmt.Sprintf("\n\nPersona instructions for %q:\n", name) + prompt
			}
		}
	}
	if conversation {
		text += "\n\n" + ConversationNote
	}
	return text
}

// clarifyClause builds the one rule that depends on a live setting: the exact
// line the model must use when a request is addressed to it but cannot be
// understood. A nil source adds nothing, so a plain builder is unchanged. The
// line is quoted so its punctuation cannot be mistaken for the rule's.
func (b *PromptBuilder) clarifyClause() string {
	if b.clarify == nil {
		return ""
	}
	line := strings.TrimSpace(b.clarify())
	if line == "" {
		line = DefaultClarifyMessage
	}
	return "If the utterance is addressed to you but you cannot tell what is being asked, reply with exactly " +
		strconv.Quote(line) + " and nothing else."
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

// Tools offers every enabled Tool except Local-only ones. Local-only Tools
// are never shown to the model, so it cannot call them; a stored disable
// override removes a Tool from the next request's list.
func (b *PromptBuilder) Tools(reg *tool.Registry) []openai.ChatCompletionToolUnionParam {
	names := reg.Names()
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(names))
	for _, name := range names {
		info, ok := reg.Info(name)
		if !ok || info.LocalOnly || !info.Enabled {
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
