// Package list implements list.add, list.read and list.remove over the fixed
// Shopping and To-do lists.
//
// #32 owner decision: the set of lists is fixed in code; new lists cannot be
// created by voice. Lists are deliberately not Local-only: the owner wants the
// Agent path, and later Hermes, to see and edit them. ADR-0002 still governs
// the egress, so contents leave the Pi only through the one prompt builder in
// a request the user made. See
// docs/adr/0005-notes-and-lists-are-agent-visible.md.
package list

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Canonical Tool names.
const (
	AddName    = "list.add"
	ReadName   = "list.read"
	RemoveName = "list.remove"
	DoneName   = "list.done"
)

// The fixed List keys. They are also the database CHECK values.
const (
	KeyShopping = "shopping"
	KeyTodo     = "to-do"
)

// Config is the tools.list section.
type Config struct {
	// MaxItems bounds one list so a runaway caller cannot grow a page or a
	// spoken answer without limit.
	MaxItems int `yaml:"max_items"`
	// MaxItemText is the longest item text, in runes.
	MaxItemText int `yaml:"max_item_text"`
}

// DefaultConfig is the shipped list configuration.
func DefaultConfig() Config { return Config{MaxItems: 200, MaxItemText: 200} }

func (c Config) validate() error {
	if c.MaxItems < 1 || c.MaxItems > 1000 {
		return errors.New("max_items must be from 1 to 1000")
	}
	if c.MaxItemText < 10 || c.MaxItemText > 1000 {
		return errors.New("max_item_text must be from 10 to 1000 runes")
	}
	return nil
}

// Module is the list Tool domain.
type Module struct {
	cfg     Config
	manager *Manager
}

// NewModule builds the list module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "list" }

// Configure decodes tools.list over the code defaults.
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

// AddIn names the fixed list and the item to add.
type AddIn struct {
	List string `json:"list"`
	Item string `json:"item"`
}

// AddOut reports the stored item. Added is false when the item was already on
// the list, which is a real answer rather than an error.
type AddOut struct {
	Item  Item `json:"item"`
	Added bool `json:"added"`
}

// ReadIn optionally narrows the read to one list; empty reads both.
type ReadIn struct {
	List string `json:"list,omitempty"`
}

// ReadOut holds the open items of one or both lists.
type ReadOut struct {
	Lists []List `json:"lists"`
}

// RemoveIn names the list and the spoken item to remove.
type RemoveIn struct {
	List string `json:"list"`
	Item string `json:"item"`
}

// RemoveOut is the removed item, or a zero item when nothing matched.
type RemoveOut struct {
	Removed bool `json:"removed"`
	Item    Item `json:"item"`
}

// DoneIn names the list and the spoken item to tick. An empty List searches
// both fixed lists for the one match.
type DoneIn struct {
	List string `json:"list"`
	Item string `json:"item"`
}

// DoneOut is the ticked item. Ticked is false when nothing matched, which is a
// spoken refusal rather than a guess.
type DoneOut struct {
	Ticked bool `json:"ticked"`
	Item   Item `json:"item"`
}

// NewAdd builds list.add. It is act: an added item is easy to remove.
func NewAdd(m *Manager) tool.Tool[AddIn, AddOut] {
	return tool.Tool[AddIn, AddOut]{
		Name:        AddName,
		Description: "Add an item to the shopping list or the to-do list.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in AddIn) (AddOut, error) {
			key, ok := ParseKey(in.List)
			if !ok {
				return AddOut{}, &tool.ToolError{Say: "I only have a shopping list and a to-do list."}
			}
			item, added, err := m.Add(ctx, key, in.Item)
			return AddOut{Item: item, Added: added}, toolError(err)
		},
		Speak: func(out AddOut) string {
			name := Title(out.Item.List)
			if !out.Added {
				return out.Item.Text + " is already on the " + strings.ToLower(name) + " list."
			}
			return "Added " + out.Item.Text + " to the " + strings.ToLower(name) + " list."
		},
		Examples: []string{
			"add milk to the shopping list",
			"put eggs on the shopping list",
			"add bread to my shopping list",
			"put toothpaste on the shopping list",
			"add butter to the shopping list please",
			"can you add coffee to the shopping list",
			"add apples to my shopping list",
			"put bananas on the shopping list",
			"add cheese to the shopping list",
			"put rice on my shopping list",
			"add orange juice to the shopping list",
			"put kitchen roll on my shopping list",
			"add frozen peas to the shopping list",
			"put yoghurt on the shopping list",
			"add cereal to my shopping list",
			"put pasta on the shopping list",
			"add carrots to my shopping list",
			"put onions on the shopping list",
			"add potatoes to my shopping list",
			"put tomatoes on the shopping list",
		},
		Extract: extractAdd,
	}
}

