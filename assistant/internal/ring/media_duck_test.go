package ring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

// countedMusic records Duck and Restore so a media Ring can prove it holds
// exactly one duck for the whole playback.
type countedMusic struct {
	mu       sync.Mutex
	ducks    int
	restores int
}

func (m *countedMusic) Duck(context.Context) (func(), error) {
	m.mu.Lock()
	m.ducks++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.restores++
		m.mu.Unlock()
	}, nil
}

func (m *countedMusic) counts() (ducks, restores int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ducks, m.restores
}

// spokenAlert records fallback speech and how many ducks were held while speaking.
type spokenAlert struct {
	mu         sync.Mutex
	texts      []string
	ducksAtSay int
	music      *countedMusic
}

func (s *spokenAlert) SayText(_ context.Context, text string) error {
	ducks, restores := s.music.counts()
	s.mu.Lock()
	s.texts = append(s.texts, text)
	s.ducksAtSay = ducks
	// Restore must not have run yet: speech is part of the ducked lifetime.
	if restores != 0 {
		s.texts = append(s.texts, "restored-too-early")
	}
	s.mu.Unlock()
	return nil
}

func (s *spokenAlert) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// withMediaLimits shortens the media cap and the silence budget for one test.
// It is not safe alongside t.Parallel.
func withMediaLimits(t *testing.T, cap, budget time.Duration) {
	t.Helper()
	prevCap, prevBudget := mediaCap, firstAudioBudget
	mediaCap, firstAudioBudget = cap, budget
	t.Cleanup(func() {
		mediaCap, firstAudioBudget = prevCap, prevBudget
	})
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func wantOneDuck(t *testing.T, music *countedMusic) {
	t.Helper()
	ducks, restores := music.counts()
	if ducks != 1 || restores != 1 {
		t.Fatalf("duck=%d restore=%d, want 1 and 1", ducks, restores)
	}
}

// TestMediaLimitsMatchTheSpec keeps the production cap and the silence budget
// at the values the Pi check depends on.
func TestMediaLimitsMatchTheSpec(t *testing.T) {
	if defaultMediaCap != 10*time.Minute {
		t.Fatalf("media cap = %s, want 10m", defaultMediaCap)
	}
	if defaultFirstAudioBudget != 3*time.Second {
		t.Fatalf("first audio budget = %s, want 3s", defaultFirstAudioBudget)
	}
}

// TestMediaDucksOnce proves success, a load error and the media cap each take
// one duck and one restore. The load error still speaks, and it does not duck
// a second time for that fallback.
func TestMediaDucksOnce(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		music := &countedMusic{}
		engine := New(Deps{
			Logger: discardLog(),
			Music:  music,
			Media: func(_ context.Context, audio string, onFirst func()) error {
				if audio != "https://youtu.be/abc" {
					t.Errorf("audio=%q", audio)
				}
				onFirst()
				onFirst() // a second chunk must not panic or duck again
				return nil
			},
		})
		result := engine.Ring(t.Context(), Request{Kind: Timer, Audio: "https://youtu.be/abc"})
		if !result.Played || result.Outcome != OutcomeStopped || result.EndedBy != "completed" {
			t.Fatalf("result=%+v", result)
		}
		wantOneDuck(t, music)
	})

	t.Run("error", func(t *testing.T) {
		music := &countedMusic{}
		speaker := &spokenAlert{music: music}
		engine := New(Deps{
			Logger:  discardLog(),
			Music:   music,
			Speaker: speaker,
			Media: func(context.Context, string, func()) error {
				return errors.New("load failed")
			},
		})
		result := engine.Ring(t.Context(), Request{Kind: Timer, Audio: "https://youtu.be/missing"})
		if !result.Played || result.Outcome != OutcomeStopped || result.EndedBy != "spoken" {
			t.Fatalf("result=%+v", result)
		}
		if got := speaker.all(); len(got) != 1 || got[0] != "Your timer is done." {
			t.Fatalf("speech=%v", got)
		}
		if speaker.ducksAtSay != 1 {
			t.Fatalf("ducks while speaking=%d, want 1", speaker.ducksAtSay)
		}
		wantOneDuck(t, music)
	})

	t.Run("error after audio", func(t *testing.T) {
		music := &countedMusic{}
		speaker := &spokenAlert{music: music}
		engine := New(Deps{
			Logger:  discardLog(),
			Music:   music,
			Speaker: speaker,
			Media: func(_ context.Context, _ string, onFirst func()) error {
				onFirst()
				return errors.New("stream died")
			},
		})
		result := engine.Ring(t.Context(), Request{Kind: Timer, Audio: "https://youtu.be/abc"})
		if !result.Played || result.EndedBy != "spoken" {
			t.Fatalf("result=%+v", result)
		}
		if speaker.ducksAtSay != 1 {
			t.Fatalf("ducks while speaking=%d, want 1", speaker.ducksAtSay)
		}
		wantOneDuck(t, music)
	})

	t.Run("timeout", func(t *testing.T) {
		// Shorten the cap so the test does not wait ten minutes. The silence
		// budget stays longer than the cap, so a stream that already started
		// ends as a timeout rather than the spoken fallback.
		withMediaLimits(t, 200*time.Millisecond, time.Hour)
		music := &countedMusic{}
		speaker := &spokenAlert{music: music}
		engine := New(Deps{
			Logger:  discardLog(),
			Music:   music,
			Speaker: speaker,
			Media: func(ctx context.Context, _ string, onFirst func()) error {
				onFirst()
				<-ctx.Done()
				return ctx.Err()
			},
		})
		result := engine.Ring(t.Context(), Request{Kind: Timer, Audio: "https://youtu.be/abc"})
		if !result.Played || result.Outcome != OutcomeMissed || result.EndedBy != "timeout" {
			t.Fatalf("result=%+v", result)
		}
		if len(speaker.all()) != 0 {
			t.Fatalf("timeout spoke %v", speaker.all())
		}
		wantOneDuck(t, music)
	})
}

