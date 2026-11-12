package assistant

import (
	"context"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// RepeatIn has no fields: "say that again" repeats whatever was last said,
// rather than taking the current utterance as its input.
type RepeatIn struct{}

// RepeatOut is the previous completed reply with its provenance.
type RepeatOut struct {
	Text      string    `json:"text"`
	Tool      string    `json:"tool,omitempty"`
	Path      string    `json:"path,omitempty"`
	LocalOnly bool      `json:"local_only,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// HelpIn has no fields.
type HelpIn struct{}

// HelpTool describes one enabled Tool. The help result is Local-only, so it
// may name Local-only Tools without exposing them to the Agent path.
type HelpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Permission  string `json:"permission"`
	LocalOnly   bool   `json:"local_only,omitempty"`
}

// HelpOut is the generated capability list and the one-line spoken form.
type HelpOut struct {
	Tools []HelpTool `json:"tools"`
	Text  string     `json:"text"`
}

// NewRepeat builds assistant.repeat. It reads the shared last-reply store at
// call time; a nil store or an empty history is a clear failure, never a
// repeated echo of the request itself.
func NewRepeat(store *lastreply.Store) tool.Tool[RepeatIn, RepeatOut] {
	return tool.Tool[RepeatIn, RepeatOut]{
		Name:        RepeatName,
		Description: "Repeat the last reply the Assistant gave.",
		Permission:  tool.PermissionRead,
		// The previous reply may be a Local-only result, and the Agent path
		// must never see it as Tool output. Declaring the repeat Tool itself
		// Local-only is the explicit, safe choice.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, RepeatIn) (RepeatOut, error) {
			reply, ok := store.Last()
			if !ok || strings.TrimSpace(reply.Text) == "" {
				return RepeatOut{}, &tool.ToolError{Say: "I haven't said anything yet."}
			}
			return RepeatOut{
				Text:      reply.Text,
				Tool:      reply.Tool,
				Path:      reply.Path,
				LocalOnly: reply.LocalOnly,
				At:        reply.At,
			}, nil
		},
		Speak: func(out RepeatOut) string { return out.Text },
		Examples: []string{
			"say that again",
			"what did you say",
			"repeat that",
			"can you repeat that",
			"say it again",
			"what was that",
			"I missed that",
			"come again",
		},
	}
}

// NewHelp builds assistant.help. The handler walks the registry every call and
// keeps only Tools whose effective Enabled flag is true, so a Dashboard
// disable applies to the next request and no feature list lives in code.
func NewHelp(registry *tool.Registry) tool.Tool[HelpIn, HelpOut] {
	return tool.Tool[HelpIn, HelpOut]{
		Name:        HelpName,
		Description: "List the Tools that are enabled right now.",
		Permission:  tool.PermissionRead,
		// The list would name Local-only Tools, which the Agent path and the
		// MCP surface must not learn. Keeping the whole result on the Pi is
		// simpler and stricter than filtering the list per caller.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, HelpIn) (HelpOut, error) {
			if registry == nil {
				return HelpOut{Text: "I don't have any tools enabled right now."}, nil
			}
			names := registry.Names()
			out := HelpOut{Tools: make([]HelpTool, 0, len(names))}
			descriptions := make([]string, 0, len(names))
			for _, name := range names {
				info, ok := registry.Info(name)
				if !ok || !info.Enabled {
					continue
				}
				out.Tools = append(out.Tools, HelpTool{
					Name:        info.Name,
					Description: info.Description,
					Permission:  string(info.Permission),
					LocalOnly:   info.LocalOnly,
				})
				if text := strings.TrimSpace(info.Description); text != "" {
					descriptions = append(descriptions, text)
				}
			}
			out.Text = helpText(descriptions)
			return out, nil
		},
		Speak: func(out HelpOut) string { return out.Text },
		Examples: []string{
			"what can you do",
			"what can you help with",
			"what are your capabilities",
			"what are you able to do",
			"list your tools",
			"what tools do you have",
			"help",
			"tell me what you can do",
		},
	}
}

// helpText joins the enabled Tools' own descriptions. It is deliberately
// generated from the registry, so a new module appears without editing this
// package.
func helpText(descriptions []string) string {
	if len(descriptions) == 0 {
		return "I don't have any tools enabled right now."
	}
	return "Here's what I can do: " + strings.Join(descriptions, " ")
}
