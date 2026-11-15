package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
)

// TestMuteUsesTheSinkFlagWithoutTouchingRingOrQuietVolume pins the two
// independent boundaries: mute sets the sink mute flag and keeps the level,
// while a Ring's absolute volume settings and quiet.volume stay untouched.
func TestMuteUsesTheSinkFlagWithoutTouchingRingOrQuietVolume(t *testing.T) {
	controls := &audioRecorder{level: 40}
	reg := tool.NewRegistry(time.Second)
	if err := audiocontrol.Register(reg, controls); err != nil {
		t.Fatalf("register audio: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{"mute": audiocontrol.VolumeName})

	reply := p.Handle(context.Background(), request(), "mute")
	if reply.Path != tool.PathControlPhrase || reply.Tool != audiocontrol.VolumeName {
		t.Fatalf("reply = %+v, want audio.volume_set", reply)
	}
	if controls.muteCalls != 1 || !controls.muted {
		t.Fatalf("sink mute calls/flag = %d/%v, want the flag set once", controls.muteCalls, controls.muted)
	}
	if controls.setCalls != 0 {
		t.Fatalf("sink volume writes = %d after mute, want the level kept", controls.setCalls)
	}

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	defs := append(ring.Definitions(), quiet.Definitions(quiet.DefaultConfig())...)
	svc, err := settings.New(context.Background(), st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}

	for _, kind := range []ring.Kind{ring.Timer, ring.Alarm} {
		got := ring.ReadSettings(svc, kind)
		want := ring.Defaults(kind)
		if got.Volume != want.Volume {
			t.Errorf("%s Ring volume = %d after mute, want the untouched default %d", kind, got.Volume, want.Volume)
		}
	}
	quietService := quiet.New(quiet.Options{Reader: svc, Now: time.Now})
	if got := quietService.Status().Volume; got != quiet.DefaultVolume {
		t.Errorf("quiet.volume = %d after mute, want the untouched default %d", got, quiet.DefaultVolume)
	}
}
