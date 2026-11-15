// Package audiocontrol implements the audio.volume_set and audio.stop Tools.
package audiocontrol

import (
	"context"
	"fmt"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	VolumeName = "audio.volume_set"
	StopName   = "audio.stop"
)

// StepPercent is the change a bare "louder" makes.
const StepPercent = 10

// Controls is the small audio seam the Tools drive. The Pulse sink implements
// it in production; tests use a fake.
type Controls = tool.AudioControls

// VolumeIn takes an absolute percent (zero makes it silent), a mute flag, a
// relative delta, or neither, which means one step up. Percent and Mute are
// pointers so an explicit zero or false is not confused with an absent
// argument.
type VolumeIn struct {
	Percent *int  `json:"percent,omitempty"`
	Delta   int   `json:"delta,omitempty"`
	Mute    *bool `json:"mute,omitempty"`
}

// VolumeOut echoes the requested change for the log and the reply.
type VolumeOut struct {
	Percent int  `json:"percent,omitempty"`
	Delta   int  `json:"delta,omitempty"`
	Muted   bool `json:"muted,omitempty"`
	Unmuted bool `json:"unmuted,omitempty"`
}

// StopIn has no arguments.
type StopIn struct{}

// StopOut reports that playback was stopped.
type StopOut struct {
	Stopped bool `json:"stopped"`
}

// NewVolume builds audio.volume_set with the default step.
func NewVolume(controls Controls) tool.Tool[VolumeIn, VolumeOut] {
	return NewVolumeWithStep(controls, StepPercent)
}

// NewVolumeWithStep builds audio.volume_set with a configured "louder" step.
func NewVolumeWithStep(controls Controls, step int) tool.Tool[VolumeIn, VolumeOut] {
	return tool.Tool[VolumeIn, VolumeOut]{
		Name:        VolumeName,
		Description: "Set the speaker volume to a percent, change it by a delta, mute or unmute the speaker, or with no arguments turn it up one step.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in VolumeIn) (VolumeOut, error) {
			if controls == nil {
				return VolumeOut{}, &tool.ToolError{Say: "Audio control is not available."}
			}
			if in.Percent != nil {
				if *in.Percent < 0 || *in.Percent > 100 {
					return VolumeOut{}, &tool.ToolError{Say: "I can only set the volume between 0 and 100."}
				}
				if err := controls.SetVolume(ctx, *in.Percent); err != nil {
					return VolumeOut{}, err
				}
				// A non-zero level clears a stale mute, as the Dashboard slider
				// does; zero only moves the level and leaves the flag alone.
				if *in.Percent > 0 {
					if err := controls.SetMute(ctx, false); err != nil {
						return VolumeOut{}, err
					}
				}
				return VolumeOut{Percent: *in.Percent, Muted: *in.Percent == 0}, nil
			}
			if in.Mute != nil {
				return setMute(ctx, controls, step, *in.Mute)
			}
			delta := in.Delta
			if delta == 0 {
				delta = step
			}
			if err := controls.AdjustVolume(ctx, delta); err != nil {
				return VolumeOut{}, err
			}
			return VolumeOut{Delta: delta}, nil
		},
		Speak: func(out VolumeOut) string {
			switch {
			case out.Unmuted:
				return "Unmuted."
			case out.Delta > 0:
				return "Volume up."
			case out.Delta < 0:
				return "Volume down."
			case out.Muted:
				return "Muted."
			default:
				return fmt.Sprintf("Volume set to %d percent.", out.Percent)
			}
		},
		Examples: []string{
			"turn the volume to 40",
			"set the volume to 30 percent",
			"turn the volume to zero",
			"turn it up",
			"louder",
			"turn the volume down",
			"quieter",
			"mute",
			"mute the sound",
			"mute the volume",
			"unmute",
		},
		Extract: func(text string) (VolumeIn, bool) { return extractVolume(text, step) },
	}
}

// setMute drives the sink mute flag and keeps the level, so an unmute restores
// what was playing. An unmute from a level of zero applies one step, because
// an earlier absolute zero left nothing to restore.
func setMute(ctx context.Context, controls Controls, step int, muted bool) (VolumeOut, error) {
	if muted {
		if err := controls.SetMute(ctx, true); err != nil {
			return VolumeOut{}, err
		}
		return VolumeOut{Muted: true}, nil
	}
	level, err := controls.Level(ctx)
	if err != nil {
		return VolumeOut{}, err
	}
	if err := controls.SetMute(ctx, false); err != nil {
		return VolumeOut{}, err
	}
	if level == 0 {
		if err := controls.AdjustVolume(ctx, step); err != nil {
			return VolumeOut{}, err
		}
	}
	return VolumeOut{Unmuted: true}, nil
}

// extractVolume reads an absolute percent, a direction, or mute from the
// utterance. Mute drives the sink mute flag and keeps the level; it never
// changes a Ring's own absolute volume settings or the Quiet-hours software
// gain.
func extractVolume(text string, step int) (VolumeIn, bool) {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "unmute"), strings.Contains(lower, "un-mute"):
		unmuted := false
		return VolumeIn{Mute: &unmuted}, true
	case strings.Contains(lower, "louder"), strings.Contains(lower, "turn it up"), strings.Contains(lower, "volume up"):
		return VolumeIn{Delta: step}, true
	case strings.Contains(lower, "quieter"), strings.Contains(lower, "turn it down"), strings.Contains(lower, "volume down"):
		return VolumeIn{Delta: -step}, true
	case strings.Contains(lower, "mute"):
		muted := true
		return VolumeIn{Mute: &muted}, true
	}
	n, ok := parse.Number(text)
	if !ok || n < 0 || n > 100 {
		return VolumeIn{}, false
	}
	return VolumeIn{Percent: &n}, true
}

// NewStop builds audio.stop.
func NewStop(controls Controls) tool.Tool[StopIn, StopOut] {
	return tool.Tool[StopIn, StopOut]{
		Name:        StopName,
		Description: "Stop the current playback.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ StopIn) (StopOut, error) {
			if controls == nil {
				return StopOut{}, &tool.ToolError{Say: "Audio control is not available."}
			}
			if err := controls.Stop(ctx); err != nil {
				return StopOut{}, err
			}
			return StopOut{Stopped: true}, nil
		},
		Speak: func(StopOut) string { return "Stopped." },
		Examples: []string{
			"stop",
			"stop talking",
			"be quiet",
		},
	}
}

// Register adds both audio Tools with the default step.
func Register(r *tool.Registry, controls Controls) error {
	return RegisterWithStep(r, controls, StepPercent)
}

// RegisterWithStep adds both audio Tools with a configured "louder" step.
func RegisterWithStep(r *tool.Registry, controls Controls, step int) error {
	if err := tool.Add(r, NewVolumeWithStep(controls, step)); err != nil {
		return err
	}
	return tool.Add(r, NewStop(controls))
}
