package ring_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

// gainSpeaker records the software gain attached to each label or fallback.
type gainSpeaker struct {
	mu    sync.Mutex
	gains []int
	texts []string
}

func (s *gainSpeaker) SayText(ctx context.Context, text string) error {
	s.mu.Lock()
	s.gains = append(s.gains, speech.GainFrom(ctx))
	s.texts = append(s.texts, text)
	s.mu.Unlock()
	return nil
}

func (s *gainSpeaker) values() ([]int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.gains...), append([]string(nil), s.texts...)
}

// onePassEngine builds an engine whose first Cue pass cancels the Ring, so the
// test observes exactly one label and one pass.
func onePassEngine(t *testing.T, speaker ring.Speaker, gain func(ring.Kind) int) *ring.Engine {
	t.Helper()
	_, cancel := context.WithCancel(context.Background())
	engine := ring.New(ring.Deps{
		Clock:      newFakeClock(),
		Speaker:    speaker,
		SpeechGain: gain,
		Settings: func(ring.Kind) ring.Settings {
			return ring.Settings{MaxRing: 5 * time.Second, Volume: 80, Gap: time.Second, SpeakLabel: ring.SpeakStart}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) {
			return sound.Clip{PCM: []byte{0, 0}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}}, true
		},
		Play: func(context.Context, sound.Clip) error {
			cancel()
			return nil
		},
	})
	t.Cleanup(cancel)
	return engine
}

func TestSpeechGainAppliesToTheTimerLabel(t *testing.T) {
	speaker := &gainSpeaker{}
	engine := onePassEngine(t, speaker, func(kind ring.Kind) int {
		if kind == ring.Timer {
			return 35
		}
		return 100
	})
	engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, ID: "1", Label: "pasta"})
	gains, texts := speaker.values()
	if len(gains) == 0 || gains[0] != 35 {
		t.Fatalf("gains = %v, want the Timer label at 35", gains)
	}
	if len(texts) == 0 || texts[0] != "Pasta timer." {
		t.Fatalf("texts = %v, want the Timer label", texts)
	}
}

func TestSpeechGainLeavesTheAlarmAtNormalVolume(t *testing.T) {
	speaker := &gainSpeaker{}
	engine := onePassEngine(t, speaker, func(kind ring.Kind) int {
		if kind == ring.Timer {
			return 20
		}
		return 100
	})
	engine.Ring(context.Background(), ring.Request{Kind: ring.Alarm, ID: "2", Label: "wake"})
	gains, _ := speaker.values()
	if len(gains) == 0 || gains[0] != 100 {
		t.Fatalf("gains = %v, want the Alarm label at 100", gains)
	}
}

func TestSpeechGainAppliesToTheNoSoundFallback(t *testing.T) {
	speaker := &gainSpeaker{}
	engine := ring.New(ring.Deps{
		Clock:      newFakeClock(),
		Speaker:    speaker,
		SpeechGain: func(ring.Kind) int { return 30 },
		Settings: func(ring.Kind) ring.Settings {
			return ring.Settings{MaxRing: time.Second, Volume: 80, Gap: time.Second, SpeakLabel: ring.SpeakNever}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) { return sound.Clip{}, false },
	})
	engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, ID: "3", FallbackText: "Your timer is done."})
	gains, texts := speaker.values()
	if len(gains) == 0 || gains[0] != 30 {
		t.Fatalf("gains = %v, want the fallback at 30", gains)
	}
	if len(texts) == 0 || texts[0] != "Your timer is done." {
		t.Fatalf("texts = %v, want the fallback text", texts)
	}
}
