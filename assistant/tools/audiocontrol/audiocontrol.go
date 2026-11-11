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
type Controls interface {
	SetVolume(ctx context.Context, percent int) error
	AdjustVolume(ctx context.Context, delta int) error
	Stop(ctx context.Context) error
}

// VolumeIn takes an absolute percent (zero mutes), a relative delta, or
// neither, which means one step up. Percent is a pointer so an explicit zero
// is not confused with an absent argument.
type VolumeIn struct {
	Percent *int `json:"percent,omitempty"`
	Delta   int  `json:"delta,omitempty"`
}

// VolumeOut echoes the requested change for the log and the reply.
type VolumeOut struct {
	Percent int `json:"percent,omitempty"`
	Delta   int `json:"delta,omitempty"`
}

// StopIn has no arguments.
type StopIn struct{}

// StopOut reports that playback was stopped.
type StopOut struct {
	Stopped bool `json:"stopped"`
}

// NewVolume builds audio.volume_set.
func NewVolume(controls Controls) tool.Tool[VolumeIn, VolumeOut] {
	return tool.Tool[VolumeIn, VolumeOut]{
		Name:        VolumeName,
		Description: "Set the speaker volume to a percent, change it by a delta, or with no arguments turn it up one step.",
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
				return VolumeOut{Percent: *in.Percent}, nil
			}
			delta := in.Delta
			if delta == 0 {
				delta = StepPercent
			}
			if err := controls.AdjustVolume(ctx, delta); err != nil {
				return VolumeOut{}, err
			}
			return VolumeOut{Delta: delta}, nil
		},
		Speak: func(out VolumeOut) string {
			switch {
			case out.Delta > 0:
				return "Volume up."
			case out.Delta < 0:
				return "Volume down."
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
		},
		Extract: extractVolume,
	}
}

// extractVolume reads an absolute percent or a direction from the utterance.
func extractVolume(text string) (VolumeIn, bool) {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "louder"), strings.Contains(lower, "turn it up"), strings.Contains(lower, "volume up"):
		return VolumeIn{Delta: StepPercent}, true
	case strings.Contains(lower, "quieter"), strings.Contains(lower, "turn it down"), strings.Contains(lower, "volume down"):
		return VolumeIn{Delta: -StepPercent}, true
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
			"pause the music",
		},
	}
}

// Register adds both audio Tools.
func Register(r *tool.Registry, controls Controls) error {
	if err := tool.Add(r, NewVolume(controls)); err != nil {
		return err
	}
	return tool.Add(r, NewStop(controls))
}
