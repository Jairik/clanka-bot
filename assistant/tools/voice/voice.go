// Package voice implements the voice.set, voice.list, voice.current and
// voice.rate Tools over the one persistent voice library and settings service.
// It was scaffolded with `make new-tool name=voice.set`; the other three Tools
// are hand-added to the same module per docs/agents/adding-a-tool.md.
package voice

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"gopkg.in/yaml.v3"
)

// Canonical Tool names.
const (
	SetName     = "voice.set"
	ListName    = "voice.list"
	CurrentName = "voice.current"
	RateName    = "voice.rate"
)

// Config is the tools.voice section.
type Config struct {
	// Step is the rate change one "faster"/"slower" utterance makes, as a
	// multiplier fraction.
	Step float64 `yaml:"step"`
}

// DefaultConfig is one tenth of normal speed per step.
func DefaultConfig() Config { return Config{Step: 0.1} }

func (c Config) validate() error {
	if math.IsNaN(c.Step) || math.IsInf(c.Step, 0) || c.Step <= 0 || c.Step > 1 {
		return errors.New("step must be a number in (0, 1]")
	}
	return nil
}

// Module is the voice Tool domain.
type Module struct {
	cfg Config
}

// NewModule builds the voice module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "voice" }

// Configure decodes tools.voice over the code defaults.
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

// Register adds all four voice Tools. Nil seams are allowed so the conformance
// suite can inspect metadata without a database; a call then reports that the
// library is unavailable instead of panicking.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := tool.Add(r, m.newSet(deps)); err != nil {
		return err
	}
	if err := tool.Add(r, m.newList(deps)); err != nil {
		return err
	}
	if err := tool.Add(r, m.newCurrent(deps)); err != nil {
		return err
	}
	return tool.Add(r, m.newRate(deps))
}

// --- voice.set -------------------------------------------------------------

// SetIn is the voice.set argument.
type SetIn struct {
	Name string `json:"name"`
}

// SetOut echoes the selected library entry and the current rate.
type SetOut struct {
	Voice   string  `json:"voice"`
	Builtin bool    `json:"builtin"`
	Rate    float64 `json:"rate"`
}

func (m *Module) newSet(deps tool.Deps) tool.Tool[SetIn, SetOut] {
	return tool.Tool[SetIn, SetOut]{
		Name:        SetName,
		Description: "Switch the Assistant to a library voice. Voice is a persistent setting, not a per-request choice.",
		Permission:  tool.PermissionAct,
		// Voice names, including custom state file names, stay on the Pi. The
		// Fast path still handles the spoken request; the Agent never sees it.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(ctx context.Context, in SetIn) (SetOut, error) {
			service := deps.Voices
			if service == nil {
				return SetOut{}, &tool.ToolError{Say: "The voice library isn't available."}
			}
			spoken := strings.TrimSpace(in.Name)
			if spoken == "" {
				return SetOut{}, &tool.ToolError{Say: "Which voice should I use?"}
			}
			name, ok := resolveVoiceName(ctx, deps, spoken)
			if !ok {
				return SetOut{}, &tool.ToolError{Say: fmt.Sprintf("I couldn't find a voice called %s.", spoken)}
			}
			voice, err := service.SetActive(ctx, name)
			if err != nil {
				return SetOut{}, voiceFailure(voice, err)
			}
			return SetOut{Voice: voice.Name, Builtin: voice.Builtin, Rate: service.Rate()}, nil
		},
		Speak: func(out SetOut) string {
			return fmt.Sprintf("Okay, I'll use the %s voice.", out.Voice)
		},
		Examples: []string{
			"use the jarvis voice",
			"switch to the alba voice",
			"change your voice to jarvis",
			"set your voice to the alba voice",
			"can you talk in the jarvis voice",
			"please use the alba voice",
		},
		Extract: extractSet,
	}
}

// --- voice.list ------------------------------------------------------------

// ListIn has no arguments.
type ListIn struct{}

// ListItem is one voice in the library list.
type ListItem struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Builtin bool   `json:"builtin"`
}

