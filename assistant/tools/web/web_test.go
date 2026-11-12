package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
	"gopkg.in/yaml.v3"
)

// configure decodes one tools.web section exactly as the module list does, so
// the tests exercise the typed config and backend switch rather than a
// constructor shortcut.
func configure(t *testing.T, values map[string]any) *web.Module {
	t.Helper()
	module := web.NewModule()
	var node yaml.Node
	if err := node.Encode(values); err != nil {
		t.Fatalf("encode tools.web: %v", err)
	}
	if err := module.Configure(&node); err != nil {
		t.Fatalf("configure tools.web: %v", err)
	}
	return module
}

func register(t *testing.T, module *web.Module) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(5 * time.Second)
	if err := module.Register(reg, tool.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("register web: %v", err)
	}
	return reg
}

func execute(ctx context.Context, t *testing.T, reg *tool.Registry, args string) (web.Out, error) {
	t.Helper()
	result, err := reg.Execute(ctx, web.Name, json.RawMessage(args))
	if err != nil {
		return web.Out{}, err
	}
	out, ok := result.(web.Out)
	if !ok {
		t.Fatalf("Execute returned %T, want web.Out", result)
	}
	return out, nil
}

func TestSearchReturnsShortBoundedResults(t *testing.T) {
	longTitle := strings.Repeat("T", 300)
	longSnippet := strings.Repeat("s", 300)
	var gotQuery, gotFormat, gotCategories string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("q")
		gotFormat = r.URL.Query().Get("format")
		gotCategories = r.URL.Query().Get("categories")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"results":[
			{"title":"City win 2-1","url":"https://example.test/report","content":" City   won 2-1.\nSecond line."},
			{"title":%q,"url":"https://example.test/long","content":%q},
			{"title":"relative","url":"/local","content":"skip this"},
			{"title":"script","url":"javascript:alert(1)","content":"skip this too"},
			{"title":"missing url","content":"no link"},
			{"title":"oversize url","url":"https://example.test/%s","content":"skip this as well"}
		]}`, longTitle, longSnippet, strings.Repeat("u", 600))
	}))
	defer server.Close()

	module := configure(t, map[string]any{
		"backend":  "searxng",
		"base_url": server.URL,
		"timeout":  "1s",
	})
	reg := register(t, module)
	out, err := execute(context.Background(), t, reg, `{"query":"who won the match last night","max_results":5}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotQuery != "who won the match last night" || gotFormat != "json" || gotCategories != "general" {
		t.Fatalf("upstream query = %q format=%q categories=%q", gotQuery, gotFormat, gotCategories)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %+v, want the two usable hits", out.Results)
	}
	first := out.Results[0]
	if first.Title != "City win 2-1" || first.URL != "https://example.test/report" {
		t.Fatalf("first result = %+v", first)
	}
	if first.Snippet != "City won 2-1. Second line." {
		t.Fatalf("snippet = %q, want one collapsed line", first.Snippet)
	}
	if len([]rune(out.Results[1].Title)) != 161 || len([]rune(out.Results[1].Snippet)) != 241 {
		t.Fatalf("long fields were not truncated: title=%d snippet=%d runes",
			len([]rune(out.Results[1].Title)), len([]rune(out.Results[1].Snippet)))
	}
}

func TestSearchClampsResultsAndQuery(t *testing.T) {
	var gotQuery string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.Query().Get("q")
		mu.Unlock()
		var hits []string
		for i := 0; i < 30; i++ {
			hits = append(hits, fmt.Sprintf(`{"title":"hit %d","url":"https://example.test/%d","content":"snippet %d"}`, i, i, i))
		}
		fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(hits, ","))
	}))
	defer server.Close()

	module := configure(t, map[string]any{
		"backend":     "searxng",
		"base_url":    server.URL,
		"results":     5,
		"max_results": 10,
	})
	reg := register(t, module)
	out, err := execute(context.Background(), t, reg, `{"query":"`+strings.Repeat("x", 500)+`","max_results":50}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.Results) != 10 {
		t.Fatalf("results = %d, want the configured cap 10", len(out.Results))
	}
	mu.Lock()
	defer mu.Unlock()
	if len([]rune(gotQuery)) != 301 {
		t.Fatalf("upstream query length = %d, want 300 runes plus an ellipsis", len([]rune(gotQuery)))
	}
}

func TestSearchEmptyResultsIsASuccessfulEmptyPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer server.Close()

	reg := register(t, configure(t, map[string]any{"base_url": server.URL, "timeout": "1s"}))
	out, err := execute(context.Background(), t, reg, `{"query":"no such thing"}`)
	if err != nil {
		t.Fatalf("empty search failed: %v", err)
	}
	if out.Results == nil || len(out.Results) != 0 {
		t.Fatalf("results = %#v, want an empty non-nil slice", out.Results)
	}
}

func TestSearchFailuresAreCleanAndSayItCouldNotSearch(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		timeout string
	}{
		{
			name: "invalid JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"results":`)
			},
			timeout: "1s",
		},
		{
			name: "upstream error status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "format json is disabled", http.StatusForbidden)
			},
			timeout: "1s",
		},
		{
			name: "oversize body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, strings.Repeat("x", 600<<10))
			},
			timeout: "2s",
		},
		{
			name: "timeout",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(250 * time.Millisecond)
				fmt.Fprint(w, `{"results":[]}`)
			},
			timeout: "30ms",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			reg := register(t, configure(t, map[string]any{
				"base_url": server.URL,
				"timeout":  tt.timeout,
			}))
			_, err := execute(context.Background(), t, reg, `{"query":"anything"}`)
			if err == nil {
				t.Fatal("search succeeded, want a clean failure")
			}
			if got := tool.FailureMessage(err); got != "I couldn't search the web right now." {
				t.Fatalf("failure message = %q", got)
			}
			// The raw upstream detail stays in the log; the Tool error carries
			// only the speakable line.
			if strings.Contains(err.Error(), "http://") {
				t.Fatalf("Tool error leaked the upstream URL: %v", err)
			}
		})
	}
}

