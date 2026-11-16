package pipeline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Expiry applies before an event, before dispatch and before listing.
func TestDeferredExpiry(t *testing.T) {
	q := &DeferredQueue{actions: []DeferredAction{{ID: 1, Event: "timer", Expires: time.Now().Add(-time.Second)}}}
	q.Trigger("timer")
	if _, ok := q.NextReady(); ok {
		t.Fatal("expired action dispatched")
	}
	if len(q.List()) != 0 {
		t.Fatal("expired action visible")
	}
}

// The Dashboard reads items and the server-side cap from the same view (#297).
func TestDeferredView(t *testing.T) {
	q := &DeferredQueue{}
	q.Add("music", "turn up the volume")

	rec := httptest.NewRecorder()
	q.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/deferred", nil))
	var view struct {
		Items []DeferredAction `json:"items"`
		Limit int              `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode GET body %q: %v", rec.Body.String(), err)
	}
	if view.Limit != deferredLimit {
		t.Fatalf("limit = %d, want %d", view.Limit, deferredLimit)
	}
	if len(view.Items) != 1 || view.Items[0].Text != "turn up the volume" || view.Items[0].Event != "music" || view.Items[0].Expires.IsZero() {
		t.Fatalf("items = %+v, want the queued action with its event and expiry", view.Items)
	}

	// Cancellation stays a form POST; it simply answers the same view.
	post := httptest.NewRequest(http.MethodPost, "/api/deferred", strings.NewReader(url.Values{"id": {"1"}}.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	q.ServeHTTP(rec, post)
	view = struct {
		Items []DeferredAction `json:"items"`
		Limit int              `json:"limit"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode POST body %q: %v", rec.Body.String(), err)
	}
	if len(view.Items) != 0 || view.Limit != deferredLimit {
		t.Fatalf("after cancel: %+v, want no items and limit %d", view, deferredLimit)
	}
}

func TestDeferredLimit(t *testing.T) {
	q := &DeferredQueue{}
	for i := 0; i < deferredLimit; i++ {
		if !q.Add("reply", "action") {
			t.Fatalf("add %d refused before the cap", i+1)
		}
	}
	if q.Add("reply", "overflow") {
		t.Fatal("accepted an action past the cap")
	}
}
