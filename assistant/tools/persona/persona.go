// Package persona implements the persona.switch, persona.list and
// persona.current Tools over the one persistent Persona library. The module was
// scaffolded with `make new-tool name=persona.switch`; the other two Tools are
// hand-added to the same module per docs/agents/adding-a-tool.md.
package persona

import (
	"context"
	"errors"
	"fmt"
	"strings"

	personacore "github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	SwitchName  = "persona.switch"
	ListName    = "persona.list"
	CurrentName = "persona.current"
)

// Module is the Persona Tool domain. main binds the one library after the
// module set is built, because the service needs the store and voice library.
type Module struct {
	service *personacore.Service
}

// NewModule builds the Persona module without a bound library. The conformance
// suite inspects metadata with this value; a call reports that the library is
// unavailable instead of panicking.
func NewModule() *Module { return &Module{} }

// Bind installs the one Persona library shared with the Dashboard.
func (m *Module) Bind(service *personacore.Service) { m.service = service }

// Name is the module name. It has no tools.persona settings section.
func (*Module) Name() string { return "persona" }

// Register adds the three Persona Tools.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := tool.Add(r, m.newSwitch(deps)); err != nil {
		return err
	}
	if err := tool.Add(r, m.newList()); err != nil {
		return err
	}
	return tool.Add(r, m.newCurrent())
}

// --- persona.switch --------------------------------------------------------

// SwitchIn is the persona.switch argument.
type SwitchIn struct {
	Name string `json:"name"`
}

// SwitchOut echoes the applied bundle.
type SwitchOut struct {
	Persona string `json:"persona"`
	Voice   string `json:"voice"`
}

func (m *Module) newSwitch(deps tool.Deps) tool.Tool[SwitchIn, SwitchOut] {
	return tool.Tool[SwitchIn, SwitchOut]{
		Name:        SwitchName,
		Description: "Switch to a stored Persona, applying its voice, personality prompt, Filler and Unavailable wording together.",
		Permission:  tool.PermissionAct,
		// Persona names and prompts stay on the Pi. The Fast path still handles
		// the spoken request; the Agent never sees it.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(ctx context.Context, in SwitchIn) (SwitchOut, error) {
			service := m.service
			if service == nil {
				return SwitchOut{}, &tool.ToolError{Say: "The persona library isn't available."}
			}
			spoken := strings.TrimSpace(in.Name)
			if spoken == "" {
				return SwitchOut{}, &tool.ToolError{Say: "Which persona should I use?"}
			}
			name, ok := resolvePersonaName(ctx, deps, service, spoken)
			if !ok {
				return SwitchOut{}, &tool.ToolError{Say: fmt.Sprintf("I couldn't find a persona called %s.", spoken)}
			}
			p, err := service.Select(ctx, name)
			if err != nil {
				return SwitchOut{}, personaFailure(name, err)
			}
			return SwitchOut{Persona: p.Name, Voice: p.Voice}, nil
		},
		Speak: func(out SwitchOut) string {
			return fmt.Sprintf("Okay, switching to the %s persona.", out.Persona)
		},
		Examples: []string{
			"switch to the glados persona",
			"switch to glados persona",
			"use the glados persona",
			"activate the glados persona",
			"change to the glados persona",
			"become glados",
			"be glados",
			"adopt the glados persona",
			"put on the glados persona",
			"switch to the villain persona",
			"use the villain persona",
			"activate the butler persona",
			"change your persona to glados",
			"set your persona to glados",
			"can you switch to the glados persona",
			"please use the glados persona",
			"switch personas to glados",
			"I want the glados persona",
			"make yourself glados",
			"go into the glados persona",
		},
		Extract: extractSwitch,
	}
}

// --- persona.list ----------------------------------------------------------

// ListIn has no arguments.
type ListIn struct{}

// ListItem is one Persona in the library list.
type ListItem struct {
	Name   string `json:"name"`
	Voice  string `json:"voice"`
	Active bool   `json:"active"`
}

// ListOut is every Persona plus the active name.
type ListOut struct {
	Personas []ListItem `json:"personas"`
	Active   string     `json:"active"`
}

