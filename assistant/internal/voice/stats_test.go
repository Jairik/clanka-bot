package voice_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// TestStatsCountsBothKinds pins #299's two counters and their denominator:
// no-speech wakes come from the loop, unroutable ones from the observer
// fan-out, and a real (routable) wake increments neither false counter.
func TestStatsCountsBothKinds(t *testing.T) {
	stats := voice.NewStats()
	stats.Wake("okay_nabu")
	stats.NoSpeech("okay_nabu")
	stats.Observe(latency.Completed{
		Source:     latency.SourceVoice,
		WakeWord:   "okay_nabu",
		Transcript: "some noise",
		Outcome:    latency.OutcomeNoAudio,
	})

	got := stats.Snapshot()["okay_nabu"]
	if got.Wakes != 1 || got.NoSpeech != 1 || got.Unroutable != 1 {
		t.Fatalf("counters = %+v, want 1 wake, 1 no-speech, 1 unroutable", got)
	}

	// A real wake with a route is a wake but not a false wake.
	stats.Wake("okay_nabu")
	stats.Observe(latency.Completed{
		Source:      latency.SourceVoice,
		WakeWord:    "okay_nabu",
		Transcript:  "what time is it",
		Path:        "control",
		Outcome:     latency.OutcomeOK,
		HasResponse: true,
	})
	got = stats.Snapshot()["okay_nabu"]
	if got.Wakes != 2 || got.NoSpeech != 1 || got.Unroutable != 1 {
		t.Fatalf("after a real wake counters = %+v, want 2 wakes and the false counts unchanged", got)
	}
}

// TestStatsObserveUnroutableConditions checks the exact predicate: a voice
// completion with a transcript, no route (Tool or Path) and no reply audio.
func TestStatsObserveUnroutableConditions(t *testing.T) {
	base := latency.Completed{
		Source:     latency.SourceVoice,
		WakeWord:   "clanker",
		Transcript: "music playing",
		Outcome:    latency.OutcomeNoAudio,
	}
	unroutable := []latency.Completed{
		base,
		func() latency.Completed { c := base; c.Outcome = latency.OutcomeFailed; return c }(),
	}
	for i, c := range unroutable {
		stats := voice.NewStats()
		stats.Observe(c)
		if got := stats.Snapshot()["clanker"].Unroutable; got != 1 {
			t.Errorf("case %d: unroutable = %d, want 1", i, got)
		}
	}

	ignored := []latency.Completed{
		func() latency.Completed { c := base; c.Path = "agent"; return c }(),
		func() latency.Completed { c := base; c.Tool = "weather"; return c }(),
		func() latency.Completed { c := base; c.HasResponse = true; return c }(),
		func() latency.Completed { c := base; c.Source = latency.SourceDebug; return c }(),
		func() latency.Completed { c := base; c.Transcript = "   "; return c }(),
		func() latency.Completed { c := base; c.WakeWord = ""; return c }(),
	}
	for i, c := range ignored {
		stats := voice.NewStats()
		stats.Observe(c)
		if got := stats.Snapshot(); len(got) != 0 {
			t.Errorf("case %d: ignored completion created counters %+v", i, got)
		}
	}
}

// TestStatsRate handles the zero denominator: an unknown rate, never 0%.
func TestStatsRate(t *testing.T) {
	cases := []struct {
		name string
		c    voice.ModelFalseWakes
		want string
	}{
		{"no wakes is unknown", voice.ModelFalseWakes{}, "unknown"},
		{"no-speech without a wake is still unknown", voice.ModelFalseWakes{NoSpeech: 3}, "unknown"},
		{"one of four is 25 percent", voice.ModelFalseWakes{Wakes: 4, NoSpeech: 1}, "25%"},
		{"two false of two is 100 percent", voice.ModelFalseWakes{Wakes: 2, NoSpeech: 1, Unroutable: 1}, "100%"},
	}
	for _, tc := range cases {
		if got := tc.c.Rate(); got != tc.want {
			t.Errorf("%s: Rate() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStatsReset shows the counters are process-local: a fresh Stats (as after
// a restart) starts empty.
func TestStatsReset(t *testing.T) {
	stats := voice.NewStats()
	stats.Wake("okay_nabu")
	stats.NoSpeech("okay_nabu")
	if got := stats.Snapshot()["okay_nabu"].NoSpeech; got != 1 {
		t.Fatalf("before reset no-speech = %d, want 1", got)
	}
	fresh := voice.NewStats()
	if got := fresh.Snapshot(); len(got) != 0 {
		t.Fatalf("a fresh Stats is not empty: %+v", got)
	}
}

// TestStatsNilIsSafe lets a loop with no Stats wired keep working unchanged.
func TestStatsNilIsSafe(t *testing.T) {
	var stats *voice.Stats
	stats.Wake("m")
	stats.NoSpeech("m")
	stats.Observe(latency.Completed{Source: latency.SourceVoice})
	if got := stats.Snapshot(); got != nil {
		t.Fatalf("nil Stats snapshot = %+v, want nil", got)
	}
}