// ListOut is every library entry plus the active voice and rate.
type ListOut struct {
	Voices  []ListItem `json:"voices"`
	Current string     `json:"current"`
	Rate    float64    `json:"rate"`
}

func (m *Module) newList(deps tool.Deps) tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the voices in the library, including custom Pocket TTS voice states and their validation status.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, ListIn) (ListOut, error) {
			service := deps.Voices
			if service == nil {
				return ListOut{}, &tool.ToolError{Say: "The voice library isn't available."}
			}
			entries := service.List()
			out := ListOut{Voices: make([]ListItem, 0, len(entries)), Rate: service.Rate()}
			if active, ok := effectiveVoice(service); ok {
				out.Current = active.Name
			}
			for _, entry := range entries {
				out.Voices = append(out.Voices, ListItem{Name: entry.Name, Status: entry.Status(), Builtin: entry.Builtin})
			}
			return out, nil
		},
		Speak: func(out ListOut) string {
			if len(out.Voices) == 0 {
				return "I don't have any voices in the library yet."
			}
			ready := make([]string, 0, len(out.Voices))
			stale := make([]string, 0, len(out.Voices))
			for _, entry := range out.Voices {
				switch entry.Status {
				case "ready", "builtin":
					ready = append(ready, entry.Name)
				default:
					stale = append(stale, entry.Name)
				}
			}
			var parts []string
			if len(ready) > 0 {
				parts = append(parts, "I have "+joinNames(ready)+".")
			}
			if len(stale) > 0 {
				verb, pronoun := "need", "them"
				if len(stale) == 1 {
					verb, pronoun = "needs", "it"
				}
				parts = append(parts, joinNames(stale)+" "+verb+" re-exporting before I can use "+pronoun+".")
			}
			return strings.Join(parts, " ")
		},
		Examples: []string{
			"list every voice in the library",
			"enumerate the installed voices",
			"read out the voice catalogue",
			"show me the voice library contents",
			"what voice choices are installed",
			"give me the full voice list",
		},
	}
}

// --- voice.current ---------------------------------------------------------

// CurrentIn has no arguments.
type CurrentIn struct{}

// CurrentOut is the effective voice and rate.
type CurrentOut struct {
	Voice   string  `json:"voice"`
	Builtin bool    `json:"builtin"`
	Status  string  `json:"status"`
	Rate    float64 `json:"rate"`
}

func (m *Module) newCurrent(deps tool.Deps) tool.Tool[CurrentIn, CurrentOut] {
	return tool.Tool[CurrentIn, CurrentOut]{
		Name:        CurrentName,
		Description: "Say which voice is speaking and how fast it is set.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, CurrentIn) (CurrentOut, error) {
			service := deps.Voices
			if service == nil {
				return CurrentOut{}, &tool.ToolError{Say: "The voice library isn't available."}
			}
			active, ok := effectiveVoice(service)
			if !ok {
				return CurrentOut{}, &tool.ToolError{Say: "I can't tell which voice is selected."}
			}
			return CurrentOut{Voice: active.Name, Builtin: active.Builtin, Status: active.Status(), Rate: service.Rate()}, nil
		},
		Speak: func(out CurrentOut) string {
			return fmt.Sprintf("I'm using the %s voice at %s.", out.Voice, rateLabel(out.Rate))
		},
		Examples: []string{
			"which voice is speaking right now",
			"tell me the voice you are using",
			"what is your current voice setting",
			"which voice have you selected",
			"identify the voice I am hearing",
			"what voice is active at the moment",
		},
	}
}

// --- voice.rate ------------------------------------------------------------

// RateIn is the voice.rate argument. Delta is in steps: -1 slower, +1 faster;
// a bare call (a Control phrase or no arguments) means one step faster. Normal
// restores 1.0 explicitly, so the two directions can share one argument-less
// Control phrase without a reset ambiguity.
type RateIn struct {
	Delta  int  `json:"delta,omitempty"`
	Normal bool `json:"normal,omitempty"`
}

