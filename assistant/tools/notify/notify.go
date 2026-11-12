// Package notify implements notify.phone, the one Tool that sends a
// Notification to the user's phone through ntfy. It is act-only and
// LocalOnlyNo: the result is just "Sent to your phone", and the Agent path may
// send a message the user dictated. The Tool never forwards a reply whose
// provenance is Local-only, so the privacy declaration of every other Tool is
// preserved.
package notify

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	core "github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Name is the canonical notify.phone Tool name.
const Name = "notify.phone"

// DefaultTitle is the notification title shown on the phone.
const DefaultTitle = "Assistant"

// Config is the tools.notify section.
type Config struct {
	// Title is shown as the notification title.
	Title string `yaml:"title"`
}

// DefaultConfig is the shipped Notify Tool configuration.
func DefaultConfig() Config { return Config{Title: DefaultTitle} }

// validate rejects an empty title, which would make a bare notification.
func (c Config) validate() error {
	if strings.TrimSpace(c.Title) == "" {
		return errors.New("title must not be empty")
	}
	return nil
}

// Module is the notify Tool domain.
type Module struct {
	cfg Config
}

// NewModule builds the notify module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "notify" }

// RequiredSecrets names the topic (generated once in the secrets table) and the
// optional server-issued ntfy access token. The token is never generated: ntfy
// only accepts tokens its own server issued.
func (*Module) RequiredSecrets() []string {
	return []string{core.TopicSecret, core.TokenSecret}
}

// Configure decodes tools.notify over the code defaults.
func (m *Module) Configure(section *yaml.Node) error {
	cfg := DefaultConfig()
	if section != nil {
		if err := section.Decode(&cfg); err != nil {
			return err
		}
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// In is one optional message. An empty message means "that": the Tool sends the
// Assistant's previous reply, which the pipeline recorded with its provenance.
type In struct {
	Message string `json:"message,omitempty"`
}

// Out reports what happened. A muted Notification is not an error: the user
// asked for something Quiet hours intentionally suppress.
type Out struct {
	Sent bool `json:"sent"`
	// Muted is true when Quiet hours suppressed the Notification.
	Muted bool `json:"muted,omitempty"`
	// Reason names the Quiet source, "quiet hours" or "do not disturb".
	Reason string `json:"reason,omitempty"`
	// Source is "message" or "last_reply" for debugging and tests.
	Source string `json:"source,omitempty"`
}

// New builds notify.phone over the shared Notification service and the
// in-memory last-reply store.
func New(cfg Config, service tool.Notifier, last *lastreply.Store) tool.Tool[In, Out] {
	return tool.Tool[In, Out]{
		Name: Name,
		Description: "Send a Notification to the user's phone through ntfy. " +
			"With no message, send the Assistant's previous reply.",
		Permission: tool.PermissionAct,
		// The notification leaves the Pi by design, so the Tool is not
		// Local-only. Its own result carries no private content.
		LocalOnly: tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in In) (Out, error) {
			if service == nil {
				return Out{}, &tool.ToolError{Say: "Notifications are not configured."}
			}
			message := strings.TrimSpace(in.Message)
			source := "message"
			if message == "" {
				reply, ok := last.Last()
				if !ok || strings.TrimSpace(reply.Text) == "" {
					return Out{}, &tool.ToolError{Say: "I don't have a recent reply to send."}
				}
				// Privacy: a Local-only Tool's output must never leave the Pi,
				// even when the user says "send that". The reply text is not
				// echoed in the error.
				if reply.LocalOnly {
					return Out{}, &tool.ToolError{Say: "I can't send that because it came from a local-only tool."}
				}
				message = strings.TrimSpace(reply.Text)
				source = "last_reply"
			}
			err := service.Send(ctx, core.Notification{
				Kind:    "phone",
				Title:   strings.TrimSpace(cfg.Title),
				Message: message,
			})
			if err != nil {
				var muted *core.MutedError
				if errors.As(err, &muted) {
					reason := strings.TrimSpace(muted.Reason)
					if reason == "" {
						reason = "quiet hours"
					}
					return Out{Muted: true, Reason: reason}, nil
				}
				// The service error may name the endpoint; it is logged by the
				// registry, but the spoken reply stays generic.
				return Out{}, &tool.ToolError{Say: "I couldn't send that to your phone."}
			}
			return Out{Sent: true, Source: source}, nil
		},
		Speak: func(out Out) string {
			if out.Muted {
				return "Notifications are muted during " + out.Reason + "."
			}
			return "Sent to your phone."
		},
		// Every Example must route here on the semantic Fast path. They are
		// deliberately phone-specific; broad "notify" wording stays out.
		Examples: []string{
			"send that to my phone",
			"send this to my phone",
			"send it to my phone",
			"send that to my phone please",
			"send that to my phone right now",
			"push that to my phone",
			"push this to my phone",
			"send me that on my phone",
			"send your answer to my phone",
			"send the last answer to my phone",
			"send your last reply to my phone",
			"send that answer to my phone",
			"send this answer to my phone",
			"put that on my phone",
			"copy that to my phone",
			"forward that to my phone",
			"text me that",
			"notify my phone",
			"ping my phone",
			"send me a notification",
			"notify my phone that I'm on my way",
			"send buy milk to my phone",
		},
		Extract: extractPhone,
	}
}

// explicitSend matches "send/push/text/forward/copy <message> to/on my phone".
var explicitSend = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:send|push|text|forward|copy)\s+(?:me\s+)?(.+?)\s+(?:to|on)\s+my\s+phone(?:\s+(?:please|now|right\s+now|when\s+you\s+can))?\s*[.!?]?\s*$`)

// notifyThat matches "notify my phone that <message>".
var notifyThat = regexp.MustCompile(`(?i)^\s*(?:please\s+)?notify\s+my\s+phone\s+(?:that\s+)?(.+?)\s*[.!?]?\s*$`)

// textMe matches "text me <message>" and "ping me <message>".
var textMe = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:text|ping)\s+me(?:\s+(.+?))?\s*[.!?]?\s*$`)

