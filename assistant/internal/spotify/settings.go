package spotify

import (
	"fmt"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Definitions are the live Spotify settings. music.max_volume is deliberately
// unset (0, no cap) until the #70 hardware spike measures a safe value; the
// Dashboard's generic Settings page edits it with no page-specific code.
func Definitions() []settings.Definition {
	return []settings.Definition{
		settings.Int(MaxVolumeKey, 0,
			settings.WithDescription("Highest Spotify volume percent the assistant will set (1-99). 0 means no cap."),
			settings.WithValidator(func(value any) error {
				n, ok := value.(int)
				if !ok || n < 0 || n > 100 {
					return fmt.Errorf("%s must be a whole number from 0 to 100", MaxVolumeKey)
				}
				return nil
			}),
		),
	}
}

// MaxVolume reads the live cap; nil service or a bad value means no cap.
func MaxVolume(svc interface {
	Get(key string) (settings.Value, bool)
}) int {
	if svc == nil {
		return 0
	}
	if value, ok := svc.Get(MaxVolumeKey); ok {
		if n, ok := value.Value.(int); ok && n > 0 && n < 100 {
			return n
		}
	}
	return 0
}
