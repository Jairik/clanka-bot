package dashboard_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// recordingResponder counts which path answered a request.
type recordingResponder struct {
	calls int
}

func (r *recordingResponder) Ask(context.Context, tool.Request, string) (pipeline.Reply, error) {
	r.calls++
	return pipeline.Reply{Text: "ok"}, nil
}

type speakFixture struct {
	t       *testing.T
	ctx     context.Context
	handler http.Handler
	svc     *settings.Service
	silent  *recordingResponder
	spoken  *recordingResponder
}

func newSpeakFixture(t *testing.T) *speakFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "speak.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := settings.New(ctx, st, dashboard.SpeakRepliesDefinition())
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	silent, spoken := &recordingResponder{}, &recordingResponder{}
	return &speakFixture{
		t:       t,
		ctx:     ctx,
		handler: newHandler(t, dashboard.Options{Settings: svc, Typed: silent, Spoken: spoken}),
		svc:     svc,
		silent:  silent,
		spoken:  spoken,
	}
}

func (f *speakFixture) post(target string, values url.Values) *http.Request {
	f.t.Helper()
	req := request(http.MethodPost, target, owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Body = io.NopCloser(strings.NewReader(values.Encode()))
	return req
}

type sessionResp struct {
	CanSpeak, Speak bool
}

func (f *speakFixture) session() sessionResp {
	f.t.Helper()
	var v sessionResp
	rec := getJSON(f.t, f.handler, "/api/ui/session", &v)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("session = %d", rec.Code)
	}
	return v
}

func TestSpeakToggleIsOffByDefault(t *testing.T) {
	f := newSpeakFixture(t)
	if s := f.session(); !s.CanSpeak || s.Speak {
		t.Fatalf("session = %+v, want a visible toggle that is off", s)
	}
}

func TestSpeakTogglePersistsTheOptIn(t *testing.T) {
	f := newSpeakFixture(t)
	rec := serve(f.handler, f.post("/api/ui/speak", url.Values{"value": {"true"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle = %d: %s", rec.Code, rec.Body.String())
	}
	var state struct{ Speak bool }
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil || !state.Speak {
		t.Fatalf("toggle body = %s, want Speak true", rec.Body.String())
	}
	if !f.session().Speak {
		t.Error("session did not report the saved opt-in")
	}
	if !f.svc.Bool(dashboard.SpeakRepliesKey) {
		t.Error("the setting service did not store the opt-in")
	}

	// Off again.
	serve(f.handler, f.post("/api/ui/speak", url.Values{"value": {"false"}}))
	if f.session().Speak {
		t.Error("the opt-in did not turn off")
	}
}

func TestSpeakToggleRejectsAnythingButABool(t *testing.T) {
	f := newSpeakFixture(t)
	for _, value := range []string{"", "maybe", "1x"} {
		rec := serve(f.handler, f.post("/api/ui/speak", url.Values{"value": {value}}))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("value %q = %d, want 400", value, rec.Code)
		}
	}
}

func TestSpeakToggleKeepsTheSameOriginGuard(t *testing.T) {
	f := newSpeakFixture(t)
	req := f.post("/api/ui/speak", url.Values{"value": {"true"}})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := serve(f.handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin toggle = %d, want 403", rec.Code)
	}
	// It is a write route; there is no read form of it.
	if rec := serve(f.handler, request(http.MethodGet, "/api/ui/speak", owner)); rec.Code != http.StatusNotFound {
		t.Fatalf("GET toggle = %d, want 404", rec.Code)
	}
}

func TestTypedAskUsesTheSpokenSessionOnlyWhileTheOptInIsOn(t *testing.T) {
	f := newSpeakFixture(t)
	ask := func() {
		t.Helper()
		rec := serve(f.handler, f.post("/activity/ask", url.Values{"text": {"what time is it"}}))
		if rec.Code != http.StatusOK {
			t.Fatalf("ask = %d: %s", rec.Code, rec.Body.String())
		}
	}

	ask()
	if f.silent.calls != 1 || f.spoken.calls != 0 {
		t.Fatalf("off: silent = %d, spoken = %d; want 1/0", f.silent.calls, f.spoken.calls)
	}

	serve(f.handler, f.post("/api/ui/speak", url.Values{"value": {"true"}}))
	ask()
	if f.silent.calls != 1 || f.spoken.calls != 1 {
		t.Fatalf("on: silent = %d, spoken = %d; want 1/1", f.silent.calls, f.spoken.calls)
	}
}

func TestSpeakIsHiddenWithoutAWiredSpeaker(t *testing.T) {
	handler := newHandler(t, dashboard.Options{Typed: &recordingResponder{}})
	var s sessionResp
	getJSON(t, handler, "/api/ui/session", &s)
	if s.CanSpeak {
		t.Error("a deployment with no speaker still offers the toggle")
	}
}
