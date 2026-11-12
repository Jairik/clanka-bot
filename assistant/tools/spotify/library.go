package spotify

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical names of the playlist and library Tools (#73).
const (
	PlayPlaylistName = "spotify.play_playlist"
	ShuffleName      = "spotify.shuffle"
	RepeatName       = "spotify.repeat"
	PreviousName     = "spotify.previous"
	QueueAddName     = "spotify.queue_add"
	LikeName         = "spotify.like"
)

// PlayPlaylistIn names one of the user's own playlists as it was spoken.
type PlayPlaylistIn struct {
	Name    string `json:"name"`
	Shuffle bool   `json:"shuffle,omitempty"`
}

// PlayPlaylistOut says which playlist started. Name is the library's spelling,
// not the spoken one.
type PlayPlaylistOut struct {
	Name    string `json:"name"`
	Shuffle bool   `json:"shuffle,omitempty"`
	Volume  int    `json:"volume,omitempty"`
	Capped  bool   `json:"capped,omitempty"`
}

// ShuffleIn turns shuffle on or off; an absent On means on.
type ShuffleIn struct {
	On *bool `json:"on,omitempty"`
}

// ShuffleOut is the shuffle state that was set.
type ShuffleOut struct {
	On bool `json:"on"`
}

// RepeatIn is off, track (this song) or context (the playlist or album).
type RepeatIn struct {
	Mode string `json:"mode"`
}

// RepeatOut is the repeat mode that was set.
type RepeatOut struct {
	Mode string `json:"mode"`
}

// PreviousIn takes no arguments.
type PreviousIn struct{}

// PreviousOut reports the step back.
type PreviousOut struct {
	Went bool `json:"went"`
}

// QueueAddIn is the spoken track to queue.
type QueueAddIn struct {
	Query string `json:"query"`
}

// QueueAddOut is the track that was queued.
type QueueAddOut struct {
	Name   string `json:"name"`
	Artist string `json:"artist,omitempty"`
}

// LikeIn takes no arguments; it likes what is playing.
type LikeIn struct{}

// LikeOut is the track that was saved.
type LikeOut struct {
	Track   string `json:"track"`
	Artists string `json:"artists,omitempty"`
}

// unavailable is the spoken line when the module was built without the
// playlist seams, for example by a test that only wants playback.
func (s *Service) unavailable() error {
	return &tool.ToolError{Say: "Spotify playlists aren't available right now."}
}

// resolvePlaylist maps a spoken name to one of the user's playlists through
// the cache and the shared name matcher. It never forces a guess: a name that
// resolves to nothing, or ambiguously, is a spoken "couldn't find".
func (s *Service) resolvePlaylist(ctx context.Context, spoken string) (spotify.Playlist, error) {
	if s.library == nil || s.playlists == nil {
		return spotify.Playlist{}, s.unavailable()
	}
	// The startup refresh runs in the background, so the first request can
	// arrive before it finishes. Load once here rather than say "not found".
	if !s.playlists.Loaded() {
		if err := s.playlists.Refresh(ctx); err != nil {
			return spotify.Playlist{}, toolError(err)
		}
	}
	res := s.names.Match(ctx, spoken, s.playlists.Names())
	if res.Matched() {
		if pl, ok := s.playlists.Lookup(res.Name); ok {
			return pl, nil
		}
	}
	switch res.Reason {
	case namematch.ReasonNoCandidates:
		return spotify.Playlist{}, &tool.ToolError{Say: "I don't see any playlists in your Spotify."}
	case namematch.ReasonEmptyName:
		return spotify.Playlist{}, &tool.ToolError{Say: "Which playlist should I play?"}
	case namematch.ReasonEmbedFailed, namematch.ReasonInvalidVectors:
		return spotify.Playlist{}, &tool.ToolError{Say: "I couldn't check your playlists right now."}
	}
	return spotify.Playlist{}, &tool.ToolError{Say: fmt.Sprintf("I couldn't find a playlist called %s.", strings.TrimSpace(spoken))}
}

// NewPlayPlaylist builds spotify.play_playlist.
func NewPlayPlaylist(svc *Service) tool.Tool[PlayPlaylistIn, PlayPlaylistOut] {
	return tool.Tool[PlayPlaylistIn, PlayPlaylistOut]{
		Name:        PlayPlaylistName,
		Description: "Play one of the user's own saved Spotify playlists by name, optionally shuffled.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in PlayPlaylistIn) (PlayPlaylistOut, error) {
			pl, err := svc.resolvePlaylist(ctx, in.Name)
			if err != nil {
				return PlayPlaylistOut{}, err
			}
			played, err := svc.library.PlayPlaylist(ctx, spotify.PlaylistPlay{Name: pl.Name, URI: pl.URI, Shuffle: in.Shuffle})
			if err != nil {
				return PlayPlaylistOut{}, toolError(err)
			}
			// Like spotify.play, a started playlist holds Music Focus.
			svc.takeFocus()
			return PlayPlaylistOut{Name: played.Name, Shuffle: in.Shuffle, Volume: played.Volume, Capped: played.Capped}, nil
		},
		Speak: func(out PlayPlaylistOut) string {
			line := fmt.Sprintf("Playing your %s playlist", out.Name)
			if out.Shuffle {
				line += " on shuffle"
			}
			line += "."
			if out.Capped {
				line += fmt.Sprintf(" The volume is capped at %d percent.", out.Volume)
			}
			return line
		},
		Examples: []string{
			"play my running playlist",
			"play my workout playlist on shuffle",
			"put on my road trip playlist",
			"play my study playlist shuffled",
			"start my gym playlist",
		},
		Extract: extractPlayPlaylist,
	}
}

