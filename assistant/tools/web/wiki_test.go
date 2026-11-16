package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
)

// wikiReg builds a registry with wiki.summary pointed at a fake origin.
func wikiReg(t *testing.T, baseURL string) *tool.Registry {
	t.Helper()
	cfg := web.DefaultWikiConfig()
	cfg.BaseURL = baseURL
	reg := tool.NewRegistry(5 * time.Second)
	if err := tool.Add(reg, web.NewWiki(cfg, &http.Client{Timeout: 3 * time.Second}, nil, nil)); err != nil {
		t.Fatalf("add wiki.summary: %v", err)
	}
	return reg
}

// wikiRun executes wiki.summary for one topic and returns the result or error.
func wikiRun(t *testing.T, reg *tool.Registry, topic string) (web.WikiOut, error) {
	t.Helper()
	args, err := json.Marshal(web.WikiIn{Topic: topic})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := reg.Execute(context.Background(), web.WikiName, args)
	if err != nil {
		return web.WikiOut{}, err
	}
	out, ok := res.(web.WikiOut)
	if !ok {
		t.Fatalf("result type %T, want WikiOut", res)
	}
	return out, nil
}

// sampleSummaryHandler returns one normal page for any topic.
func sampleSummaryHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/search/title"):
			fmt.Fprint(w, `{"pages":[{"title":"Capybara"}]}`)
		case strings.Contains(r.URL.Path, "/page/summary/"):
			fmt.Fprint(w, `{"title":"Capybara","extract":"The capybara is a mammal native to South America. It is the largest living rodent.","type":"standard","content_urls":{"desktop":{"page":"https://en.wikipedia.org/wiki/Capybara"}}}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestWikiNormalPage(t *testing.T) {
	srv := httptest.NewServer(sampleSummaryHandler())
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	out, err := wikiRun(t, reg, "capybara")
	if err != nil {
		t.Fatalf("normal page: %v", err)
	}
	if out.Title != "Capybara" {
		t.Errorf("title = %q, want Capybara", out.Title)
	}
	if !strings.Contains(out.Extract, "South America") {
		t.Errorf("extract = %q, want it to contain the summary", out.Extract)
	}
	if out.URL != "https://en.wikipedia.org/wiki/Capybara" {
		t.Errorf("url = %q, want the canonical page URL", out.URL)
	}
}

func TestWikiCachesSummary(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		sampleSummaryHandler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	if _, err := wikiRun(t, reg, "capybara"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := wikiRun(t, reg, "capybara"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if hits == 0 || hits > 2 {
		t.Errorf("expected the cache to skip the second upstream call, got %d hits", hits)
	}
}

func TestWikiDisambiguation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/search/title") {
			fmt.Fprint(w, `{"pages":[{"title":"Mercury"}]}`)
			return
		}
		fmt.Fprint(w, `{"title":"Mercury","extract":"","type":"disambiguation","content_urls":{"desktop":{"page":"https://en.wikipedia.org/wiki/Mercury"}}}`)
	}))
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	_, err := wikiRun(t, reg, "mercury")
	if err == nil {
		t.Fatal("expected a disambiguation refusal, got nil")
	}
	if got := tool.FailureMessage(err); got != "That could mean several things. Can you be more specific?" {
		t.Errorf("refusal = %q, want the exact disambiguation line", got)
	}
}

func TestWikiMissingPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/search/title") {
			fmt.Fprint(w, `{"pages":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	_, err := wikiRun(t, reg, "nonexistent topic")
	if err == nil {
		t.Fatal("expected a missing-page refusal, got nil")
	}
	got := tool.FailureMessage(err)
	if !strings.Contains(got, "nonexistent topic") {
		t.Errorf("missing refusal = %q, want it to name the topic", got)
	}
}

func TestWikiMissingPageSummary404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/search/title") {
			fmt.Fprint(w, `{"pages":[{"title":"No Such Page"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	_, err := wikiRun(t, reg, "made up entry")
	if err == nil {
		t.Fatal("expected a missing-page refusal, got nil")
	}
	if got := tool.FailureMessage(err); !strings.Contains(got, "made up entry") {
		t.Errorf("missing refusal = %q, want it to name the topic", got)
	}
}

func TestWikiSlowUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprint(w, `{"pages":[{"title":"Capybara"}]}`)
	}))
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	start := time.Now()
	_, err := wikiRun(t, reg, "capybara")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout refusal, got nil")
	}
	if got := tool.FailureMessage(err); got != "I couldn't reach Wikipedia right now." {
		t.Errorf("timeout refusal = %q, want the unavailable line", got)
	}
	if elapsed > 3*time.Second {
		t.Errorf("2.5 s budget was not enforced: handler took %v", elapsed)
	}
}

func TestWikiOversizeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/search/title") {
			fmt.Fprint(w, `{"pages":[{"title":"Big"}]}`)
			return
		}
		// Body larger than the 256 KiB limit per call.
		fmt.Fprint(w, `{"title":"Big","extract":"`)
		for i := 0; i < (256<<10)+100; i++ {
			fmt.Fprint(w, "x")
		}
		fmt.Fprint(w, `","type":"standard"}`)
	}))
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	_, err := wikiRun(t, reg, "big")
	if err == nil {
		t.Fatal("expected an oversize refusal, got nil")
	}
}

func TestWikiEmptyTopic(t *testing.T) {
	srv := httptest.NewServer(sampleSummaryHandler())
	defer srv.Close()
	reg := wikiReg(t, srv.URL)

	_, err := wikiRun(t, reg, "   ")
	if err == nil {
		t.Fatal("expected an empty-topic refusal, got nil")
	}
	if got := tool.FailureMessage(err); got != "What should I look up?" {
		t.Errorf("empty-topic refusal = %q, want the prompt", got)
	}
}
