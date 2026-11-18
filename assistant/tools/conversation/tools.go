package conversation

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// StartIn has no arguments: "let's chat" turns the mode on as it is spoken.
type StartIn struct{}

// StartOut is the spoken acknowledgement. The phrase book may replace it with
// the active Persona's wording.
type StartOut struct {
	Text string `json:"text"`
}

// EndIn has no arguments.
type EndIn struct{}

// EndOut is the spoken acknowledgement. Ending is silent apart from this line.
type EndOut struct {
	Text string `json:"text"`
}

// NewStart builds conversation.start. A nil mode is a clear failure rather
// than a mode that appears to be on.
func NewStart(mode tool.Conversation) tool.Tool[StartIn, StartOut] {
	return tool.Tool[StartIn, StartOut]{
		Name:        StartName,
		Description: "Start conversation mode so the next Agent turns may give longer answers.",
		Permission:  tool.PermissionAct,
		// The latch is in memory on the Pi and the acknowledgement is fixed
		// wording, so nothing here is model-visible or sent off the device.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, StartIn) (StartOut, error) {
			if mode == nil {
				return StartOut{}, &tool.ToolError{Say: "Conversation mode is not available."}
			}
			mode.Start()
			return StartOut{Text: "Let's talk."}, nil
		},
		Speak: func(out StartOut) string { return out.Text },
		Examples: []string{
			"let's chat",
			"let's talk",
			"can we just chat",
			"can we talk for a bit",
			"I want to talk with you",
			"I want to have a chat",
			"I'd like to chat",
			"I'd like to talk",
			"talk with me for a bit",
			"chat with me for a bit",
			"let's have a nice chat",
			"can we chat for a while",
			"I want to chat with you",
			"let's have a proper chat",
			"can we just talk",
			"I'd like to have a chat",
			"let's have a chat",
			"I need someone to talk to",
			"can we talk a little",
			"talk with me for a while",
		},
	}
}

// NewEnd builds conversation.end. It is the explicit off switch; the mode also
// ends on its own after the live idle window.
func NewEnd(mode tool.Conversation) tool.Tool[EndIn, EndOut] {
	return tool.Tool[EndIn, EndOut]{
		Name:        EndName,
		Description: "End conversation mode and go back to the short spoken budget.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, EndIn) (EndOut, error) {
			if mode == nil {
				return EndOut{}, &tool.ToolError{Say: "Conversation mode is not available."}
			}
			mode.End()
			return EndOut{Text: "Okay."}, nil
		},
		Speak: func(out EndOut) string { return out.Text },
		Examples: []string{
			"end the conversation",
			"end conversation",
			"finish the conversation",
			"let's end this conversation",
			"we're done talking",
			"I'm done talking",
			"no more chatting",
			"I'm finished talking",
			"leave conversation mode",
			"exit conversation mode",
			"I want to end our conversation",
			"the conversation is over",
			"let's stop the conversation",
			"that's the end of our chat",
			"I'd like to end this conversation",
			"end our chat",
			"I'm done with this conversation",
			"let's wrap up the conversation",
			"wrap up this chat",
			"finish our conversation",
		},
	}
}
