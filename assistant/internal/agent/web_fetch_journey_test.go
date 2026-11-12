package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
)

// fixtureFetcher stands in for the SSRF-guarded reader in the Agent journey.
// The real guard, DNS pin and redirect checks are exercised in tools/web; here
// the local fixture proves the Agent offers, calls and grounds an answer in
// web.fetch.
type fixtureFetcher struct {
	mu    sync.Mutex
	got   string
	page  web.Page
	err   error
	calls int
}

func (f *fixtureFetcher) Fetch(_ context.Context, rawURL string) (web.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = rawURL
	f.calls++
	return f.page, f.err
}

func fetchRegistry(t *testing.T, fetcher web.Fetcher) *tool.Registry {
	t.Helper()
	reg := newRegistry(t)
	addTool(t, reg, web.NewFetch(web.DefaultFetchConfig(), fetcher,
		slog.New(slog.NewTextHandler(io.Discard, nil))))
	return reg
}

// TestAgentReadsAnArticleWithWebFetch is the #67 Agent journey: the real Agent
// loop, prompt builder and web.fetch Tool contract against a local page
// fixture and a scripted model. It asserts the article text reached the
// follow-up request and the answer came from it, not from the model's guess.
func TestAgentReadsAnArticleWithWebFetch(t *testing.T) {
	fetcher := &fixtureFetcher{page: web.Page{
		URL:   "https://news.example.test/report",
		Title: "City beat United 2-1",
		Text:  "City beat United 2-1 with a stoppage-time winner. The report says the crowd stayed to applaud.",
	}}
	fake := newFakeProvider(t,
		toolCallReply("",
			fakeCall{ID: "call-fetch", Name: "web__fetch", Args: `{"url":"https://news.example.test/report"}`}),
		textReply("City beat United 2-1 with a stoppage-time winner."),
	)
	server := fake.start()

	a := newTestAgent(t, testSettings(server.URL), fetchRegistry(t, fetcher), nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-web-fetch", Device: tool.DevicePi},
		"read me that article about the match", nil)

	if fetcher.got != "https://news.example.test/report" {
		t.Fatalf("fetcher URL = %q", fetcher.got)
	}
	if result.Text != "City beat United 2-1 with a stoppage-time winner." {
		t.Fatalf("reply = %q", result.Text)
	}
	if len(result.Calls) != 1 || result.Calls[0].Name != web.FetchName {
		t.Fatalf("calls = %+v, want one web.fetch call", result.Calls)
	}
	if strings.Contains(string(result.Calls[0].Result), "error") {
		t.Fatalf("web.fetch failed: %s", result.Calls[0].Result)
	}

	// The Tool was offered under its wire name with url required and the
	// untrusted-page warning intact.
	first := fake.request(t, 0)
	firstTools, err := json.Marshal(first.Body["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if !strings.Contains(string(firstTools), `"web__fetch"`) {
		t.Fatalf("the model was not offered web.fetch: %s", firstTools)
	}
	if !strings.Contains(string(firstTools), `"required":["url"]`) {
		t.Fatalf("web.fetch schema does not require url: %s", firstTools)
	}
	if !strings.Contains(string(firstTools), "untrusted facts") {
		t.Fatalf("web.fetch description lost the untrusted-page warning: %s", firstTools)
	}

	// The follow-up request carries the real page text, so the scripted answer
	// is grounded in the fetch rather than invented.
	secondMessages, err := json.Marshal(fake.request(t, 1).Body["messages"])
	if err != nil {
		t.Fatalf("marshal second messages: %v", err)
	}
	if !strings.Contains(string(secondMessages), "stoppage-time winner") {
		t.Fatalf("the page text did not reach the model: %s", secondMessages)
	}
	if !strings.Contains(string(secondMessages), `"role":"tool"`) {
		t.Fatalf("no tool message in the follow-up request: %s", secondMessages)
	}
}

// TestAgentSaysItCouldNotReadThePage proves a fetch failure is a clean Tool
// failure the model can relay, not a fabricated answer or a leaked address.
func TestAgentSaysItCouldNotReadThePage(t *testing.T) {
	fetcher := &fixtureFetcher{err: errors.New("guard: destination is not public")}
	fake := newFakeProvider(t,
		toolCallReply("",
			fakeCall{ID: "call-fetch", Name: "web__fetch", Args: `{"url":"http://127.0.0.1/secret"}`}),
		textReply("I couldn't read that page right now."),
	)
	server := fake.start()

	a := newTestAgent(t, testSettings(server.URL), fetchRegistry(t, fetcher), nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-web-fetch-down", Device: tool.DevicePi},
		"read me the page at that address", nil)

	if result.Text != "I couldn't read that page right now." {
		t.Fatalf("reply = %q", result.Text)
	}
	if len(result.Calls) != 1 || result.Calls[0].Name != web.FetchName {
		t.Fatalf("calls = %+v, want one web.fetch call", result.Calls)
	}
	if !strings.Contains(string(result.Calls[0].Result), "I couldn't read that page right now.") {
		t.Fatalf("Tool result did not carry the speakable failure: %s", result.Calls[0].Result)
	}
	// The fixed line reaches the model; the raw reason does not. The URL is
	// present only because the model itself supplied it in its tool call.
	messages, err := json.Marshal(fake.request(t, 1).Body["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	if !strings.Contains(string(messages), "I couldn't read that page right now.") {
		t.Fatalf("the failure did not reach the model: %s", messages)
	}
	if strings.Contains(string(messages), "guard:") {
		t.Fatalf("raw fetch detail leaked to the model: %s", messages)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(messages, &decoded); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	for _, message := range decoded {
		if message["role"] != "tool" {
			continue
		}
		content, _ := message["content"].(string)
		if strings.Contains(content, "guard:") || strings.Contains(content, "127.0.0.1") {
			t.Fatalf("Tool result leaked the raw reason or address: %q", content)
		}
	}
}