// RateOut is the stored multiplier and the step that produced it.
type RateOut struct {
	Rate  float64 `json:"rate"`
	Delta int     `json:"delta"`
	Reset bool    `json:"reset,omitempty"`
}

func (m *Module) newRate(deps tool.Deps) tool.Tool[RateIn, RateOut] {
	return tool.Tool[RateIn, RateOut]{
		Name:        RateName,
		Description: "Change how fast the Assistant speaks. A positive delta is faster, a negative delta is slower, normal restores normal speed, and no arguments makes one step faster.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(ctx context.Context, in RateIn) (RateOut, error) {
			service := deps.Voices
			if service == nil {
				return RateOut{}, &tool.ToolError{Say: "The voice library isn't available."}
			}
			target := 1.0
			delta := in.Delta
			switch {
			case in.Normal:
				delta = 0
			case delta == 0:
				// The Control phrase ("speak faster") carries no arguments;
				// its direction is the default one step up.
				delta = 1
				target = service.Rate() + m.cfg.Step
			default:
				target = service.Rate() + float64(delta)*m.cfg.Step
			}
			rate, err := service.SetRate(ctx, target)
			if err != nil {
				return RateOut{}, &tool.ToolError{Say: "I couldn't change the speaking rate."}
			}
			return RateOut{Rate: rate, Delta: delta, Reset: in.Normal}, nil
		},
		Speak: func(out RateOut) string {
			switch {
			case out.Reset:
				return "Okay, back to normal speed."
			case out.Delta > 0:
				return "Okay, I'll speak a little faster."
			case out.Delta < 0:
				return "Okay, I'll speak a little slower."
			default:
				return "Okay, back to normal speed."
			}
		},
		Examples: []string{
			"speak faster",
			"talk more slowly",
			"speed up your speech",
			"slow down your speaking",
			"return to normal speed",
			"make your speech quicker",
			"slow down your voice",
			"speak slower",
			"can you speak faster",
		},
		Extract: extractRate,
	}
}

// --- helpers ---------------------------------------------------------------

// effectiveVoice reports the voice that will actually speak. An unusable
// persisted entry falls back to the default built-in, matching the speaker, so
// "which voice is this" cannot claim a stale state is in use.
func effectiveVoice(service *voicelib.Service) (voicelib.Voice, bool) {
	active, ok := service.Active()
	if ok && active.Ready() {
		return active, true
	}
	return service.Get(service.DefaultName())
}

// resolveVoiceName is the one shared name matcher lookup. Exact and normalised
// spellings win without an embed call; a near miss needs the live threshold.
func resolveVoiceName(ctx context.Context, deps tool.Deps, spoken string) (string, bool) {
	service := deps.Voices
	if service == nil {
		return "", false
	}
	candidates := service.Candidates()
	if deps.Names != nil {
		result := deps.Names.Match(ctx, spoken, candidates)
		return result.Name, result.Matched()
	}
	// Conformance and tests may omit the embedder; exact spelling is still a
	// legitimate match and cannot guess.
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(spoken), candidate) {
			return candidate, true
		}
	}
	return "", false
}

// voiceFailure maps a library rejection to the one line the user hears.
func voiceFailure(voice voicelib.Voice, err error) error {
	name := voice.Name
	if name == "" {
		name = "That voice"
	}
	switch {
	case errors.Is(err, voicelib.ErrStaleVoice):
		return &tool.ToolError{Say: fmt.Sprintf("%s was exported for a different model revision. Re-export it on the desktop first.", name)}
	case errors.Is(err, voicelib.ErrNotValidated):
		return &tool.ToolError{Say: fmt.Sprintf("%s hasn't passed the model check yet. Try again after a library rescan.", name)}
	case errors.Is(err, voicelib.ErrUnknownVoice):
		return &tool.ToolError{Say: fmt.Sprintf("I couldn't find a voice called %s.", name)}
	default:
		return &tool.ToolError{Say: "I couldn't switch voices."}
	}
}