// TestMediaNoPCMFallsBackToSpeech cancels a stream that never produces audio
// and speaks the same alert a load failure speaks.
func TestMediaNoPCMFallsBackToSpeech(t *testing.T) {
	withMediaLimits(t, time.Hour, 200*time.Millisecond)
	music := &countedMusic{}
	speaker := &spokenAlert{music: music}
	engine := New(Deps{
		Logger:  discardLog(),
		Music:   music,
		Speaker: speaker,
		Media: func(ctx context.Context, _ string, _ func()) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	start := time.Now()
	result := engine.Ring(t.Context(), Request{Kind: Timer, Audio: "https://youtu.be/slow", FallbackText: "Your timer is done."})
	elapsed := time.Since(start)
	if !result.Played || result.Outcome != OutcomeStopped || result.EndedBy != "spoken" {
		t.Fatalf("result=%+v", result)
	}
	if got := speaker.all(); len(got) != 1 || got[0] != "Your timer is done." {
		t.Fatalf("speech=%v", got)
	}
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("fallback after %s, want it inside the silence budget", elapsed)
	}
	if speaker.ducksAtSay != 1 {
		t.Fatalf("ducks while speaking=%d, want 1", speaker.ducksAtSay)
	}
	wantOneDuck(t, music)
}

// TestMediaStopBeforeAudioDoesNotSpeak keeps a stop during the silence wait
// from becoming the spoken fallback.
func TestMediaStopBeforeAudioDoesNotSpeak(t *testing.T) {
	withMediaLimits(t, time.Hour, time.Hour)
	music := &countedMusic{}
	speaker := &spokenAlert{music: music}
	registry := focus.New()
	entered := make(chan struct{})
	engine := New(Deps{
		Logger:  discardLog(),
		Music:   music,
		Speaker: speaker,
		Focus:   registry,
		Media: func(ctx context.Context, _ string, _ func()) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		},
	})
	done := make(chan Result, 1)
	go func() {
		done <- engine.Ring(context.Background(), Request{Kind: Timer, Audio: "https://youtu.be/abc"})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("media did not start")
	}
	if _, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop did not reach the media Ring")
	}
	result := <-done
	if !result.Played || result.Outcome != OutcomeStopped || result.EndedBy != "stop" {
		t.Fatalf("result=%+v", result)
	}
	if len(speaker.all()) != 0 {
		t.Fatalf("stop spoke %v", speaker.all())
	}
	wantOneDuck(t, music)
}

// TestMediaCueStopsWhenAudioStarts plays the kind cue as soon as media starts
// and cancels it when the first audio is signalled, while media keeps running.
func TestMediaCueStopsWhenAudioStarts(t *testing.T) {
	withMediaLimits(t, 2*time.Second, time.Hour)
	played := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	engine := New(Deps{
		Logger: discardLog(),
		Clip: func(Kind) (sound.Clip, bool) {
			return sound.Clip{
				PCM:    []byte{0x00, 0x10, 0x00, 0x10},
				Format: audio.Format{Rate: 8000, Width: 2, Channels: 1},
			}, true
		},
		Play: func(ctx context.Context, clip sound.Clip) error {
			if len(clip.PCM) == 0 {
				t.Errorf("cue PCM is empty")
			}
			once.Do(func() { close(played) })
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		},
		Media: func(ctx context.Context, _ string, onFirst func()) error {
			select {
			case <-played:
			case <-ctx.Done():
				return ctx.Err()
			}
			onFirst()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Error("cue kept playing after the first audio")
			}
			return nil
		},
	})
	result := engine.Ring(t.Context(), Request{Kind: Timer, Audio: "https://youtu.be/abc"})
	if !result.Played || result.Outcome != OutcomeStopped || result.EndedBy != "completed" {
		t.Fatalf("result=%+v", result)
	}
}
