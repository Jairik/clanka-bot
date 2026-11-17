package conversation_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/conversation"
)

func TestStateTurnsCueWordsAndEndsOnIdle(t *testing.T) {
	clock := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	idle := func() time.Duration { return 120 * time.Second }
	state := conversation.NewState(now, idle)
	cues := []string{"explain", "tell me about"}

	if got := state.Turn("what time is it", cues); got != "" {
		t.Fatalf("plain turn = %q, want the default budget", got)
	}
	if got := state.Turn("explain gravity", cues); got != conversation.Cue {
		t.Fatalf("cue turn = %q, want %q", got, conversation.Cue)
	}
	// A cue word lifts one turn only.
	if got := state.Turn("what time is it", cues); got != "" {
		t.Fatalf("turn after a cue = %q, want the default budget", got)
	}

	state.Start()
	if !state.Active() {
		t.Fatal("mode is not active after Start")
	}
	clock = clock.Add(100 * time.Second)
	// Any request while the mode is on restarts the idle clock, not only an
	// Agent turn.
	state.Touch()
	if !state.Active() {
		t.Fatal("mode ended before the idle window after a Touch")
	}
	clock = clock.Add(60 * time.Second)
	if got := state.Turn("tell me about black holes", cues); got != conversation.Mode {
		t.Fatalf("mode turn = %q, want %q", got, conversation.Mode)
	}
	// The mode turn restarted the idle clock.
	clock = clock.Add(90 * time.Second)
	if !state.Active() {
		t.Fatal("mode ended before the idle window after a turn")
	}
	clock = clock.Add(40 * time.Second)
	if state.Active() {
		t.Fatal("mode did not end after the idle window")
	}
	if got := state.Turn("what time is it", cues); got != "" {
		t.Fatalf("turn after the idle end = %q, want the default budget", got)
	}
}

func TestStateEndTurnsTheModeOff(t *testing.T) {
	state := conversation.NewState(nil, nil)
	state.Start()
	if !state.Active() {
		t.Fatal("mode is not active after Start")
	}
	state.End()
	if state.Active() {
		t.Fatal("mode is still active after End")
	}
	if got := state.Turn("explain gravity", []string{"explain"}); got != conversation.Cue {
		t.Fatalf("cue after End = %q, want %q", got, conversation.Cue)
	}
}

func TestHasCueMatchesWholeWordsOnly(t *testing.T) {
	cues := conversation.DefaultCueWords
	cases := []struct {
		utterance string
		want      bool
	}{
		{"explain gravity", true},
		{"explain, please", true},
		{"can you explain it", true},
		{"explaining gravity is hard", false},
		{"tell me about black holes", true},
		{"tell me about it, then", true},
		{"tell me, about it", true},
		{"tell me more", true},
		{"tell me something", false},
		{"why is the sky blue", true},
		{"how does it work", true},
		{"how does it work?", true},
		{"walk me through the setup", true},
		{"walk me home", false},
		{"set a timer for ten minutes", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := conversation.HasCue(tc.utterance, cues); got != tc.want {
			t.Errorf("HasCue(%q) = %v, want %v", tc.utterance, got, tc.want)
		}
	}
}
