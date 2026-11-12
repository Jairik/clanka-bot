package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// countingResponder records how often the debug entry reached the pipeline, so
// a rejected request can be shown to have no side effect.
type countingResponder struct{ calls atomic.Int32 }

func (r *countingResponder) Ask(context.Context, tool.Request, string) (pipeline.Reply, error) {
	r.calls.Add(1)
	return pipeline.Reply{Text: "ok", RequestID: "req"}, nil
}

func debugRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:40000"
	return req
}

func do(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestDebugListenerRejectsAHostileHost(t *testing.T) {
	responder := &countingResponder{}
	handler, _ := newTestServerWith(t, api.WithResponder(responder))

	req := debugRequest(http.MethodPost, "/debug/transcript", `{"text":"what time is it"}`)
	req.Host = "attacker.example:8081"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://attacker.example:8081")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := do(handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("hostile Host status = %d, want 403", rec.Code)
	}
	if responder.calls.Load() != 0 {
		t.Fatal("a hostile Host reached the pipeline")
	}
}

func TestDebugListenerRejectsCrossSiteTextPlainSimplePost(t *testing.T) {
	responder := &countingResponder{}
	handler, _ := newTestServerWith(t, api.WithResponder(responder))

	req := debugRequest(http.MethodPost, "/debug/transcript", `{"text":"what time is it"}`)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "text/plain")
	if rec := do(handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site text/plain status = %d, want 403", rec.Code)
	}
	if responder.calls.Load() != 0 {
		t.Fatal("a cross-site simple POST reached the pipeline")
	}
}

func TestDebugListenerRequiresJSONForMutationBodies(t *testing.T) {
	responder := &countingResponder{}
	handler, _ := newTestServerWith(t, api.WithResponder(responder))

	req := debugRequest(http.MethodPost, "/debug/transcript", `{"text":"what time is it"}`)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "text/plain")
	if rec := do(handler, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain status = %d, want 415", rec.Code)
	}
	if responder.calls.Load() != 0 {
		t.Fatal("a text/plain body reached the pipeline")
	}

	// A JSON mutation with the charset parameter is still JSON.
	req = debugRequest(http.MethodPost, "/debug/transcript", `{"text":"what time is it"}`)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if rec := do(handler, req); rec.Code != http.StatusOK {
		t.Fatalf("JSON status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestDebugListenerKeepsTheDocumentedLoopbackCurl(t *testing.T) {
	responder := &countingResponder{}
	handler, _ := newTestServerWith(t, api.WithResponder(responder))

	// curl sends no Origin and no Fetch Metadata.
	req := debugRequest(http.MethodPost, "/debug/transcript", `{"text":"what time is it"}`)
	req.Header.Set("Content-Type", "application/json")
	if rec := do(handler, req); rec.Code != http.StatusOK {
		t.Fatalf("loopback curl status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestDebugListenerAnswersOptionsWithoutSideEffects(t *testing.T) {
	responder := &countingResponder{}
	handler, _ := newTestServerWith(t, api.WithResponder(responder))

	req := debugRequest(http.MethodOptions, "/debug/transcript", "")
	req.Header.Set("Origin", "https://evil.example")
	if rec := do(handler, req); rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204", rec.Code)
	}
	if rec := do(handler, debugRequest(http.MethodOptions, "/debug/announce", "")); rec.Code != http.StatusNoContent {
		t.Fatalf("announce OPTIONS status = %d, want 204", rec.Code)
	}
	if responder.calls.Load() != 0 {
		t.Fatal("OPTIONS reached the pipeline")
	}
}

func TestDebugAPISubtreeRejectsHostileHostOriginAndPlainBody(t *testing.T) {
	var calls atomic.Int32
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/settings", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"settings":[]}`))
	})
	apiMux.HandleFunc("/settings/{key}", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	handler := api.NewHandler(nil, nil, true, api.WithAPI(apiMux))

	get := debugRequest(http.MethodGet, "/api/settings", "")
	get.Host = "attacker.example:8081"
	if rec := do(handler, get); rec.Code != http.StatusForbidden {
		t.Fatalf("hostile Host API read = %d, want 403", rec.Code)
	}

	put := debugRequest(http.MethodPut, "/api/settings/focus", `{"value":"x"}`)
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("Origin", "https://evil.example")
	put.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(handler, put); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site API write = %d, want 403", rec.Code)
	}

	plain := debugRequest(http.MethodPut, "/api/settings/focus", `{"value":"x"}`)
	plain.Header.Set("Content-Type", "text/plain")
	plain.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := do(handler, plain); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain API write = %d, want 415", rec.Code)
	}

	ok := debugRequest(http.MethodPut, "/api/settings/focus", `{"value":"x"}`)
	ok.Header.Set("Content-Type", "application/json")
	if rec := do(handler, ok); rec.Code != http.StatusOK {
		t.Fatalf("loopback API write = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("API handler calls = %d, want 1", calls.Load())
	}
}

func TestDebugAnnounceRejectsHostileHostAndRequiresJSON(t *testing.T) {
	queue := newAnnounceQueue(t, newAnnounceSpeaker())
	handler, _ := newTestServerWith(t, api.WithAnnouncements(queue))

	hostile := debugRequest(http.MethodPost, "/debug/announce", `{"text":"Dinner is ready."}`)
	hostile.Host = "attacker.example"
	hostile.Header.Set("Content-Type", "application/json")
	if rec := do(handler, hostile); rec.Code != http.StatusForbidden {
		t.Fatalf("hostile Host announce = %d, want 403", rec.Code)
	}

	plain := debugRequest(http.MethodPost, "/debug/announce", `{"text":"Dinner is ready."}`)
	plain.Header.Set("Content-Type", "text/plain")
	plain.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := do(handler, plain); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain announce = %d, want 415", rec.Code)
	}

	ok := debugRequest(http.MethodPost, "/debug/announce", `{"text":"Dinner is ready."}`)
	ok.Header.Set("Content-Type", "application/json")
	if rec := do(handler, ok); rec.Code != http.StatusAccepted {
		t.Fatalf("JSON announce = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}
