package spotify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	PlayName       = "spotify.play"
	PauseName      = "spotify.pause"
	NextName       = "spotify.next"
	VolumeName     = "spotify.volume"
	NowPlayingName = "spotify.now_playing"
)

// volumeStep is what "turn the music up" moves Spotify's volume by.
const volumeStep = 10

// PlayIn resumes (empty Query, no Kind) or searches. Kind is one of track,
// artist, album, playlist or liked; empty searches every type. Volume is an
// optional level to start at, still subject to music.max_volume.
type PlayIn struct {
	Query  string `json:"query,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Volume *int   `json:"volume,omitempty"`
}

// PlayOut says what started.
type PlayOut struct {
	Name    string `json:"name,omitempty"`
	Artist  string `json:"artist,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	Volume  int    `json:"volume,omitempty"`
	Capped  bool   `json:"capped,omitempty"`
}

// PauseIn and NextIn take no arguments.
type PauseIn struct{}

// PauseOut reports the pause.
type PauseOut struct {
	Paused bool `json:"paused"`
}

// NextIn takes no arguments.
type NextIn struct{}

// NextOut reports the skip.
type NextOut struct {
	Skipped bool `json:"skipped"`
}

// VolumeIn is an absolute percent or a relative delta of Spotify's own volume.
// Percent is a pointer so an explicit zero is not an absent argument.
type VolumeIn struct {
	Percent *int `json:"percent,omitempty"`
	Delta   int  `json:"delta,omitempty"`
}

// VolumeOut is the level that was applied after the cap.
type VolumeOut struct {
	Percent int  `json:"percent"`
	Capped  bool `json:"capped,omitempty"`
	Cap     int  `json:"cap,omitempty"`
}

// NowPlayingIn takes no arguments.
type NowPlayingIn struct{}

// NowPlayingOut is the current track; empty Track means nothing is playing.
type NowPlayingOut struct {
	Track   string `json:"track,omitempty"`
	Artists string `json:"artists,omitempty"`
	Album   string `json:"album,omitempty"`
	Playing bool   `json:"playing,omitempty"`
}

// NewPlay builds spotify.play.
func NewPlay(svc *Service) tool.Tool[PlayIn, PlayOut] {
	return tool.Tool[PlayIn, PlayOut]{
		Name:        PlayName,
		Description: "Play music on the speaker: resume, or search for a song, artist, album, playlist or the user's liked songs and play it.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in PlayIn) (PlayOut, error) {
			if in.Volume != nil && (*in.Volume < 0 || *in.Volume > 100) {
				return PlayOut{}, &tool.ToolError{Say: "I can only set the volume between 0 and 100."}
			}
			played, err := svc.player.Play(ctx, spotify.PlayRequest{Query: in.Query, Kind: in.Kind, Volume: in.Volume})
			if err != nil {
				return PlayOut{}, toolError(err)
			}
			// Music now holds Focus, so "pause" and "next" reach it.
			svc.takeFocus()
			return PlayOut(played), nil
		},
		Speak: func(out PlayOut) string {
			var line string
			switch {
			case out.Resumed:
				line = "Resuming the music."
			case out.Kind == spotify.KindTrack && out.Artist != "":
				line = fmt.Sprintf("Playing %s by %s.", out.Name, out.Artist)
			default:
				line = fmt.Sprintf("Playing %s.", out.Name)
			}
			if out.Capped {
				line += fmt.Sprintf(" The volume is capped at %d percent.", out.Volume)
			}
			return line
		},
		Examples: []string{
			"play some Radiohead",
			"play the album OK Computer",
			"play the song Karma Police",
			"play my liked songs",
			"play the song Hey Jude",
			"put on some jazz",
			"resume the music",
			"start playing music",
			"play music by Taylor Swift",
			"listen to the Beatles",
		},
		Extract: extractPlay,
	}
}

