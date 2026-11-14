package dashboard_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// wordingPreview requests the Settings wording preview as the owner.
func wordingPreview(t *testing.T, handler http.Handler, text string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(handler, request(http.MethodGet, "/settings/wording/preview?text="+url.QueryEscape(text), owner))
}

func TestWordingPreviewServesWAVInTheActiveVoice(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.post("/voices/rate", url.Values{"value": {"1.25"}}); rec.Code != http.StatusOK {
		t.Fatalf("rate = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := wordingPreview(t, f.handler, "  One moment.\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "audio/wav" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "inline") || !strings.Contains(disposition, "filename=wording-preview.wav") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	body := rec.Body.Bytes()
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("preview is not a WAV (%d bytes)", len(body))
	}
	if length := rec.Header().Get("Content-Length"); length == "" || length == "0" {
		t.Fatalf("Content-Length = %q, want the served size", length)
	}
	calls := f.tts.synthCalls()
	if len(calls) != 1 {
		t.Fatalf("synth calls = %v", calls)
	}
	if calls[0].Text != "One moment." {
		t.Fatalf("text = %q, want the trimmed draft", calls[0].Text)
	}
	if calls[0].Voice != "alba" || calls[0].Rate != 1.25 {
		t.Fatalf("voice/rate = %q/%v, want the active voice and its rate", calls[0].Voice, calls[0].Rate)
	}
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("the preview changed voice.name to %q", got)
	}
}

func TestWordingPreviewFallsBackToTheDefaultVoiceWhenActiveIsStale(t *testing.T) {
	f := newVoicesFixture(t)
	if rec := f.upload("jarvis.safetensors", stateBytes(t, 2, true), false); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.post("/voices/active/jarvis", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("select = %d, body = %s", rec.Code, rec.Body.String())
	}
	f.tts.setRevision("test-revision-2")
	if err := f.voices.Rescan(t.Context(), f.probe); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, _, ok := f.voices.ActiveSpeech(); ok {
		t.Fatal("fixture did not make the active voice stale")
	}

	rec := wordingPreview(t, f.handler, "Still speaks.")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := f.tts.synthCalls()
	if len(calls) != 1 || calls[0].Voice != "alba" {
		t.Fatalf("synth calls = %v, want the default built-in fallback", calls)
	}
	// The Settings row must name the voice the endpoint actually uses, not the
	// stale active entry.
	var view settingsResp
	if settingsRec := getJSON(t, f.handler, "/api/ui/settings", &view); settingsRec.Code != http.StatusOK {
		t.Fatalf("settings = %d, body = %s", settingsRec.Code, settingsRec.Body.String())
	}
	if view.Voice != "alba" {
		t.Fatalf("the row label names %q, want the default voice", view.Voice)
	}
	if calls[0].Voice != view.Voice {
		t.Fatalf("label %q disagrees with the voice the endpoint requested %q", view.Voice, calls[0].Voice)
	}
	if got := f.settings.String(voicelib.KeyName); got != "jarvis" {
		t.Fatalf("the fallback rewrote voice.name to %q", got)
	}
}

func TestWordingPreviewRejectsBlankAndOverLongTextBeforeTTS(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"whitespace", "  \n\t "},
		{"over the rune bound", strings.Repeat("a", voicesample.MaxPreviewRunes+1)},
	}
	f := newVoicesFixture(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.tts.synthCalls())
			rec := wordingPreview(t, f.handler, tc.text)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got := len(f.tts.synthCalls()); got != before {
				t.Fatalf("a rejected draft reached TTS (%d -> %d calls)", before, got)
			}
			if body := rec.Body.String(); strings.Contains(body, "502") || !strings.Contains(body, "preview") {
				t.Fatalf("body = %q, want a short preview message", body)
			}
		})
	}
}

func TestWordingPreviewIsNotFoundWhenUnwired(t *testing.T) {
	f := newVoicesFixture(t)
	cases := []struct {
		name    string
		handler http.Handler
	}{
		{"no voices", newHandler(t, dashboard.Options{Settings: f.settings})},
		{"no preview sampler", newHandler(t, dashboard.Options{
			Settings: f.settings, Voices: f.voices, VoiceProbe: f.probe,
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := wordingPreview(t, tc.handler, "hello"); rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestWordingPreviewRequiresTheOwner(t *testing.T) {
	f := newVoicesFixture(t)
	rec := serve(f.handler, request(http.MethodGet, "/settings/wording/preview?text=hello", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestWordingPreviewReportsAServiceFailure(t *testing.T) {
	f := newVoicesFixture(t)
	f.tts.setSynthFailure(true)
	rec := wordingPreview(t, f.handler, "hello")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if synth := f.tts.synthCalls(); len(synth) != 1 {
		t.Fatalf("synth calls = %v", synth)
	}
}

func TestSettingsViewReportsThePreviewVoiceAndBound(t *testing.T) {
	t.Run("voices wired", func(t *testing.T) {
		f := newVoicesFixture(t)
		var v settingsResp
		rec := getJSON(t, f.handler, "/api/ui/settings", &v)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if v.Voice != "alba" || !v.CanPreview || v.PreviewMaxRunes != voicesample.MaxPreviewRunes {
			t.Fatalf("voice = %q, canPreview = %v, max = %d", v.Voice, v.CanPreview, v.PreviewMaxRunes)
		}
	})

	t.Run("voices unwired", func(t *testing.T) {
		f := newSettingsFixture(t)
		v, _ := f.view()
		if v.Voice != "" || v.CanPreview || v.PreviewMaxRunes != 0 {
			t.Fatalf("voice = %q, canPreview = %v, max = %d, want the unwired zero values", v.Voice, v.CanPreview, v.PreviewMaxRunes)
		}
	})
}

// allSettings flattens every group the Settings view renders.
func allSettings(v settingsResp) []settingRow {
	rows := append([]settingRow{}, v.Agent...)
	rows = append(rows, v.Quiet...)
	rows = append(rows, v.Live...)
	rows = append(rows, v.Restart...)
	for _, w := range v.ToolWording {
		if w.Control != nil {
			rows = append(rows, *w.Control)
		}
		if w.Examples != nil {
			rows = append(rows, *w.Examples)
		}
	}
	return rows
}

func TestSettingsAgentRowsCarryThePreviewContract(t *testing.T) {
	f := newSettingsFixture(t)
	v, _ := f.view()
	want := map[string]struct{ action, kind string }{
		wording.FillerKey:      {"/settings/wording/preview", "spoken"},
		wording.UnavailableKey: {"/settings/wording/preview", "spoken"},
		wording.PrefixesKey:    {"", "match"},
	}
	for key, expected := range want {
		row := v.mustSetting(t, key)
		if row.PreviewAction != expected.action || row.PreviewKind != expected.kind {
			t.Errorf("%s preview = %q/%q, want %q/%q", key, row.PreviewAction, row.PreviewKind, expected.action, expected.kind)
		}
	}
	for _, row := range allSettings(v) {
		if _, ok := want[row.Key]; ok {
			continue
		}
		if row.PreviewAction != "" || row.PreviewKind != "" {
			t.Errorf("%s carries a preview contract: %q/%q", row.Key, row.PreviewAction, row.PreviewKind)
		}
	}
}
