// Package note implements note.remember, note.recall and note.forget.
//
// #32 owner decision: Notes are deliberately not Local-only. The owner wants
// the Agent path, and later Hermes, to see and add notes because they may need
// to interact with them. ADR-0002 still governs the egress: note text leaves
// the Pi only through the one prompt builder, in a request the user made, never
// in a background push. Deletion is confirm because it is hard to undo. See
// docs/adr/0005-notes-and-lists-are-agent-visible.md.
package note

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Canonical Tool names.
const (
	RememberName = "note.remember"
	RecallName   = "note.recall"
	ForgetName   = "note.forget"
)

// Config is the tools.note section.
type Config struct {
	// MinScore is the cosine floor for a semantic recall match.
	MinScore float64 `yaml:"min_score"`
	// MaxResults bounds the notes one recall returns.
	MaxResults int `yaml:"max_results"`
	// MaxText is the longest note, in runes.
	MaxText int `yaml:"max_text"`
	// MaxNotes bounds the notes a recall embeds, newest first.
	MaxNotes int `yaml:"max_notes"`
}

// DefaultConfig is the shipped note configuration.
func DefaultConfig() Config {
	return Config{MinScore: 0.35, MaxResults: 3, MaxText: 2000, MaxNotes: 500}
}

// validate rejects values that would make recall useless or unbounded.
func (c Config) validate() error {
	if c.MinScore <= 0 || c.MinScore > 1 {
		return errors.New("min_score must be in (0, 1]")
	}
	if c.MaxResults < 1 || c.MaxResults > 20 {
		return errors.New("max_results must be from 1 to 20")
	}
	if c.MaxText < 100 || c.MaxText > 10000 {
		return errors.New("max_text must be from 100 to 10000 runes")
	}
	if c.MaxNotes < 1 || c.MaxNotes > 5000 {
		return errors.New("max_notes must be from 1 to 5000")
	}
	return nil
}

// Module is the note Tool domain.
type Module struct {
	cfg     Config
	manager *Manager
	deps    tool.Deps
}

// NewModule builds the note module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "note" }

// Configure decodes tools.note over the code defaults.
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

// Manager exposes the live Manager for the Dashboard and the tests. It is nil
// before Register.
func (m *Module) Manager() *Manager { return m.manager }

// RememberIn is one fact to keep.
type RememberIn struct {
	Text string `json:"text"`
}

// RememberOut is the note that was stored.
type RememberOut = Note

// RecallIn is a search by meaning across the notes.
type RecallIn struct {
	Query string `json:"query"`
}

// RecallOut is the best matches, closest first. An empty list is a real answer,
// not an error.
type RecallOut struct {
	Query string  `json:"query"`
	Notes []Match `json:"notes"`
}

// ForgetIn names one note by id or by a phrase from its text.
type ForgetIn struct {
	ID    string `json:"id,omitempty"`
	Query string `json:"query,omitempty"`
}

// ForgetOut reports how many notes were deleted.
type ForgetOut struct {
	Deleted int `json:"deleted"`
}

// NewRemember builds note.remember. It is act: adding a note is easy to undo.
func NewRemember(m *Manager, cfg Config) tool.Tool[RememberIn, RememberOut] {
	return tool.Tool[RememberIn, RememberOut]{
		Name:        RememberName,
		Description: "Remember a short personal fact or note for later recall.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in RememberIn) (RememberOut, error) {
			note, err := m.Remember(ctx, in.Text)
			return note, toolError(err)
		},
		Speak: func(out RememberOut) string {
			if out.ID == "" {
				return "I couldn't save that note."
			}
			return "Noted."
		},
		Examples: []string{
			"remember that I parked on level 3",
			"remember I left my bike in the shed",
			"make a note that the boiler needs servicing",
			"note that I owe Sam ten pounds",
			"add a note that the spare key is under the pot",
			"remember the wifi password is in the drawer",
			"make a note that the bins go out on Tuesday",
			"note down that the plumber is coming Friday",
			"remember where I put the passport",
			"add a note that I need to renew the car insurance",
			"remember that the library books are due back",
			"remember the car is parked in bay 12",
			"make a note that the oven is on the blink",
			"note down the meeting is at four",
			"add a note that Sam's number is in the kitchen",
			"remember that I lent the drill to Alex",
			"remember the filter needs changing in March",
			"make a note that the front door code is 4321",
			"note that the parcel is behind the bin",
			"remember the spare batteries are in the hall cupboard",
		},
		Extract: extractRemember,
	}
}

// NewRecall builds note.recall. Search is by meaning, with exact substring
// matches winning before any embedding call.
func NewRecall(m *Manager) tool.Tool[RecallIn, RecallOut] {
	return tool.Tool[RecallIn, RecallOut]{
		Name:        RecallName,
		Description: "Search the personal notes by meaning and read the closest match.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in RecallIn) (RecallOut, error) {
			query := strings.TrimSpace(in.Query)
			if query == "" {
				return RecallOut{}, &tool.ToolError{Say: "What should I look for in your notes?"}
			}
			notes, err := m.Recall(ctx, query)
			if err != nil {
				return RecallOut{}, &tool.ToolError{Say: "I couldn't search my notes right now."}
			}
			return RecallOut{Query: query, Notes: notes}, nil
		},
		Speak: func(out RecallOut) string {
			switch len(out.Notes) {
			case 0:
				return "I don't have a note about that."
			case 1:
				return "You noted: " + out.Notes[0].Text
			default:
				return fmt.Sprintf("I found %d notes. The closest: %s", len(out.Notes), out.Notes[0].Text)
			}
		},
		Examples: []string{
			"what do I remember about the boiler",
			"what did I note about the plumber",
			"do I have a note about the spare key",
			"recall my note about the car insurance",
			"search my notes for the wifi password",
			"what do I know about the library books",
			"what have I noted about the bins",
			"find my note about the passport",
			"where did I park",
			"what are my notes about the bike",
			"what did I write down about Sam",
			"where did I leave the drill",
			"where did I put the spare key",
			"what do I remember about the front door code",
			"do I have a note about the oven",
			"recall my note about the meeting",
			"search my notes for the parcel",
			"what do I know about the car insurance",
			"what have I noted about Sam",
			"find my note about the batteries",
		},
		Extract: extractRecall,
	}
}