// NewRead builds list.read. It is read: no side effects.
func NewRead(m *Manager) tool.Tool[ReadIn, ReadOut] {
	return tool.Tool[ReadIn, ReadOut]{
		Name:        ReadName,
		Description: "Read the shopping list or the to-do list, or both.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in ReadIn) (ReadOut, error) {
			key := ""
			if strings.TrimSpace(in.List) != "" {
				var ok bool
				key, ok = ParseKey(in.List)
				if !ok {
					return ReadOut{}, &tool.ToolError{Say: "I only have a shopping list and a to-do list."}
				}
			}
			lists, err := m.Read(ctx, key)
			return ReadOut{Lists: lists}, toolError(err)
		},
		Speak: SpeakRead,
		Examples: []string{
			"what's on my shopping list",
			"read me the shopping list",
			"what's on my to-do list",
			"show me my to do list",
			"what do I need to buy",
			"what's left on my shopping list",
			"read out my to do list",
			"tell me what's on my shopping list",
			"what's on the shopping list",
			"what do I still need to buy",
			"show me the to-do list",
			"what's on the list for shopping",
			"show me the shopping list",
			"show me my shopping list",
		},
		Extract: extractRead,
	}
}

// NewRemove builds list.remove. A spoken item is resolved through the shared
// name matcher; it never forces a guess.
func NewRemove(m *Manager) tool.Tool[RemoveIn, RemoveOut] {
	return tool.Tool[RemoveIn, RemoveOut]{
		Name:        RemoveName,
		Description: "Remove one item from the shopping list or the to-do list.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in RemoveIn) (RemoveOut, error) {
			key, ok := ParseKey(in.List)
			if !ok {
				return RemoveOut{}, &tool.ToolError{Say: "I only have a shopping list and a to-do list."}
			}
			item, near, err := m.RemoveWithNear(ctx, key, in.Item)
			if err != nil {
				if errors.Is(err, ErrItemNotFound) {
					// A near miss is one spoken question, not a removed row: no
					// item changes and no state is kept.
					if near != "" {
						return RemoveOut{}, &tool.ToolError{Say: namematch.NearQuestion(near, "list item")}
					}
					return RemoveOut{}, &tool.ToolError{Say: "I couldn't find that on the " + strings.ToLower(Title(key)) + " list."}
				}
				return RemoveOut{}, toolError(err)
			}
			return RemoveOut{Removed: true, Item: item}, nil
		},
		Speak: func(out RemoveOut) string {
			if !out.Removed {
				return "I couldn't find that on the list."
			}
			return "Removed " + out.Item.Text + " from the " + strings.ToLower(Title(out.Item.List)) + " list."
		},
		Examples: []string{
			"remove the old sofa from the to-do list",
			"take the library books off my to-do list",
			"delete the parcel from my to-do list",
			"cross off the dry cleaning from my to-do list",
			"remove the car tax from the to-do list",
			"take renew the passport off my to-do list",
			"delete book the flights from my to-do list",
			"cross off fix the gate from the to-do list",
			"remove the tent from the shopping list",
			"take the spare bulbs off the shopping list",
			"delete the batteries from the shopping list",
			"remove the paint from the shopping list",
			"take the birthday card off the shopping list",
			"remove the charcoal from the shopping list",
			"delete the foil from the shopping list",
			"take the olive oil off the shopping list",
			"remove the dish soap from the shopping list",
			"cross off the salt from the shopping list",
			"delete the flour from the shopping list",
			"take the washing up liquid off the shopping list",
		},
		Extract: extractRemove,
	}
}

