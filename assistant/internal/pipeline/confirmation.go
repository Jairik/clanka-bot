package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// CancelledReply is spoken when a pending Confirmation is dropped.
const CancelledReply = "Cancelled."

// DoneReply stands in when a confirmed Tool has no Speak renderer.
const DoneReply = "Done."

// clearYes is the conservative set of utterances that count as a spoken yes.
// Anything else drops the pending call.
var clearYes = map[string]struct{}{
	"yes":         {},
	"yeah":        {},
	"yep":         {},
	"yes please":  {},
	"sure":        {},
	"ok":          {},
	"okay":        {},
	"do it":       {},
	"go ahead":    {},
	"confirm":     {},
	"affirmative": {},
	"please do":   {},
}

// Confirmation resolves the pending Confirmation before any other routing
// check. The registry holds the pending call, so nothing here can run a
// confirm Tool without a clear yes.
type Confirmation struct {
	registry *tool.Registry
	logger   *slog.Logger
}

// NewConfirmation wires the registry-backed Confirmation stage.
func NewConfirmation(reg *tool.Registry, logger *slog.Logger) *Confirmation {
	if logger == nil {
		logger = slog.Default()
	}
	return &Confirmation{registry: reg, logger: logger}
}

// Resolve runs the stored call on a clear yes, or drops it and asks the
// caller to route the utterance normally.
func (c *Confirmation) Resolve(ctx context.Context, req tool.Request, transcript string) ConfirmationResult {
	if c == nil || c.registry == nil {
		return ConfirmationResult{}
	}
	pending, ok := c.registry.Pending()
	if !ok {
		return ConfirmationResult{}
	}

	if isClearYes(transcript) {
		req.Path = tool.PathConfirmation
		result, done, err := c.registry.ExecutePendingFor(tool.WithRequest(ctx, req), pending.ID)
		if errors.Is(err, tool.ErrNoPending) {
			// An expired, replaced, or consumed call leaves this yes unanswered.
			c.logger.Info("confirmation no longer pending", "request_id", req.ID)
			return ConfirmationResult{}
		}
		if err != nil {
			c.logger.Error("confirmation failed",
				"request_id", req.ID, "tool", done.Tool, "error", err.Error())
			return ConfirmationResult{Handled: true, Reply: Reply{
				Text: tool.FailureMessage(err), Tool: done.Tool, Path: tool.PathConfirmation,
			}}
		}
		spoken, ok := c.registry.Speak(done.Tool, result)
		if !ok {
			spoken = DoneReply
		}
		c.logger.Info("confirmation accepted", "request_id", req.ID, "tool", done.Tool)
		confirmReply := Reply{Text: spoken, Tool: done.Tool, Path: tool.PathConfirmation}
		// A confirmed Tool whose result carries a one-utterance rate keeps it
		// on the Reply, so it is spoken and stored exactly like a Fast path one.
		confirmReply.SpeechRate = rateOf(result)
		return ConfirmationResult{
			Handled: true,
			Reply:   confirmReply,
			Call:    confirmedCall(c.registry, done, result),
		}
	}

	// No, silence, Barge-in or an unrelated request drops the call. The caller
	// still routes the utterance, so a new request is not lost.
	if !c.registry.DropPendingFor(pending.ID) {
		return ConfirmationResult{}
	}
	c.logger.Info("confirmation cancelled", "request_id", req.ID, "tool", pending.Tool)
	return ConfirmationResult{Prefix: CancelledReply}
}

// DropPending discards a waiting call. Barge-in (#25) uses it when the wake
// word abandons playback so a stale yes cannot run later.
func (c *Confirmation) DropPending() (tool.PendingConfirmation, bool) {
	if c == nil || c.registry == nil {
		return tool.PendingConfirmation{}, false
	}
	return c.registry.DropPending()
}

// confirmedCall records the executed call for the shared history.
func confirmedCall(reg *tool.Registry, pending tool.PendingConfirmation, result any) *agent.ToolCall {
	call := &agent.ToolCall{Name: pending.Tool, Args: pending.Args}
	if reg != nil {
		// A confirmed result keeps its declared provenance, exactly like the
		// Fast path: a result value cannot override its Tool's static flag.
		call.LocalOnly = reg.Provenance(pending.Tool, result)
	}
	if payload, err := json.Marshal(result); err == nil {
		call.Result = payload
	}
	return call
}

// isClearYes matches an explicit agreement after normalisation. A comma inside
// an agreement ("yes, please") is punctuation the yes set does not store, and
// it is internal so TrimSentence keeps it; retry without commas and close the
// gap they leave so the clear-yes lookup still lands.
func isClearYes(transcript string) bool {
	normalized := NormalizePhrase(transcript)
	if _, ok := clearYes[normalized]; ok {
		return true
	}
	if !strings.Contains(normalized, ",") {
		return false
	}
	commaless := strings.Join(strings.Fields(strings.ReplaceAll(normalized, ",", " ")), " ")
	_, ok := clearYes[commaless]
	return ok
}
