package dashboard_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// replyAudioFixture reuses the Voice page's real library and fake TTS, then adds
// the Activity store the chat reads. The handler is separate so tests can leave
// the other one free of Activity when checking the unwired cases.
type replyAudioFixture struct {
	t       *testing.T
	handler *dashboard.Handler
	base    *voicesFixture
	store   *activity.Store
}

func newReplyAudioFixture(t *testing.T) *replyAudioFixture {
	t.Helper()
	base := newVoicesFixture(t)
	client := wyoming.NewClient(base.tts.Addr(), wyoming.WithTimeout(2*time.Second))
	store := activity.New(activity.DefaultLimit)
	handler := newHandler(t, dashboard.Options{
		Settings:     base.settings,
		Voices:       base.voices,
		VoiceProbe:   base.probe,
		VoicePreview: voicesample.New(client, 2*time.Second),
		Activity:     store,
	})
	return &replyAudioFixture{t: t, handler: handler, base: base, store: store}
}

// seedReply puts one completed request with its answer in the Activity ring.
func (f *replyAudioFixture) seedReply(id, reply string, localOnly bool) {
	f.t.Helper()
	f.store.Observe(latency.Completed{
		ID:         id,
		Transcript: "the question",
		Path:       "agent_path",
		Detail:     &latency.Detail{Reply: reply, LocalOnly: localOnly},
	})
}

func TestReplyAudioServesTheReplyAsWAVInTheActiveVoice(t *testing.T) {
	f := newReplyAudioFixture(t)
	if rec := f.base.post("/voices/rate", url.Values{"value": {"1.25"}}); rec.Code != http.StatusOK {
		t.Fatalf("rate = %d, body = %s", rec.Code, rec.Body.String())
	}
	// A Local-only answer says the words stay on the Pi; reading them into the
	// owner's own browser is exactly what this endpoint is for.
	f.seedReply("req-1", "  The answer. \n", true)

	rec := serve(f.handler, request(http.MethodGet, "/reply/audio/req-1", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "inline") || !strings.Contains(disposition, "filename=reply.wav") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	body := rec.Body.Bytes()
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("reply audio is not a WAV (%d bytes)", len(body))
	}
	calls := f.base.tts.synthCalls()
	if len(calls) != 1 {
		t.Fatalf("synth calls = %v", calls)
	}
	if calls[0].Text != "The answer." {
		t.Fatalf("text = %q, want the stored reply trimmed", calls[0].Text)
	}
	if calls[0].Voice != "alba" || calls[0].Rate != 1.25 {
		t.Fatalf("voice/rate = %q/%v, want the active voice and its rate", calls[0].Voice, calls[0].Rate)
	}
	if got := f.base.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("the endpoint changed voice.name to %q", got)
	}
}

func TestReplyAudioCarriesOnlyTheIDInThePath(t *testing.T) {
	f := newReplyAudioFixture(t)
	reply := "A reply with & symbols and spaces"
	f.seedReply("req-1", reply, false)

	// The id is a path segment: no reply text, token or voice/rate in a query.
	target := "/reply/audio/req-1"
	req := request(http.MethodGet, target, owner)
	if req.URL.RawQuery != "" || req.URL.Path != target {
		t.Fatalf("request = %q, want the id in the path and no query", target)
	}
	rec := serve(f.handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The words came from the store, not from anything the caller sent.
	if calls := f.base.tts.synthCalls(); len(calls) != 1 || calls[0].Text != reply {
		t.Fatalf("synth calls = %v, want the stored reply", calls)
	}
}

func TestReplyAudioRefusesBadRequestsBeforeTTS(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		reply  string
		seed   bool
		status int
	}{
		{name: "blank id", id: "", status: http.StatusBadRequest},
		{name: "unknown id", id: "missing", status: http.StatusNotFound},
		{name: "blank reply", id: "req-1", reply: "  \n ", seed: true, status: http.StatusBadRequest},
		{name: "over the rune bound", id: "req-1", reply: strings.Repeat("a", voicesample.MaxPreviewRunes+1), seed: true, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplyAudioFixture(t)
			if tc.seed {
				f.seedReply(tc.id, tc.reply, false)
			}
			target := "/reply/audio/" + tc.id
			rec := serve(f.handler, request(http.MethodGet, target, owner))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if calls := f.base.tts.synthCalls(); len(calls) != 0 {
				t.Fatalf("a refused reply reached TTS: %v", calls)
			}
			if body := rec.Body.String(); strings.TrimSpace(body) == "" {
				t.Fatal("a refused reply gave the row no line to show")
			}
		})
	}
}