// extractSet pulls a voice name out of a natural request. It deliberately
// returns false for anything that is not recognisably a voice change, so the
// request can fall through instead of switching to a random phrase.
func extractSet(text string) (SetIn, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return SetIn{}, false
	}
	trimmed := lower
	changed := false
	prefixes := []string{
		"can you ", "could you ", "would you ", "please ", "i want you to ",
		"i'd like you to ", "i would like you to ",
		"change your voice to the ", "change your voice to ",
		"change your speaking voice to the ", "change your speaking voice to ",
		"set your voice to the ", "set your voice to ",
		"set the voice to the ", "set the voice to ",
		"switch your voice to the ", "switch your voice to ",
		"switch to the ", "switch to ", "change to the ", "change to ",
		"talk in the ", "talk in ", "speak in the ", "speak in ",
		"use the ", "use ", "make your voice the ", "make your voice ",
		"sound like ",
	}
	// Polite lead-ins stack ("can you please use..."), so strip repeatedly
	// until no known prefix matches.
	for stripped := true; stripped; {
		stripped = false
		for _, prefix := range prefixes {
			if rest, ok := strings.CutPrefix(trimmed, prefix); ok {
				trimmed = strings.TrimSpace(rest)
				changed = true
				stripped = true
				break
			}
		}
	}
	suffixes := []string{
		" voice please", " voice for me", " voice now", " voice",
		" from now on", " as your voice", " please", " for me", " now",
	}
	for _, suffix := range suffixes {
		if rest, ok := strings.CutSuffix(trimmed, suffix); ok {
			trimmed = strings.TrimSpace(rest)
		}
	}
	if trimmed == "" {
		return SetIn{}, false
	}
	// A request without a change verb must at least talk about a voice.
	if !changed && !strings.Contains(lower, "voice") {
		return SetIn{}, false
	}
	if strings.ContainsAny(trimmed, "?!.") {
		return SetIn{}, false
	}
	// "alba's voice" is the same library name as "alba".
	trimmed = strings.TrimSuffix(trimmed, "'s")
	if len(strings.Fields(trimmed)) > 4 {
		return SetIn{}, false
	}
	return SetIn{Name: trimmed}, true
}

// extractRate reads a faster/slower/normal direction. It intentionally does
// not match volume or music controls: "turn it up" is a different Tool.
func extractRate(text string) (RateIn, bool) {
	lower := strings.ToLower(text)
	// "play faster" and "slow down the music" belong to playback, not speech.
	for _, other := range []string{"music", "song", "playlist", "volume", "fan"} {
		if strings.Contains(lower, other) {
			return RateIn{}, false
		}
	}
	switch {
	case strings.Contains(lower, "normal speed"),
		strings.Contains(lower, "regular speed"),
		strings.Contains(lower, "normal speaking rate"),
		strings.Contains(lower, "normal rate"),
		strings.Contains(lower, "speak normally"),
		strings.Contains(lower, "talk normally"),
		strings.Contains(lower, "normal voice"),
		strings.Contains(lower, "back to normal"):
		return RateIn{Normal: true}, true
	case strings.Contains(lower, "faster"),
		strings.Contains(lower, "speed up"),
		strings.Contains(lower, "quickly"),
		strings.Contains(lower, "quicker"),
		strings.Contains(lower, "increase") && (strings.Contains(lower, "speed") || strings.Contains(lower, "rate")):
		return RateIn{Delta: 1}, true
	case strings.Contains(lower, "slower"),
		strings.Contains(lower, "slowly"),
		strings.Contains(lower, "slow down"),
		strings.Contains(lower, "slow your speaking"),
		strings.Contains(lower, "slow your speech"):
		return RateIn{Delta: -1}, true
	default:
		return RateIn{}, false
	}
}

// rateLabel describes the multiplier without pretending it is words per minute.
func rateLabel(rate float64) string {
	percent := int(math.Round((rate - 1) * 100))
	switch {
	case percent == 0:
		return "normal speed"
	case percent > 0:
		return fmt.Sprintf("%d percent faster", percent)
	default:
		return fmt.Sprintf("%d percent slower", -percent)
	}
}

// joinNames renders a short spoken list.
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