// NewDone builds list.done. It is act: a ticked item is easy to untick from the
// Dashboard (or a later "put it back" direction), and the manager only sets the
// done flag rather than deleting the row. The spoken item is resolved through
// the shared name matcher; it never forces a guess, so an unknown or ambiguous
// item is a spoken refusal with no write.
func NewDone(m *Manager) tool.Tool[DoneIn, DoneOut] {
	return tool.Tool[DoneIn, DoneOut]{
		Name:        DoneName,
		Description: "Tick one item off the shopping list or the to-do list.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in DoneIn) (DoneOut, error) {
			key := ""
			if strings.TrimSpace(in.List) != "" {
				var ok bool
				key, ok = ParseKey(in.List)
				if !ok {
					return DoneOut{}, &tool.ToolError{Say: "I only have a shopping list and a to-do list."}
				}
			}
			item, near, err := m.DoneWithNear(ctx, key, in.Item)
			if err != nil {
				if errors.Is(err, ErrItemNotFound) {
					if near != "" {
						return DoneOut{}, &tool.ToolError{Say: namematch.NearQuestion(near, "list item")}
					}
					if key == "" {
						return DoneOut{}, &tool.ToolError{Say: "I couldn't find that on either list."}
					}
					return DoneOut{}, &tool.ToolError{Say: "I couldn't find that on the " + strings.ToLower(Title(key)) + " list."}
				}
				return DoneOut{}, toolError(err)
			}
			return DoneOut{Ticked: true, Item: item}, nil
		},
		Speak: func(out DoneOut) string {
			if !out.Ticked {
				return "I couldn't find that on the list."
			}
			return "Ticked " + out.Item.Text + " off the " + strings.ToLower(Title(out.Item.List)) + " list."
		},
		Examples: []string{
			"I got the milk",
			"I got the eggs",
			"I got the parcel",
			"I've got the bins",
			"I got the bread",
			"I finished the milk",
			"I finished the eggs",
			"I've finished the parcel",
			"I completed the bins",
			"I've done the laundry",
			"I did the tax return",
			"the eggs are done",
			"I'm done with the bins",
			"ticked off the eggs",
			"got the milk",
			"got the parcel",
			"tick the bins off my to-do list",
			"the parcel is done",
			"I finished the bread",
			"I've done the bins",
			"ticked off the parcel",
		},
		Extract: extractDone,
	}
}

// Register adds every list Tool.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.manager = NewManager(Options{
		DB:     deps.Store,
		Names:  deps.Names,
		Now:    deps.Clock,
		Logger: deps.Logger,
		Config: m.cfg,
	})
	if err := tool.Add(r, NewAdd(m.manager)); err != nil {
		return err
	}
	if err := tool.Add(r, NewRead(m.manager)); err != nil {
		return err
	}
	if err := tool.Add(r, NewRemove(m.manager)); err != nil {
		return err
	}
	return tool.Add(r, NewDone(m.manager))
}

// List is one fixed list and its open items.
type List struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Items []Item `json:"items"`
}

// SpeakRead renders one short spoken answer for one or both lists.
func SpeakRead(out ReadOut) string {
	if len(out.Lists) == 0 {
		return "I only have a shopping list and a to-do list."
	}
	parts := make([]string, 0, len(out.Lists))
	for _, list := range out.Lists {
		name := strings.ToLower(Title(list.Key))
		if len(list.Items) == 0 {
			parts = append(parts, "Your "+name+" list is empty.")
			continue
		}
		texts := make([]string, 0, len(list.Items))
		for _, item := range list.Items {
			texts = append(texts, item.Text)
		}
		parts = append(parts, "On your "+name+" list: "+strings.Join(texts, ", ")+".")
	}
	return strings.Join(parts, " ")
}

// listTitles are the one place a key becomes a spoken title.
var listTitles = map[string]string{
	KeyShopping: "Shopping",
	KeyTodo:     "To-do",
}

// Title returns the display title for one fixed key.
func Title(key string) string {
	if title, ok := listTitles[key]; ok {
		return title
	}
	return key
}

// ParseKey folds a spoken list name onto one of the fixed keys.
func ParseKey(raw string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimSuffix(name, " list")
	switch name {
	case "shopping", "shop", "shops", "groceries", "grocery":
		return KeyShopping, true
	case "to-do", "todo", "to do", "tasks", "task", "jobs":
		return KeyTodo, true
	default:
		return "", false
	}
}

// addPatterns capture (item, list) from the Fast path phrasing.
var addPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:can\s+you\s+)?(?:add|put)\s+(.+?)\s+(?:to|on)\s+(?:the\s+|my\s+)?(.+?)\s+list\s*[.!]?\s*(?:please)?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:add|put)\s+(.+?)\s+(?:to|on)\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?add\s+(.+?)\s+to\s+(?:the\s+|my\s+)?list\s+for\s+(?:the\s+)?shops?\s*[.!]?\s*$`),
}

func extractAdd(text string) (AddIn, bool) {
	for _, pattern := range addPatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		key, ok := ParseKey(match[2])
		if !ok {
			return AddIn{}, false
		}
		item := cleanItem(match[1])
		if item == "" {
			return AddIn{}, false
		}
		return AddIn{List: key, Item: item}, true
	}
	return AddIn{}, false
}

// readPatterns capture at most one list key; an empty key reads both.
var readPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?tell\s+me\s+what(?:'?s|\s+is)\s+on\s+(?:my\s+|the\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what(?:'?s|\s+is)\s+(?:on|left\s+on)\s+(?:my\s+|the\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:read|show|tell)\s+(?:me\s+)?(?:my\s+|the\s+)?(?:out\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?read\s+out\s+my\s+(shopping|to[-\s]?do|todo)\s+list\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?show\s+me\s+(?:my\s+|the\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what\s+do\s+i\s+(?:need|still\s+need)\s+to\s+buy\s*[?]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?what'?s\s+on\s+the\s+list\s+for\s+shopping\s*[?]?\s*$`),
}

