package web

import "context"

const (
	// maxQueryRunes bounds one model-supplied query. SearXNG forwards the
	// query to public engines; a runaway prompt must not become an unbounded
	// URL.
	maxQueryRunes = 300
	// maxTitleRunes and maxSnippetRunes keep a result short enough to speak
	// from or paste into the Agent prompt without swallowing the context.
	maxTitleRunes   = 160
	maxSnippetRunes = 240
	// maxURLRunes bounds a result link; a valid public URL is far shorter.
	maxURLRunes = 512
)

// Result is one search hit. It is exactly what web.search returns to the
// Agent: no HTML, engine names, scores or thumbnails.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Provider is one search backend. The module picks one implementation from
// tools.web.backend, so a new backend is a config case plus a constructor, not
// a Tool change.
type Provider interface {
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
}

// HealthChecker is a Provider that can answer readiness without searching.
// The shared status snapshot calls it through Module.Health.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// truncate shortens text to at most max runes, preserving valid UTF-8 and
// marking the cut with an ellipsis.
func truncate(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}
