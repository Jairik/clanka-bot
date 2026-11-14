package dashboard_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

// fakeVolume is an in-memory speaker that records every write, so a test can
// prove a rejected request wrote nothing.
type fakeVolume struct {
	mu      sync.Mutex
	percent int
	muted   bool
	readErr error
	writes  []string
}

func (v *fakeVolume) Volume(context.Context) (audio.VolumeState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.readErr != nil {
		return audio.VolumeState{}, v.readErr
	}
	return audio.VolumeState{Percent: v.percent, Muted: v.muted}, nil
}

func (v *fakeVolume) SetVolume(_ context.Context, percent int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.writes = append(v.writes, "volume")
	v.percent = percent
	return nil
}

func (v *fakeVolume) SetMute(_ context.Context, muted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.writes = append(v.writes, "mute")
	v.muted = muted
	return nil
}

func (v *fakeVolume) state() (int, bool, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.percent, v.muted, len(v.writes)
}

type volumeResp struct {
	Available bool
	Percent   int
	Muted     bool
	Error     string
}

func postVolume(handler http.Handler, values url.Values, edit func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/ui/volume", strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if edit != nil {
		edit(req)
	}
	return serve(handler, req)
}

func TestVolumeReadReportsTheSpeaker(t *testing.T) {
	speaker := &fakeVolume{percent: 42, muted: true}
	handler := newHandler(t, dashboard.Options{Volume: speaker})
	var v volumeResp
	rec := getJSON(t, handler, "/api/ui/volume", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !v.Available || v.Percent != 42 || !v.Muted || v.Error != "" {
		t.Errorf("view = %+v, want available 42%% muted", v)
	}
}

func TestVolumeSetPercentSetsTheLevelAndUnmutes(t *testing.T) {
	speaker := &fakeVolume{percent: 10, muted: true}
	handler := newHandler(t, dashboard.Options{Volume: speaker})
	rec := postVolume(handler, url.Values{"percent": {"35"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var v volumeResp
	decodeJSON(t, rec, &v)
	if !v.Available || v.Percent != 35 || v.Muted || v.Error != "" {
		t.Errorf("view = %+v, want 35%% unmuted", v)
	}
	if percent, muted, _ := speaker.state(); percent != 35 || muted {
		t.Errorf("speaker = %d%% muted=%v", percent, muted)
	}
}

func TestVolumeMutedFlagMutesWithoutChangingTheLevel(t *testing.T) {
	speaker := &fakeVolume{percent: 60}
	handler := newHandler(t, dashboard.Options{Volume: speaker})
	rec := postVolume(handler, url.Values{"muted": {"true"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var v volumeResp
	decodeJSON(t, rec, &v)
	if !v.Muted || v.Percent != 60 {
		t.Errorf("view = %+v, want muted at the unchanged 60%%", v)
	}
	if percent, muted, writes := speaker.state(); percent != 60 || !muted || writes != 1 {
		t.Errorf("speaker = %d%% muted=%v after %d writes, want only the mute written", percent, muted, writes)
	}

	// Unmuting through the flag also leaves the level alone.
	rec = postVolume(handler, url.Values{"muted": {"false"}}, nil)
	decodeJSON(t, rec, &v)
	if v.Muted || v.Percent != 60 {
		t.Errorf("after unmute view = %+v", v)
	}
}

func TestVolumeExplicitMuteWinsOverTheLevelsImplicitUnmute(t *testing.T) {
	speaker := &fakeVolume{percent: 10}
	handler := newHandler(t, dashboard.Options{Volume: speaker})
	rec := postVolume(handler, url.Values{"percent": {"50"}, "muted": {"true"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if percent, muted, _ := speaker.state(); percent != 50 || !muted {
		t.Errorf("speaker = %d%% muted=%v, want 50%% muted", percent, muted)
	}
}

func TestVolumeRejectsInvalidInputWithoutWriting(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values url.Values
		want   string
	}{
		{"negative", url.Values{"percent": {"-1"}}, "0 to 100"},
		{"over 100", url.Values{"percent": {"101"}}, "0 to 100"},
		{"not a number", url.Values{"percent": {"x"}}, "0 to 100"},
		{"bad muted", url.Values{"muted": {"maybe"}}, "true or false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			speaker := &fakeVolume{percent: 20}
			handler := newHandler(t, dashboard.Options{Volume: speaker})
			rec := postVolume(handler, tt.values, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var v volumeResp
			decodeJSON(t, rec, &v)
			if !strings.Contains(v.Error, tt.want) {
				t.Errorf("error = %q, want it to mention %q", v.Error, tt.want)
			}
			if !v.Available || v.Percent != 20 {
				t.Errorf("error view = %+v, want the current state", v)
			}
			if _, _, writes := speaker.state(); writes != 0 {
				t.Errorf("a rejected request wrote to the speaker %d times", writes)
			}
		})
	}
}

func TestVolumeWriteRequiresSameOrigin(t *testing.T) {
	speaker := &fakeVolume{percent: 20}
	handler := newHandler(t, dashboard.Options{Volume: speaker})
	rec := postVolume(handler, url.Values{"percent": {"80"}}, func(r *http.Request) {
		r.Header.Del("Sec-Fetch-Site")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no origin status = %d, want 403", rec.Code)
	}
	rec = postVolume(handler, url.Values{"percent": {"80"}}, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}
	if _, _, writes := speaker.state(); writes != 0 {
		t.Errorf("a forbidden request wrote to the speaker %d times", writes)
	}
}

func TestVolumeWithoutAControlIsUnavailable(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	var v volumeResp
	rec := getJSON(t, handler, "/api/ui/volume", &v)
	if rec.Code != http.StatusOK || v.Available {
		t.Fatalf("GET = %d %+v, want 200 with Available=false", rec.Code, v)
	}
	if rec := postVolume(handler, url.Values{"percent": {"50"}}, nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST = %d, want 404", rec.Code)
	}
}

func TestVolumeFailedReadHidesTheControl(t *testing.T) {
	speaker := &fakeVolume{readErr: errors.New("pactl: connection refused")}
	handler := newHandler(t, dashboard.Options{Volume: speaker})
	var v volumeResp
	rec := getJSON(t, handler, "/api/ui/volume", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if v.Available || v.Error == "" {
		t.Errorf("view = %+v, want unavailable with a message", v)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Error("the Pulse error text leaked to the client")
	}
}