func TestReplyAudioIsNotFoundWhenItCannotSynthesize(t *testing.T) {
	f := newReplyAudioFixture(t)
	f.seedReply("req-1", "hello", false)
	client := wyoming.NewClient(f.base.tts.Addr(), wyoming.WithTimeout(2*time.Second))
	preview := voicesample.New(client, 2*time.Second)

	cases := []struct {
		name    string
		handler http.Handler
	}{
		{"no activity", newHandler(t, dashboard.Options{Settings: f.base.settings, Voices: f.base.voices, VoicePreview: preview})},
		{"no voices", newHandler(t, dashboard.Options{Settings: f.base.settings, Activity: f.store, VoicePreview: preview})},
		{"no TTS sampler", newHandler(t, dashboard.Options{Settings: f.base.settings, Voices: f.base.voices, Activity: f.store})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(tc.handler, request(http.MethodGet, "/reply/audio/req-1", owner))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestReplyAudioRequiresTheOwnerAndAllowsOnlyReads(t *testing.T) {
	f := newReplyAudioFixture(t)
	f.seedReply("req-1", "hello", false)
	if rec := serve(f.handler, request(http.MethodGet, "/reply/audio/req-1", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if rec := serve(f.handler, request(http.MethodPost, "/reply/audio/req-1", owner)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestReplyAudioReportsAServiceFailure(t *testing.T) {
	f := newReplyAudioFixture(t)
	f.seedReply("req-1", "hello", false)
	f.base.tts.setSynthFailure(true)
	rec := serve(f.handler, request(http.MethodGet, "/reply/audio/req-1", owner))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

type replySession struct {
	CanSpeak       bool
	CanPlayReplies bool
}

func replySessionFor(t *testing.T, handler *dashboard.Handler) replySession {
	t.Helper()
	var v replySession
	rec := getJSON(t, handler, "/api/ui/session", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d, body = %s", rec.Code, rec.Body.String())
	}
	return v
}

func TestSessionReportsBrowserPlaybackSeparatelyFromThePiSpeaker(t *testing.T) {
	f := newReplyAudioFixture(t)
	if s := replySessionFor(t, f.handler); !s.CanPlayReplies {
		t.Fatalf("wired session = %+v, want browser playback", s)
	}

	// A Pi speaker alone is not a TTS service: the button must stay hidden even
	// though the top-bar speak toggle is available.
	speakerOnly := newHandler(t, dashboard.Options{
		Settings: f.base.settings,
		Spoken:   &micTyped{},
	})
	if s := replySessionFor(t, speakerOnly); !s.CanSpeak || s.CanPlayReplies {
		t.Fatalf("speaker-only session = %+v, want CanSpeak but no playback", s)
	}
	if s := replySessionFor(t, newHandler(t, dashboard.Options{})); s.CanSpeak || s.CanPlayReplies {
		t.Fatalf("bare session = %+v, want neither", s)
	}

	// Confirm the endpoint agrees with the flag when TTS is missing.
	rec := serve(speakerOnly, request(http.MethodGet, "/reply/audio/req-1", owner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("speaker-only reply audio = %d, want 404", rec.Code)
	}
}