// NewPause builds spotify.pause.
func NewPause(svc *Service) tool.Tool[PauseIn, PauseOut] {
	return tool.Tool[PauseIn, PauseOut]{
		Name:        PauseName,
		Description: "Pause the music on the speaker.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ PauseIn) (PauseOut, error) {
			if err := svc.player.Pause(ctx); err != nil {
				return PauseOut{}, toolError(err)
			}
			return PauseOut{Paused: true}, nil
		},
		Speak: func(PauseOut) string { return "Paused." },
		Examples: []string{
			"pause the music",
			"pause the song that's playing",
			"pause my music",
			"pause this song",
		},
		Extract: func(text string) (PauseIn, bool) { return PauseIn{}, true },
	}
}

// NewNext builds spotify.next.
func NewNext(svc *Service) tool.Tool[NextIn, NextOut] {
	return tool.Tool[NextIn, NextOut]{
		Name:        NextName,
		Description: "Skip to the next song on the speaker.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ NextIn) (NextOut, error) {
			if err := svc.player.Next(ctx); err != nil {
				return NextOut{}, toolError(err)
			}
			return NextOut{Skipped: true}, nil
		},
		Speak: func(NextOut) string { return "Next track." },
		Examples: []string{
			"skip this song",
			"next song",
			"skip to the next track",
			"skip the song",
			"go to the next track",
		},
		Extract: func(text string) (NextIn, bool) { return NextIn{}, true },
	}
}

// NewVolume builds spotify.volume.
func NewVolume(svc *Service) tool.Tool[VolumeIn, VolumeOut] {
	return tool.Tool[VolumeIn, VolumeOut]{
		Name:        VolumeName,
		Description: "Set the music (Spotify) volume to a percent or change it by a delta. music.max_volume caps it when set.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in VolumeIn) (VolumeOut, error) {
			var (
				applied int
				capped  bool
				err     error
			)
			if in.Percent != nil {
				if *in.Percent < 0 || *in.Percent > 100 {
					return VolumeOut{}, &tool.ToolError{Say: "I can only set the music volume between 0 and 100."}
				}
				applied, capped, err = svc.player.SetVolume(ctx, *in.Percent)
			} else {
				delta := in.Delta
				if delta == 0 {
					delta = volumeStep
				}
				applied, capped, err = svc.player.AdjustVolume(ctx, delta)
			}
			if err != nil {
				return VolumeOut{}, toolError(err)
			}
			out := VolumeOut{Percent: applied, Capped: capped}
			if capped {
				out.Cap = svc.player.Cap()
			}
			return out, nil
		},
		Speak: func(out VolumeOut) string {
			if out.Capped {
				return fmt.Sprintf("The music volume is capped at %d percent.", out.Percent)
			}
			return fmt.Sprintf("Music volume set to %d percent.", out.Percent)
		},
		Examples: []string{
			"make the music louder",
			"make the music quieter",
			"lower the music",
			"raise the music",
			"set the music to 40 percent",
		},
		Extract: extractVolume,
	}
}

// NewNowPlaying builds spotify.now_playing. Its result names the user's
// listening, so it stays on the Pi.
func NewNowPlaying(svc *Service) tool.Tool[NowPlayingIn, NowPlayingOut] {
	return tool.Tool[NowPlayingIn, NowPlayingOut]{
		Name:        NowPlayingName,
		Description: "Say which song is playing right now.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(ctx context.Context, _ NowPlayingIn) (NowPlayingOut, error) {
			np, err := svc.player.NowPlaying(ctx)
			if err != nil {
				if isNothingPlaying(err) {
					return NowPlayingOut{}, nil
				}
				return NowPlayingOut{}, toolError(err)
			}
			return NowPlayingOut{Track: np.Track, Artists: np.Artists, Album: np.Album, Playing: np.Playing}, nil
		},
		Speak: func(out NowPlayingOut) string {
			if out.Track == "" {
				return "Nothing is playing."
			}
			what := out.Track
			if out.Artists != "" {
				what += " by " + out.Artists
			}
			if !out.Playing {
				return what + " is paused."
			}
			return what + " is playing."
		},
		Examples: []string{
			"what's playing",
			"what song is this",
			"who sings this song",
			"which track is playing",
			"what am I listening to",
		},
		Extract: func(text string) (NowPlayingIn, bool) { return NowPlayingIn{}, true },
	}
}

func isNothingPlaying(err error) bool {
	return errors.Is(err, spotify.ErrNothingPlaying)
}