func (m *Module) newList() tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the stored Personas and which one is active.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, ListIn) (ListOut, error) {
			service := m.service
			if service == nil {
				return ListOut{}, &tool.ToolError{Say: "The persona library isn't available."}
			}
			entries := service.List()
			out := ListOut{Personas: make([]ListItem, 0, len(entries))}
			if current, ok := service.Current(); ok {
				out.Active = current.Persona.Name
			}
			for _, entry := range entries {
				out.Personas = append(out.Personas, ListItem{
					Name:   entry.Name,
					Voice:  entry.Voice,
					Active: strings.EqualFold(entry.Name, out.Active),
				})
			}
			return out, nil
		},
		Speak: func(out ListOut) string {
			if len(out.Personas) == 0 {
				return "I don't have any personas yet."
			}
			names := make([]string, 0, len(out.Personas))
			for _, entry := range out.Personas {
				names = append(names, entry.Name)
			}
			text := "I have " + joinNames(names) + "."
			if out.Active != "" {
				text += fmt.Sprintf(" %s is active.", out.Active)
			} else {
				text += " No persona is selected."
			}
			return text
		},
		Examples: []string{
			"list the personas",
			"show me the persona library",
			"give me the full persona list",
			"enumerate the personas",
			"read out the persona catalogue",
			"list every persona",
			"show me all personas",
			"what personas are defined",
			"tell me the personas you know",
			"what's in the persona library",
			"list my personas",
			"show the available personas",
			"what personas are saved",
			"read me the persona names",
			"give me the list of personas",
		},
	}
}

// --- persona.current -------------------------------------------------------

// CurrentIn has no arguments.
type CurrentIn struct{}

// CurrentOut is the active Persona and an honest comparison with the settings
// that actually apply. A manual override or an edit after selection sets the
// corresponding Differs flag; the caller must not claim the exact bundle then.
type CurrentOut struct {
	Persona              string `json:"persona"`
	Voice                string `json:"voice"`
	EffectiveVoice       string `json:"effective_voice"`
	EffectiveFiller      string `json:"effective_filler"`
	EffectiveUnavailable string `json:"effective_unavailable"`
	Filler               string `json:"filler"`
	Unavailable          string `json:"unavailable"`
	VoiceDiffers         bool   `json:"voice_differs"`
	FillerDiffers        bool   `json:"filler_differs"`
	UnavailableDiffers   bool   `json:"unavailable_differs"`
	Exact                bool   `json:"exact"`
}

func (m *Module) newCurrent() tool.Tool[CurrentIn, CurrentOut] {
	return tool.Tool[CurrentIn, CurrentOut]{
		Name:        CurrentName,
		Description: "Say which Persona is active, including whether its voice or wording has been overridden since selection.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, CurrentIn) (CurrentOut, error) {
			service := m.service
			if service == nil {
				return CurrentOut{}, &tool.ToolError{Say: "The persona library isn't available."}
			}
			current, ok := service.Current()
			if !ok {
				return CurrentOut{}, &tool.ToolError{Say: "No persona is selected."}
			}
			return CurrentOut{
				Persona:              current.Persona.Name,
				Voice:                current.Persona.Voice,
				EffectiveVoice:       current.EffectiveVoice,
				EffectiveFiller:      current.EffectiveFiller,
				EffectiveUnavailable: current.EffectiveUnavailable,
				Filler:               current.Persona.Filler,
				Unavailable:          current.Persona.Unavailable,
				VoiceDiffers:         current.VoiceDiffers,
				FillerDiffers:        current.FillerDiffers,
				UnavailableDiffers:   current.UnavailableDiffers,
				Exact:                current.Exact,
			}, nil
		},
		Speak: speakCurrent,
		Examples: []string{
			"are you in character",
			"which character are you",
			"tell me your current character",
			"which personality are you running",
			"is a character selected",
			"do you have a character active",
			"what personality is active",
			"are you playing a character",
			"who are you right now in character",
			"does a character define your voice",
			"what's your character setting",
			"show me the active character",
			"are you using a character",
			"tell me which personality is loaded",
			"what character is speaking",
			"current character please",
			"what persona is active",
		},
	}
}

