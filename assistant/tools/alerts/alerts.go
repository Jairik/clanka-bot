// Package alerts implements the alerts.next Tool: one Local-only answer for
// everything scheduled next across Timers, Alarms and Reminders. It is a leaf
// read Tool: it reads each manager's List seam directly and never calls another
// Tool.
package alerts

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical alerts.next Tool name.
const Name = "alerts.next"

// maxItems bounds the spoken answer: the soonest item from each of the three
// scheduling sources (Timer, Alarm and Reminder).
const maxItems = 3

// Item is one scheduled thing merged from a Timer, Alarm or Reminder. At is
// when it fires and is used only for ordering; Text is the spoken clause.
type Item struct {
	Source string    `json:"source"` // "timer", "alarm" or "reminder"
	ID     string    `json:"id,omitempty"`
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
}

// Out is the merged answer. Items are the soonest items, ordered by when they
// fire. Unavailable names the sources that could not be read, so a failed
// source never silently hides the ones that answered.
type Out struct {
	Items       []Item   `json:"items"`
	Unavailable []string `json:"unavailable,omitempty"`
}

// Source is one scheduling manager's read seam. The boolean is false when the
// source could not be read (an unwired manager or a store failure), which the
// Tool reports honestly instead of claiming nothing is scheduled there.
type Source interface {
	NextAlerts() ([]Item, bool)
}

// In has no fields: alerts.next takes no arguments, which keeps it eligible
// for the Fast path.
type In struct{}

// Module is the alerts Tool domain. It composes the three scheduling sources
// by their list seams; no scheduling code changes.
type Module struct {
	sources []namedSource
}

type namedSource struct {
	name   string
	source Source
}

// NewModule builds the alerts module from its three scheduling sources.
func NewModule(timers, alarms, reminders Source) *Module {
	return &Module{sources: []namedSource{
		{name: "timer", source: timers},
		{name: "alarm", source: alarms},
		{name: "reminder", source: reminders},
	}}
}

// Name is the module name and config section suffix.
func (*Module) Name() string { return "alerts" }

// Register adds alerts.next to the registry.
func (m *Module) Register(r *tool.Registry, _ tool.Deps) error {
	return tool.Add(r, tool.Tool[In, Out]{
		Name: Name,
		Description: "Report the next scheduled thing across timers, alarms and reminders in one answer.",
		Permission:  tool.PermissionRead,
		// The answer names private timer labels, alarm labels and reminder
		// messages, so it never leaves the Pi: the Agent path cannot see or
		// call it, and neither can Hermes or the MCP surface.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, In) (Out, error) {
			return collect(m.sources), nil
		},
		Speak: speak,
		Examples: []string{
			"what's next",
			"what's scheduled",
			"what's coming up",
			"what do I have scheduled",
			"what's next on my schedule",
			"what's my next alert",
			"tell me what's scheduled",
			"what have I got coming up",
		},
	})
}

// collect reads every source, merges the items in fire-time order and records
// the ones that could not be read. A nil source or an unavailable one is named
// in Unavailable; it never removes the other sources' items from the answer.
func collect(sources []namedSource) Out {
	var out Out
	for _, ns := range sources {
		if ns.source == nil {
			out.Unavailable = append(out.Unavailable, ns.name)
			continue
		}
		items, ok := ns.source.NextAlerts()
		if !ok {
			out.Unavailable = append(out.Unavailable, ns.name)
			continue
		}
		out.Items = append(out.Items, items...)
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		return out.Items[i].At.Before(out.Items[j].At)
	})
	if len(out.Items) > maxItems {
		out.Items = out.Items[:maxItems]
	}
	return out
}

// speak renders the merged answer as one short sentence. An empty answer is
// honest about why it is empty: nothing scheduled, or sources that failed.
func speak(out Out) string {
	if len(out.Items) == 0 {
		if len(out.Unavailable) == 0 {
			return "Nothing is scheduled."
		}
		return "I couldn't check " + joinNames(out.Unavailable) + "."
	}
	parts := make([]string, 0, len(out.Items)+1)
	for i, item := range out.Items {
		if i == 0 {
			parts = append(parts, upperFirst(item.Text))
			continue
		}
		parts = append(parts, item.Text)
	}
	sentence := strings.Join(parts, "; ") + "."
	if len(out.Unavailable) > 0 {
		sentence += " I couldn't check " + joinNames(out.Unavailable) + "."
	}
	return sentence
}

// joinNames renders a short "a, b and c" list of source names.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// upperFirst capitalises the first rune without splitting a multi-byte letter.
func upperFirst(text string) string {
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	return strings.ToUpper(string(first)) + text[size:]
}
