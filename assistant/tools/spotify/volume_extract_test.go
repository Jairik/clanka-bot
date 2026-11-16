package spotify_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	spotifytools "github.com/Jairik/clanka-bot/assistant/tools/spotify"
)

// TestMusicVolumeExtractGrammar runs the shared direction-plus-number grammar
// through the Spotify extractor: a number after "to"/"at" is absolute, a number
// after a bare direction is relative, and a bare direction is one step.
func TestMusicVolumeExtractGrammar(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	module := spotifytools.NewModule()
	if err := module.Register(reg, tool.Deps{Secrets: fakeStore{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	const name = spotifytools.VolumeName
	cases := []struct {
		text string
		want spotifytools.VolumeIn
	}{
		{"turn the music volume down to 20", spotifytools.VolumeIn{Percent: spi(20)}},
		{"turn the music up to 80", spotifytools.VolumeIn{Percent: spi(80)}},
		{"turn the music down by 30 percent", spotifytools.VolumeIn{Percent: nil, Delta: -30}},
		{"music volume up to 70", spotifytools.VolumeIn{Percent: spi(70)}},
		{"set the music volume to 5 percent", spotifytools.VolumeIn{Percent: spi(5)}},
		{"music volume 100", spotifytools.VolumeIn{Percent: spi(100)}},
		{"make the music louder", spotifytools.VolumeIn{Percent: nil, Delta: 10}},
		{"make the music quieter", spotifytools.VolumeIn{Percent: nil, Delta: -10}},
		{"raise the music volume", spotifytools.VolumeIn{Percent: nil, Delta: 10}},
		{"lower the music volume", spotifytools.VolumeIn{Percent: nil, Delta: -10}},
	}
	for _, tc := range cases {
		args, ok := reg.Extract(name, tc.text)
		if !ok {
			t.Fatalf("Extract(%q) failed", tc.text)
		}
		var got spotifytools.VolumeIn
		if err := json.Unmarshal(args, &got); err != nil {
			t.Fatalf("Extract(%q) unmarshal: %v", tc.text, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Extract(%q) = %+v, want %+v", tc.text, got, tc.want)
		}
	}
}

func spi(n int) *int { return &n }
