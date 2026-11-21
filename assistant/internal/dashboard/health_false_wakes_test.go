package dashboard_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

type falseWakesHealthView struct {
	FalseWakes []struct {
		Model      string
		Wakes      int64
		NoSpeech   int64
		Unroutable int64
		Rate       string
	}
}

func voiceWakeword() dashboard.WakewordInfo {
	return dashboard.WakewordInfo{
		Addr: "127.0.0.1:10400",
		Models: []dashboard.WakewordModel{
			{Name: "okay_nabu", Enabled: true},
			{Name: "clanker", Enabled: true},
			{Name: "stop", Command: true, Enabled: true},
		},
	}
}

// TestHealthViewFalseWakes renders the counters per wake model: numbers and a
// rate only, in config order, with the stop model skipped. A model with no
// wakes shows "unknown" rather than a fake 0% (the zero-denominator case).
func TestHealthViewFalseWakes(t *testing.T) {
	stats := voice.NewStats()
	stats.Wake("okay_nabu")
	stats.Wake("okay_nabu")
	stats.Wake("okay_nabu")
	stats.Wake("okay_nabu")
	stats.NoSpeech("okay_nabu")
	stats.Observe(latency.Completed{
		Source:     latency.SourceVoice,
		WakeWord:   "okay_nabu",
		Transcript: "music",
		Outcome:    latency.OutcomeNoAudio,
	})

	handler := newHandler(t, dashboard.Options{Wakeword: voiceWakeword(), FalseWakes: stats})
	var view falseWakesHealthView
	if rec := getJSON(t, handler, "/api/ui/health", &view); rec.Code != 200 {
		t.Fatalf("health status = %d", rec.Code)
	}
	if len(view.FalseWakes) != 2 {
		t.Fatalf("FalseWakes rows = %+v, want the two wake models only", view.FalseWakes)
	}
	nabu := view.FalseWakes[0]
	if nabu.Model != "okay_nabu" || nabu.Wakes != 4 || nabu.NoSpeech != 1 || nabu.Unroutable != 1 || nabu.Rate != "50%" {
		t.Errorf("okay_nabu row = %+v, want 4 wakes, 1+1 false, 50%%", nabu)
	}
	clanker := view.FalseWakes[1]
	if clanker.Model != "clanker" || clanker.Wakes != 0 || clanker.Rate != "unknown" {
		t.Errorf("clanker row = %+v, want zero wakes and an unknown rate", clanker)
	}
}

// TestHealthViewFalseWakesOmittedWhenVoiceOff: a disabled loop has no
// measurement to show, so the row is absent instead of an error.
func TestHealthViewFalseWakesOmittedWhenVoiceOff(t *testing.T) {
	stats := voice.NewStats()
	stats.Wake("okay_nabu")
	stats.NoSpeech("okay_nabu")

	cases := []struct {
		name string
		opts dashboard.Options
	}{
		{
			name: "no counters wired",
			opts: dashboard.Options{Wakeword: voiceWakeword()},
		},
		{
			name: "wakeword address empty",
			opts: dashboard.Options{
				Wakeword:   dashboard.WakewordInfo{Models: voiceWakeword().Models},
				FalseWakes: stats,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newHandler(t, tc.opts)
			var view falseWakesHealthView
			if rec := getJSON(t, handler, "/api/ui/health", &view); rec.Code != 200 {
				t.Fatalf("health status = %d", rec.Code)
			}
			if len(view.FalseWakes) != 0 {
				t.Fatalf("FalseWakes rows = %+v, want the row omitted", view.FalseWakes)
			}
		})
	}
}