func TestSearchWhenSearXNGIsDownFailsCleanly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	reg := register(t, configure(t, map[string]any{"base_url": url, "timeout": "200ms"}))
	_, err := execute(context.Background(), t, reg, `{"query":"anything"}`)
	if err == nil {
		t.Fatal("search against a closed server succeeded, want failure")
	}
	if got := tool.FailureMessage(err); got != "I couldn't search the web right now." {
		t.Fatalf("failure message = %q", got)
	}
}

func TestEmptyQueryIsRejectedBeforeTheUpstream(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	reg := register(t, configure(t, map[string]any{"base_url": server.URL}))
	_, err := execute(context.Background(), t, reg, `{"query":"   "}`)
	if err == nil {
		t.Fatal("blank query succeeded, want a Tool error")
	}
	if called {
		t.Fatal("blank query reached the upstream")
	}
	if got := tool.FailureMessage(err); got != "What should I search for?" {
		t.Fatalf("failure message = %q", got)
	}
}

func TestUnknownBackendFailsConfiguration(t *testing.T) {
	module := web.NewModule()
	var node yaml.Node
	if err := node.Encode(map[string]any{"backend": "googlecse"}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	err := module.Configure(&node)
	if err == nil {
		t.Fatal("unknown backend was accepted")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %v, want an unsupported-backend message", err)
	}
	if _, err := web.NewProvider(web.Config{Backend: "duckduckgo", BaseURL: "http://example.test"}); err == nil {
		t.Fatal("NewProvider accepted an unknown backend")
	}
}

func TestToolIsAgentOnlyAndNotOnTheFastPath(t *testing.T) {
	reg := register(t, web.NewModule())
	info, ok := reg.Info(web.Name)
	if !ok {
		t.Fatal("web.search was not registered")
	}
	if !info.AgentOnly {
		t.Fatal("web.search is not declared AgentOnly")
	}
	if info.LocalOnly {
		t.Fatal("web.search must be visible to the Agent path")
	}
	if info.Permission != tool.PermissionRead {
		t.Fatalf("Permission = %q, want read", info.Permission)
	}
	if info.HasSpeak || info.HasExtract || len(info.Examples) != 0 {
		t.Fatalf("web.search has Fast path hooks: %+v", info)
	}
	if pipeline.FastPathEligible(info) {
		t.Fatal("web.search is eligible for the semantic Fast path")
	}
}

func TestSearchReusesOneKeepAliveConnection(t *testing.T) {
	var mu sync.Mutex
	var peers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		peers = append(peers, r.RemoteAddr)
		mu.Unlock()
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer server.Close()

	reg := register(t, configure(t, map[string]any{"base_url": server.URL, "timeout": "1s"}))
	for i := 0; i < 2; i++ {
		if _, err := execute(context.Background(), t, reg, `{"query":"keep alive"}`); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peers) != 2 || peers[0] != peers[1] {
		t.Fatalf("remote peers = %v, want both requests on one kept-alive connection", peers)
	}
}

func TestHealthChecksTheBackendHealthz(t *testing.T) {
	status := http.StatusOK
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
	}))
	defer server.Close()

	module := configure(t, map[string]any{"base_url": server.URL, "timeout": "1s"})
	register(t, module)
	if err := module.Health(context.Background()); err != nil {
		t.Fatalf("healthy backend: %v", err)
	}
	if path != "/healthz" {
		t.Fatalf("health path = %q, want /healthz", path)
	}
	status = http.StatusServiceUnavailable
	if err := module.Health(context.Background()); err == nil {
		t.Fatal("unhealthy backend reported healthy")
	}
}

func TestFromConfigReadsTheModuleSection(t *testing.T) {
	cfg := config.Default()
	if cfg.Tools.Sections == nil {
		cfg.Tools.Sections = map[string]yaml.Node{}
	}
	cfg.Tools.Sections["web"] = yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "base_url"},
		{Kind: yaml.ScalarNode, Value: "http://search.internal:9000"},
	}}
	got := web.FromConfig(cfg)
	if got.BaseURL != "http://search.internal:9000" {
		t.Fatalf("base_url = %q", got.BaseURL)
	}
	if got.Backend != "searxng" {
		t.Fatalf("backend = %q, want the default", got.Backend)
	}
}

// TestLiveSearXNG exercises the real SearXNG JSON endpoint when
// SEARXNG_LIVE_URL points at an isolated instance (for example the Compose
// web-search service on a spare loopback port). It skips by default so the
// normal suite stays offline; it never invents a result.
func TestLiveSearXNG(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("SEARXNG_LIVE_URL"))
	if baseURL == "" {
		t.Skip("set SEARXNG_LIVE_URL to an isolated SearXNG instance to run the live smoke")
	}
	reg := register(t, configure(t, map[string]any{"base_url": baseURL}))
	out, err := execute(context.Background(), t, reg, `{"query":"searxng","max_results":3}`)
	if err != nil {
		t.Fatalf("live search: %v", err)
	}
	if len(out.Results) == 0 {
		t.Fatal("live SearXNG returned no results for a common query")
	}
	for _, result := range out.Results {
		if result.Title == "" || result.URL == "" {
			t.Fatalf("live result is missing a title or URL: %+v", result)
		}
		if len([]rune(result.Snippet)) > 241 || len([]rune(result.Title)) > 161 {
			t.Fatalf("live result is not short: %+v", result)
		}
	}
}