var (
	trailing   = regexp.MustCompile(`(?i)[\s.!?,]+$`)
	politePre  = regexp.MustCompile(`(?i)^(?:hey\s+)?(?:please\s+|can you\s+|could you\s+|would you\s+)+`)
	politeTail = regexp.MustCompile(`(?i)\s+(?:please|on spotify|on the speaker)$`)
	resumeRe   = regexp.MustCompile(`(?i)^(?:play|resume|continue|start playing|unpause)(?:\s+playing)?(?:\s+(?:the|my|some))?\s*(?:music|spotify|playback|song|songs)?$`)
	likedRe    = regexp.MustCompile(`(?i)^(?:play|put on)\s+(?:my|the|some)?\s*(?:liked|saved|favou?rite)\s+(?:songs|tracks|music)$`)
	kindRe     = regexp.MustCompile(`(?i)^(?:play|put on)\s+(?:the\s+|some\s+)?(song|track|album|artist|band|playlist)\s+(.+)$`)
	musicByRe  = regexp.MustCompile(`(?i)^(?:play|put on)\s+(?:some\s+)?music\s+(?:by|from)\s+(.+)$`)
	playRe     = regexp.MustCompile(`(?i)^(?:play|put on|listen to)\s+(?:some\s+|the\s+)?(.+)$`)
)

// extractPlay fills PlayIn from a spoken request. It returns false when the
// words are not a play request, so the Agent path can still handle them.
func extractPlay(text string) (PlayIn, bool) {
	text = normalizeRequest(text)
	if text == "" {
		return PlayIn{}, false
	}
	if resumeRe.MatchString(text) {
		return PlayIn{}, true
	}
	if likedRe.MatchString(text) {
		return PlayIn{Kind: spotify.KindLiked}, true
	}
	if m := kindRe.FindStringSubmatch(text); m != nil {
		kind := strings.ToLower(m[1])
		switch kind {
		case "band":
			kind = spotify.KindArtist
		case "song":
			kind = spotify.KindTrack
		}
		return PlayIn{Query: byToSpace(m[2]), Kind: kind}, true
	}
	if m := musicByRe.FindStringSubmatch(text); m != nil {
		return PlayIn{Query: byToSpace(m[1]), Kind: spotify.KindArtist}, true
	}
	if m := playRe.FindStringSubmatch(text); m != nil {
		return PlayIn{Query: byToSpace(m[1])}, true
	}
	return PlayIn{}, false
}

// byToSpace turns "Creep by Radiohead" into "Creep Radiohead", the plain-text
// form Spotify's search ranks well.
func byToSpace(query string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(" "+query+" ", " by ", " ")), " ")
}

func normalizeRequest(text string) string {
	text = strings.TrimSpace(text)
	text = trailing.ReplaceAllString(text, "")
	text = politePre.ReplaceAllString(text, "")
	text = politeTail.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

var (
	volumeSubjectRe = regexp.MustCompile(`(?i)\b(?:music|spotify|song|songs|tunes)\b`)
	volumePercentRe = regexp.MustCompile(`(\d{1,3})\s*(?:%|percent)?`)
	volumeUpRe      = regexp.MustCompile(`(?i)\b(?:up|louder|raise|higher|increase)\b`)
	volumeDownRe    = regexp.MustCompile(`(?i)\b(?:down|quieter|softer|lower|decrease|reduce)\b`)
)

// extractVolume reads a music-volume request. It requires the word music (or
// Spotify) so a plain "volume up" stays with audio.volume_set.
func extractVolume(text string) (VolumeIn, bool) {
	text = normalizeRequest(text)
	if !volumeSubjectRe.MatchString(text) {
		return VolumeIn{}, false
	}
	if m := volumePercentRe.FindStringSubmatch(text); m != nil {
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		if n >= 0 && n <= 100 {
			return VolumeIn{Percent: &n}, true
		}
		return VolumeIn{}, false
	}
	switch {
	case volumeDownRe.MatchString(text):
		return VolumeIn{Delta: -volumeStep}, true
	case volumeUpRe.MatchString(text):
		return VolumeIn{Delta: volumeStep}, true
	}
	return VolumeIn{}, false
}
