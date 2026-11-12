package spotify

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when an Out type changes.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		PlayName:         PlayOut{Name: "Karma Police", Artist: "Radiohead", Kind: "track"},
		PauseName:        PauseOut{Paused: true},
		NextName:         NextOut{Skipped: true},
		VolumeName:       VolumeOut{Percent: 40},
		NowPlayingName:   NowPlayingOut{Track: "Creep", Artists: "Radiohead", Playing: true},
		PlayPlaylistName: PlayPlaylistOut{Name: "Running", Shuffle: true},
		ShuffleName:      ShuffleOut{On: true},
		RepeatName:       RepeatOut{Mode: "track"},
		PreviousName:     PreviousOut{Went: true},
		QueueAddName:     QueueAddOut{Name: "Creep", Artist: "Radiohead"},
		LikeName:         LikeOut{Track: "Creep", Artists: "Radiohead"},
	}
}