// NewShuffle builds spotify.shuffle.
func NewShuffle(svc *Service) tool.Tool[ShuffleIn, ShuffleOut] {
	return tool.Tool[ShuffleIn, ShuffleOut]{
		Name:        ShuffleName,
		Description: "Turn shuffle on or off for the music that is playing.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in ShuffleIn) (ShuffleOut, error) {
			if svc.library == nil {
				return ShuffleOut{}, svc.unavailable()
			}
			on := in.On == nil || *in.On
			if err := svc.library.SetShuffle(ctx, on); err != nil {
				return ShuffleOut{}, toolError(err)
			}
			return ShuffleOut{On: on}, nil
		},
		Speak: func(out ShuffleOut) string {
			if out.On {
				return "Shuffle is on."
			}
			return "Shuffle is off."
		},
		Examples: []string{
			"shuffle the music",
			"turn shuffle on",
			"turn off shuffle",
			"stop shuffling",
			"shuffle my songs",
		},
		Extract: extractShuffle,
	}
}

// NewRepeat builds spotify.repeat.
func NewRepeat(svc *Service) tool.Tool[RepeatIn, RepeatOut] {
	return tool.Tool[RepeatIn, RepeatOut]{
		Name:        RepeatName,
		Description: "Set the music repeat mode: repeat this song, repeat the whole playlist or album, or turn repeat off.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in RepeatIn) (RepeatOut, error) {
			if svc.library == nil {
				return RepeatOut{}, svc.unavailable()
			}
			switch in.Mode {
			case spotify.RepeatOff, spotify.RepeatTrack, spotify.RepeatContext:
			default:
				return RepeatOut{}, &tool.ToolError{Say: "I can repeat this song, the playlist, or turn repeat off."}
			}
			if err := svc.library.SetRepeat(ctx, in.Mode); err != nil {
				return RepeatOut{}, toolError(err)
			}
			return RepeatOut{Mode: in.Mode}, nil
		},
		Speak: func(out RepeatOut) string {
			switch out.Mode {
			case spotify.RepeatTrack:
				return "Repeating this song."
			case spotify.RepeatContext:
				return "Repeating the playlist."
			}
			return "Repeat is off."
		},
		Examples: []string{
			"repeat this song",
			"put this song on repeat",
			"turn on repeat",
			"turn off repeat",
			"stop repeating the song",
		},
		Extract: extractRepeat,
	}
}

// NewPrevious builds spotify.previous. It is a plain Tool, not a Focus intent:
// the shared intent vocabulary is fixed (stop, pause, resume, next, cancel,
// snooze), and "previous" means nothing to a Ring or a reply.
func NewPrevious(svc *Service) tool.Tool[PreviousIn, PreviousOut] {
	return tool.Tool[PreviousIn, PreviousOut]{
		Name:        PreviousName,
		Description: "Go back to the previous song on the speaker.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ PreviousIn) (PreviousOut, error) {
			if svc.library == nil {
				return PreviousOut{}, svc.unavailable()
			}
			if err := svc.library.Previous(ctx); err != nil {
				return PreviousOut{}, toolError(err)
			}
			return PreviousOut{Went: true}, nil
		},
		Speak: func(PreviousOut) string { return "Previous track." },
		Examples: []string{
			"previous track",
			"go back a song",
			"play the previous song",
		},
		Extract: func(string) (PreviousIn, bool) { return PreviousIn{}, true },
	}
}