// extractPhone fills the optional message from explicit phrasing. A
// demonstrative ("that", "this", "your answer") becomes an empty message so the
// handler resolves the previous reply at call time, not at extraction time.
func extractPhone(text string) (In, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	// A question or a device-control phrase is not a send request even if a
	// near embedding brings it here.
	if strings.Contains(lower, "phone number") ||
		strings.Contains(lower, "on silent") ||
		strings.HasPrefix(lower, "where") ||
		strings.HasPrefix(lower, "what") ||
		strings.HasPrefix(lower, "call") ||
		strings.HasPrefix(lower, "turn ") ||
		strings.HasPrefix(lower, "enable ") ||
		strings.HasPrefix(lower, "disable ") ||
		strings.HasPrefix(lower, "read ") {
		return In{}, false
	}
	if !containsAny(lower, "phone", "notify me", "text me", "ping me", "notification") {
		return In{}, false
	}
	for _, pattern := range []*regexp.Regexp{explicitSend, notifyThat, textMe} {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		message := ""
		if len(match) > 1 {
			message = messageFromMatch(match[1])
		}
		return In{Message: message}, true
	}
	return In{}, true
}

// messageFromMatch turns a captured phrase into a message. A demonstrative
// refers to the previous reply and therefore produces an empty message.
func messageFromMatch(raw string) string {
	cleaned := strings.TrimSpace(strings.Trim(raw, `"'`))
	switch strings.ToLower(strings.TrimRight(cleaned, ".!?")) {
	case "", "that", "this", "it", "that answer", "this answer", "that message",
		"the answer", "your answer", "the last answer", "your last reply",
		"the last reply", "your reply", "your message":
		return ""
	default:
		return cleaned
	}
}

// containsAny reports whether text contains one of the phrases.
func containsAny(text string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// Register adds notify.phone. Missing seams are allowed so the conformance
// suite can inspect metadata without a database or network.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	return tool.Add(r, New(m.cfg, deps.Notify, deps.LastReply))
}
