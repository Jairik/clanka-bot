package dashboard_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// newSpeakingVoicesFixture is the Voice page with a Pi speaker: the sound
// service is bound to a recording sink, and previews may go through a queue.
func newSpeakingVoicesFixture(t *testing.T, previews dashboard.PreviewQueue) (*voicesFixture, *soundsSink) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tts := newFakeTTS(t, "test-revision-1")
	st, err := store.Open(ctx, filepath.Join(dir, "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	voices, err := voicelib.New(ctx, st, voicelib.Config{
		Dir: filepath.Join(dir, "voices"), TTSDir: "/voices", Default: "alba", Builtins: []string{"alba"}, Logger: logger,
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	cues, err := sound.New(ctx, st, filepath.Join(dir, "sounds"), testCueFormat, logger)
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	defs := append(voices.Definitions(voicelib.DefinitionConfig{Name: "alba", Rate: 1.0}), cues.Definitions(sound.Config{})...)
	settingsSvc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	voices.Bind(settingsSvc)
	sink := newSoundsSink()
	cues.Bind(settingsSvc, sink)
	client := wyoming.NewClient(tts.Addr(), wyoming.WithTimeout(2*time.Second))
	probe := voicelib.NewTTSProbe(client, 2*time.Second)
	if err := voices.Rescan(ctx, probe); err != nil {
		t.Fatalf("initial Rescan: %v", err)
	}
	f := &voicesFixture{t: t, dir: dir, settings: settingsSvc, voices: voices, tts: tts, probe: probe}
	f.handler = newHandler(t, dashboard.Options{
		Settings:     settingsSvc,
		Voices:       voices,
		VoiceProbe:   probe,
		VoicePreview: voicesample.New(client, 2*time.Second),
		Sounds:       cues,
		PreviewQueue: previews,
	})
	return f, sink
}

func TestVoicesBrowserPreviewSpeaksTheOwnersText(t *testing.T) {
	f := newVoicesFixture(t)
	rec := f.get("/voices/audio/alba?text=" + url.QueryEscape("  Good morning, clanka.  "))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	synths := f.tts.synthCalls()
	if len(synths) != 1 || synths[0].Text != "Good morning, clanka." {
		t.Fatalf("synth calls = %+v, want the trimmed owner text", synths)
	}
}

func TestVoicesPreviewRefusesTextOverTheLimit(t *testing.T) {
	f := newVoicesFixture(t)
	long := strings.Repeat("a", voicesample.MaxTextRunes+1)
	if rec := f.get("/voices/audio/alba?text=" + long); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := len(f.tts.synthCalls()); got != 0 {
		t.Fatalf("an over-long sample reached TTS (%d calls)", got)
	}
}

func TestVoicesViewOffersThePiOnlyWithASpeaker(t *testing.T) {
	plain := newVoicesFixture(t)
	if _, body := plain.view(); !strings.Contains(body, `"CanPlayOnPi":false`) {
		t.Fatalf("view without a speaker offers the Pi: %s", body)
	}
	if rec := plain.post("/voices/speak/alba", url.Values{}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("speak without a speaker = %d, want 503", rec.Code)
	}

	f, _ := newSpeakingVoicesFixture(t, nil)
	_, body := f.view()
	for _, want := range []string{`"CanPlayOnPi":true`, `"SpeakAction":"/voices/speak/alba"`, `"SampleText":"` + voicesample.Phrase + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("view lacks %s: %s", want, body)
		}
	}
}

func TestVoicesSpeakPlaysTheSampleOnThePiWithoutChangingTheVoice(t *testing.T) {
	f, sink := newSpeakingVoicesFixture(t, nil)
	rec := f.post("/voices/speak/alba", url.Values{"text": {"Testing the kitchen speaker."}})
	if rec.Code != http.StatusOK {
		t.Fatalf("speak status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v := decodeVoices(t, rec); !strings.Contains(v.Flash, "Pi speaker") {
		t.Fatalf("flash = %q", v.Flash)
	}
	if sink.count() == 0 {
		t.Fatal("the sample never reached the Pi sink")
	}
	synths := f.tts.synthCalls()
	if len(synths) != 1 || synths[0].Text != "Testing the kitchen speaker." || synths[0].Voice != "alba" {
		t.Fatalf("synth calls = %+v", synths)
	}
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("speak changed voice.name to %q", got)
	}
}

func TestVoicesSpeakWaitsForTheReplyThroughThePreviewQueue(t *testing.T) {
	queue := announce.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), announce.WithPlayback())
	defer queue.Close()
	f, sink := newSpeakingVoicesFixture(t, queue)

	queue.TurnStarted()
	rec := f.post("/voices/speak/alba", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("speak status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v := decodeVoices(t, rec); !strings.Contains(v.Flash, "Queued") {
		t.Fatalf("flash = %q, want the queued message", v.Flash)
	}
	if synths := f.tts.synthCalls(); len(synths) != 1 || synths[0].Text != voicesample.Phrase {
		t.Fatalf("an empty sample should speak the default phrase: %+v", synths)
	}
	select {
	case <-sink.started:
		t.Fatal("the sample played during a reply")
	case <-time.After(100 * time.Millisecond):
	}
	queue.TurnFinished()
	sink.wait(t)
}

func TestVoicesSpeakRefusesAnUnknownVoiceAndLongText(t *testing.T) {
	f, sink := newSpeakingVoicesFixture(t, nil)
	if rec := f.post("/voices/speak/nobody", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown voice = %d, want 404", rec.Code)
	}
	long := strings.Repeat("a", voicesample.MaxTextRunes+1)
	rec := f.post("/voices/speak/alba", url.Values{"text": {long}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long text = %d, want 400", rec.Code)
	}
	if v := decodeVoices(t, rec); !strings.Contains(v.Error, "limited to") {
		t.Fatalf("error = %q", v.Error)
	}
	if len(f.tts.synthCalls()) != 0 || sink.count() != 0 {
		t.Fatal("a refused sample reached TTS or the speaker")
	}
}