// NewQueueAdd builds spotify.queue_add.
func NewQueueAdd(svc *Service) tool.Tool[QueueAddIn, QueueAddOut] {
	return tool.Tool[QueueAddIn, QueueAddOut]{
		Name:        QueueAddName,
		Description: "Add a song to the play queue so it plays after the current one.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in QueueAddIn) (QueueAddOut, error) {
			if svc.library == nil {
				return QueueAddOut{}, svc.unavailable()
			}
			if strings.TrimSpace(in.Query) == "" {
				return QueueAddOut{}, &tool.ToolError{Say: "Which song should I add to the queue?"}
			}
			queued, err := svc.library.QueueAdd(ctx, in.Query)
			if err != nil {
				return QueueAddOut{}, toolError(err)
			}
			return QueueAddOut{Name: queued.Name, Artist: queued.Artist}, nil
		},
		Speak: func(out QueueAddOut) string {
			if out.Artist != "" {
				return fmt.Sprintf("Added %s by %s to the queue.", out.Name, out.Artist)
			}
			return fmt.Sprintf("Added %s to the queue.", out.Name)
		},
		Examples: []string{
			"add Karma Police to the queue",
			"queue up Creep by Radiohead",
			"add Yellow to my queue",
			"queue Wonderwall next",
		},
		Extract: extractQueueAdd,
	}
}

// NewLike builds spotify.like. It saves what is playing to the user's liked
// songs, which is an edit to their account, so it never guesses a track: with
// nothing playing it says so.
func NewLike(svc *Service) tool.Tool[LikeIn, LikeOut] {
	return tool.Tool[LikeIn, LikeOut]{
		Name:        LikeName,
		Description: "Save the song that is playing to the user's liked songs.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ LikeIn) (LikeOut, error) {
			if svc.library == nil {
				return LikeOut{}, svc.unavailable()
			}
			liked, err := svc.library.LikeCurrent(ctx)
			if err != nil {
				if isNothingPlaying(err) {
					return LikeOut{}, &tool.ToolError{Say: "Nothing is playing to like."}
				}
				return LikeOut{}, toolError(err)
			}
			return LikeOut{Track: liked.Track, Artists: liked.Artists}, nil
		},
		Speak: func(out LikeOut) string {
			if out.Artists != "" {
				return fmt.Sprintf("Saved %s by %s to your liked songs.", out.Track, out.Artists)
			}
			return fmt.Sprintf("Saved %s to your liked songs.", out.Track)
		},
		Examples: []string{
			"like this song",
			"save this song to my liked songs",
			"I love this song, save it",
			"heart this track",
			"add this song to my favorites",
		},
		Extract: func(string) (LikeIn, bool) { return LikeIn{}, true },
	}
}

var (
	shuffleTailRe = regexp.MustCompile(`(?i)\s+(?:on shuffle|shuffled|with shuffle|in shuffle mode|and shuffle it|on random)$`)
	// "play my <name> playlist [shuffle phrase]" and "shuffle my <name> playlist".
	namedPlaylistRe = regexp.MustCompile(`(?i)^(?:(play|put on|start|listen to|shuffle)\s+)(?:my|the)\s+(.+?)\s+playlist(?:\s+(.+))?$`)
	// "play the playlist <name>" and "play my playlist called <name>".
	playlistNamedRe = regexp.MustCompile(`(?i)^(?:(play|put on|start|listen to|shuffle)\s+)(?:my|the)\s+playlist\s+(?:called\s+|named\s+)?(.+)$`)

	shuffleOffRe = regexp.MustCompile(`(?i)^(?:turn\s+(?:shuffle\s+off|off\s+shuffle)|stop\s+shuffling(?:\s+(?:the|my|this)?\s*(?:music|songs|tracks|playlist))?|shuffle\s+off|disable\s+shuffle|no\s+shuffle|unshuffle(?:\s+(?:the|my)?\s*(?:music|songs|playlist))?)$`)
	shuffleOnRe  = regexp.MustCompile(`(?i)^(?:turn\s+(?:shuffle\s+on|on\s+shuffle)|enable\s+shuffle|shuffle(?:\s+(?:the|my|this|these))?\s*(?:music|songs|tracks|playlist|album|queue)?|put\s+(?:it|the\s+music|this)\s+on\s+shuffle)$`)

	repeatOffRe   = regexp.MustCompile(`(?i)^(?:turn\s+(?:repeat\s+off|off\s+repeat)|stop\s+repeating(?:\s+(?:the|this|my)?\s*(?:song|track|playlist|album|music))?|repeat\s+off|disable\s+repeat|no\s+repeat)$`)
	repeatTrackRe = regexp.MustCompile(`(?i)^(?:repeat\s+(?:this|the\s+current|that)\s+(?:song|track)|(?:put|set)\s+(?:this|the|that)\s+(?:song|track)\s+on\s+repeat|(?:this|the)\s+(?:song|track)\s+on\s+repeat|loop\s+(?:this|the)\s+(?:song|track))$`)
	repeatListRe  = regexp.MustCompile(`(?i)^(?:repeat\s+(?:this|the|my)?\s*(?:playlist|album|music|queue|all)|turn\s+(?:repeat\s+on|on\s+repeat)|(?:put|set)\s+(?:the\s+|this\s+)?(?:playlist|album|music)\s+on\s+repeat|loop\s+(?:the|this)\s+(?:playlist|album))$`)

	queueAddRe  = regexp.MustCompile(`(?i)^(?:add|queue(?:\s+up)?|put)\s+(.+?)\s+(?:to|on|in|onto)\s+(?:the\s+|my\s+|up\s+)?(?:queue|next)$`)
	queueVerbRe = regexp.MustCompile(`(?i)^queue(?:\s+up)?\s+(?:a\s+song\s+called\s+|the\s+song\s+)?(.+?)(?:\s+next)?$`)
)