func extractRead(text string) (ReadIn, bool) {
	for _, pattern := range readPatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
			key, ok := ParseKey(match[1])
			if !ok {
				return ReadIn{}, false
			}
			return ReadIn{List: key}, true
		}
		// "What do I need to buy" and "the list for shopping" both name the
		// shopping list without saying the word "shopping list".
		if strings.Contains(strings.ToLower(text), "buy") || strings.Contains(strings.ToLower(text), "shopping") {
			return ReadIn{List: KeyShopping}, true
		}
		return ReadIn{}, true
	}
	return ReadIn{}, false
}

// removePatterns capture (item, list) from the Fast path phrasing.
var removePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:remove|delete|take|cross)\s+(?:off\s+)?(.+?)\s+(?:from|off)\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[.!]?\s*(?:please)?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:remove|delete|take|cross)\s+(.+?)\s+(?:from|off)\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?cross\s+off\s+(.+?)\s+(?:on|from)\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[.!]?\s*(?:please)?\s*$`),
}

func extractRemove(text string) (RemoveIn, bool) {
	for _, pattern := range removePatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		key, ok := ParseKey(match[2])
		if !ok {
			return RemoveIn{}, false
		}
		item := cleanItem(match[1])
		if item == "" {
			return RemoveIn{}, false
		}
		return RemoveIn{List: key, Item: item}, true
	}
	return RemoveIn{}, false
}

// donePatterns capture (item, list) from the Fast path phrasing. The shapes
// that name no list ("I got the X", "the X is done", "ticked off the X") let the
// handler search both fixed lists; only the "tick ... off the <list> list" and
// "mark ... done on the <list> list" shapes carry a list. "cross" is
// deliberately absent: that phrasing belongs to list.remove.
var donePatterns = []*regexp.Regexp{
	// tick ... off the <list> list
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?tick\s+off\s+(.+?)\s+(?:on|from|off)\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?tick\s+(.+?)\s+off\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[.!]?\s*$`),
	// ticked off <item> (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?ticked\s+off\s+(.+?)\s*[.!]?\s*$`),
	// I got / finished / completed / did the <item> (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?i\s+(?:got|finished|completed|did)\s+(?:the\s+|my\s+)?(.+?)\s*[.!]?\s*$`),
	// I've got / finished / done the <item> (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?i'?ve\s+(?:got|finished|done)\s+(?:the\s+|my\s+)?(.+?)\s*[.!]?\s*$`),
	// got the <item> (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?got\s+(?:the\s+|my\s+)?(.+?)\s*[.!]?\s*$`),
	// the <item> is/are done (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?the\s+(.+?)\s+(?:is|are)\s+done\s*[.!]?\s*$`),
	// <item> is ticked off (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?(.+?)\s+is\s+ticked\s+off\s*[.!]?\s*$`),
	// I'm done with the <item> (no list)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?i'?m\s+done\s+with\s+(?:the\s+|my\s+)?(.+?)\s*[.!]?\s*$`),
	// mark <item> (as) done on/from/off the <list> list (issue Extract coverage)
	regexp.MustCompile(`(?i)^\s*(?:please\s+)?mark\s+(?:the\s+|my\s+)?(.+?)\s+(?:as\s+)?done\s+(?:on|from|off)\s+(?:the\s+|my\s+)?(shopping|to[-\s]?do|todo)\s+list\s*[.!]?\s*$`),
}

func extractDone(text string) (DoneIn, bool) {
	for _, pattern := range donePatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		item := cleanItem(match[1])
		if item == "" {
			return DoneIn{}, false
		}
		// Patterns with a second capture group carry an explicit list key.
		if len(match) > 2 && strings.TrimSpace(match[2]) != "" {
			key, ok := ParseKey(match[2])
			if !ok {
				return DoneIn{}, false
			}
			return DoneIn{List: key, Item: item}, true
		}
		// Every other shape names no list, so the handler searches both.
		return DoneIn{List: "", Item: item}, true
	}
	return DoneIn{}, false
}

// cleanItem strips the leading article the phrasing carries but the stored
// item does not, so "take the milk off" matches the row "milk".
func cleanItem(raw string) string {
	item := strings.TrimSpace(raw)
	for _, lead := range []string{"the ", "my ", "a ", "an ", "some "} {
		if strings.HasPrefix(strings.ToLower(item), lead) {
			item = strings.TrimSpace(item[len(lead):])
			break
		}
	}
	return strings.Trim(item, `"'`)
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