// NewForget builds note.forget. It is confirm and AgentOnly: deletion is hard
// to undo, so the model must ask through a Confirmation and the semantic Fast
// path never sees it.
func NewForget(m *Manager) tool.Tool[ForgetIn, ForgetOut] {
	return tool.Tool[ForgetIn, ForgetOut]{
		Name:        ForgetName,
		Description: "Delete one personal note by id or by a phrase from its text.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   true,
		Summary: func(in ForgetIn) string {
			if strings.TrimSpace(in.ID) != "" {
				return "Delete that note?"
			}
			query := strings.TrimSpace(in.Query)
			if query == "" {
				return "Delete a note?"
			}
			return fmt.Sprintf("Delete the note about %q?", query)
		},
		Handler: func(ctx context.Context, in ForgetIn) (ForgetOut, error) {
			var (
				deleted int
				err     error
			)
			if strings.TrimSpace(in.ID) != "" {
				deleted, err = m.Delete(ctx, in.ID)
			} else {
				deleted, err = m.ForgetQuery(ctx, in.Query)
			}
			return ForgetOut{Deleted: deleted}, toolError(err)
		},
		Speak: func(out ForgetOut) string {
			if out.Deleted == 0 {
				return "I don't have a note like that."
			}
			return "Forgotten."
		},
	}
}

// Register adds every note Tool.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.deps = deps
	m.manager = NewManager(Options{
		DB:     deps.Store,
		Embed:  deps.Embed,
		Now:    deps.Clock,
		Logger: deps.Logger,
		Config: m.cfg,
	})
	if err := tool.Add(r, NewRemember(m.manager, m.cfg)); err != nil {
		return err
	}
	if err := tool.Add(r, NewRecall(m.manager)); err != nil {
		return err
	}
	return tool.Add(r, NewForget(m.manager))
}

// rememberPatterns are the Fast path lead-ins for note.remember. Each captures
// only the fact the user asked to keep.
var rememberPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?remember\s+(?:that\s+)?(.+?)\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?make\s+a\s+note\s+(?:that\s+)?(.+?)\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?add\s+a\s+note\s+(?:that\s+)?(.+?)\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?note\s+(?:down\s+)?(?:that\s+)?(.+?)\s*[.!]?\s*$`),
}

// extractRemember fills RememberIn from an explicit "remember …" phrasing.
func extractRemember(text string) (RememberIn, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || strings.HasPrefix(lower, "what") || strings.HasPrefix(lower, "do ") ||
		strings.HasPrefix(lower, "did ") || strings.HasPrefix(lower, "where") {
		return RememberIn{}, false
	}
	for _, pattern := range rememberPatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		fact := strings.TrimSpace(match[1])
		if fact == "" || mentionsExternalStore(fact) {
			return RememberIn{}, false
		}
		// "remember to call the dentist" is a Reminder request, not a note.
		// The reminder.set Examples own that phrasing.
		if strings.HasPrefix(strings.ToLower(fact), "to ") {
			return RememberIn{}, false
		}
		return RememberIn{Text: fact}, true
	}
	return RememberIn{}, false
}

// recallPatterns are the Fast path lead-ins for note.recall. Each captures the
// subject to search for, never the whole sentence.
var recallPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what\s+(?:do|did)\s+i\s+(?:remember|note|noted)\s+(?:about|on)\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what\s+have\s+i\s+(?:noted|remembered)\s+about\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:do|did)\s+i\s+have\s+(?:a\s+)?note\s+(?:about|on)\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:recall|search)\s+(?:my\s+)?notes?\s+(?:for|about)\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what\s+(?:are|were)\s+my\s+notes?\s+(?:on|about)\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what\s+do\s+i\s+know\s+about\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what\s+did\s+i\s+write\s+down\s+about\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:find|look\s+up)\s+(?:my\s+)?note\s+(?:about|on)\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*where\s+did\s+i\s+(?:park|leave|put)\s+(.+?)\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*where\s+did\s+i\s+park\s*[?]?\s*$`),
}

// extractRecall fills RecallIn from a question about the notes.
func extractRecall(text string) (RecallIn, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || mentionsExternalStore(lower) {
		return RecallIn{}, false
	}
	for _, pattern := range recallPatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		query := ""
		if len(match) > 1 {
			query = strings.TrimSpace(match[1])
		}
		if query == "" && strings.Contains(lower, "where did i park") {
			query = "where did I park"
		}
		if query == "" {
			continue
		}
		return RecallIn{Query: query}, true
	}
	return RecallIn{}, false
}

// mentionsExternalStore rejects a question about a phone, calendar or notes app.
// The Pi's own notes are the only ones this Tool can answer.
func mentionsExternalStore(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range []string{"phone", "iphone", "android", "calendar", "outlook", "notes app", "reminder", "shopping list", "to do list", "to-do list", "todo list"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// toolError turns a manager validation error into the spoken Tool error.
func toolError(err error) error {
	if err == nil {
		return nil
	}
	var spoken *Error
	if errors.As(err, &spoken) {
		return &tool.ToolError{Say: spoken.Message}
	}
	return err
}