// extractPlayPlaylist reads "play my <name> playlist [on shuffle]". It needs a
// name: a request without one is not this Tool's.
func extractPlayPlaylist(text string) (PlayPlaylistIn, bool) {
	text = normalizeRequest(text)
	if m := playlistNamedRe.FindStringSubmatch(text); m != nil {
		name := m[2]
		shuffle := strings.EqualFold(m[1], "shuffle")
		if shuffleTailRe.MatchString(name) {
			name, shuffle = shuffleTailRe.ReplaceAllString(name, ""), true
		}
		if name = strings.TrimSpace(name); name != "" {
			return PlayPlaylistIn{Name: name, Shuffle: shuffle}, true
		}
		return PlayPlaylistIn{}, false
	}
	if m := namedPlaylistRe.FindStringSubmatch(text); m != nil {
		shuffle := strings.EqualFold(m[1], "shuffle")
		if tail := strings.TrimSpace(m[3]); tail != "" {
			// Only a shuffle phrase may follow the word "playlist"; anything
			// else is a different request.
			if !shuffleTailRe.MatchString(" " + tail) {
				return PlayPlaylistIn{}, false
			}
			shuffle = true
		}
		return PlayPlaylistIn{Name: strings.TrimSpace(m[2]), Shuffle: shuffle}, true
	}
	return PlayPlaylistIn{}, false
}

func extractShuffle(text string) (ShuffleIn, bool) {
	text = normalizeRequest(text)
	off, on := false, true
	switch {
	case shuffleOffRe.MatchString(text):
		return ShuffleIn{On: &off}, true
	case shuffleOnRe.MatchString(text):
		return ShuffleIn{On: &on}, true
	}
	return ShuffleIn{}, false
}

// extractRepeat needs a music word. A bare "repeat that" is assistant.repeat's.
func extractRepeat(text string) (RepeatIn, bool) {
	text = normalizeRequest(text)
	switch {
	case repeatOffRe.MatchString(text):
		return RepeatIn{Mode: spotify.RepeatOff}, true
	case repeatTrackRe.MatchString(text):
		return RepeatIn{Mode: spotify.RepeatTrack}, true
	case repeatListRe.MatchString(text):
		return RepeatIn{Mode: spotify.RepeatContext}, true
	}
	return RepeatIn{}, false
}

// vagueQuery are requests that name no song, so there is nothing to search.
var vagueQuery = map[string]bool{
	"this": true, "that": true, "it": true, "a song": true, "a track": true,
	"something": true, "this song": true, "this track": true, "another song": true,
}

func extractQueueAdd(text string) (QueueAddIn, bool) {
	text = normalizeRequest(text)
	for _, re := range []*regexp.Regexp{queueAddRe, queueVerbRe} {
		if m := re.FindStringSubmatch(text); m != nil {
			if query := byToSpace(m[1]); query != "" && !vagueQuery[strings.ToLower(query)] {
				return QueueAddIn{Query: query}, true
			}
			return QueueAddIn{}, false
		}
	}
	return QueueAddIn{}, false
}