// speakCurrent states the exact bundle only when no difference exists; each
// mismatch is named so a manual voice/Filler override is not hidden.
func speakCurrent(out CurrentOut) string {
	details := make([]string, 0, 3)
	if out.VoiceDiffers {
		if out.EffectiveVoice != "" {
			details = append(details, fmt.Sprintf("the voice is set to %s instead of %s", out.EffectiveVoice, out.Voice))
		} else {
			details = append(details, "the persona's voice is not available")
		}
	}
	if out.FillerDiffers {
		details = append(details, "the Filler wording is different")
	}
	if out.UnavailableDiffers {
		details = append(details, "the Unavailable message is different")
	}
	if len(details) == 0 {
		return fmt.Sprintf("I'm using the %s persona.", out.Persona)
	}
	return fmt.Sprintf("I'm using the %s persona, but %s.", out.Persona, joinClauses(details))
}

// --- helpers ---------------------------------------------------------------

// resolvePersonaName uses the one shared name matcher. Exact and normalised
// spellings work without an embedding call; a near miss needs the live
// threshold and never becomes a forced guess.
func resolvePersonaName(ctx context.Context, deps tool.Deps, service *personacore.Service, spoken string) (string, bool) {
	candidates := service.Names()
	if deps.Names != nil {
		result := deps.Names.Match(ctx, spoken, candidates)
		return result.Name, result.Matched()
	}
	for _, candidate := range candidates {
		if strings.EqualFold(spoken, candidate) {
			return candidate, true
		}
	}
	return "", false
}

// personaFailure maps a library rejection to one user-safe spoken line.
func personaFailure(name string, err error) error {
	if message := personacore.Message(err); message != "" {
		return &tool.ToolError{Say: message}
	}
	switch {
	case errors.Is(err, personacore.ErrUnknownPersona):
		return &tool.ToolError{Say: fmt.Sprintf("I couldn't find a persona called %s.", name)}
	case errors.Is(err, personacore.ErrVoiceUnavailable):
		return &tool.ToolError{Say: "That persona's voice isn't ready, so I left everything as it was."}
	default:
		return &tool.ToolError{Say: "I couldn't switch personas."}
	}
}

// extractSwitch pulls a Persona name out of a natural request. It deliberately
// rejects voice changes so a near-miss cannot steal a voice.set request, and
// only accepts explicit persona/character constructions.
func extractSwitch(text string) (SwitchIn, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || strings.Contains(lower, "voice") {
		return SwitchIn{}, false
	}
	trimmed := lower
	changed := false
	prefixes := []string{
		"can you ", "could you ", "would you ", "please ", "i want you to ",
		"i'd like you to ", "i would like you to ", "i want the ",
		"change your persona to the ", "change your persona to ",
		"set your persona to the ", "set your persona to ",
		"switch your persona to the ", "switch your persona to ",
		"switch personas to the ", "switch personas to ",
		"switch to the ", "switch to ", "change to the ", "change to ",
		"use the ", "use ", "activate the ", "activate ",
		"adopt the ", "adopt ", "put on the ", "put on ",
		"go into the ", "go into ", "become ", "be ",
		"make yourself the ", "make yourself ",
	}
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
		" persona please", " persona for me", " persona now", " persona",
		" character please", " character", " please", " for me", " now",
	}
	for _, suffix := range suffixes {
		if rest, ok := strings.CutSuffix(trimmed, suffix); ok {
			trimmed = strings.TrimSpace(rest)
		}
	}
	if trimmed == "" || !changed {
		return SwitchIn{}, false
	}
	// A request that still looks like a sentence is not a persona name.
	if strings.ContainsAny(trimmed, "?!.") || len(strings.Fields(trimmed)) > 4 {
		return SwitchIn{}, false
	}
	return SwitchIn{Name: trimmed}, true
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

// joinClauses renders a short list of mismatch clauses.
func joinClauses(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}
