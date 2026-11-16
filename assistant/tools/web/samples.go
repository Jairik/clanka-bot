package web

// ConformanceSamples gives the conformance suite one representative output for
// each Speak Tool so the real renderer is exercised. web.search and web.fetch
// are Agent-only and need no sample.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		HeadlinesName: HeadlinesOut{
			Source: "Example",
			Items: []HeadlineItem{
				{Title: "Sample headline one", Link: "https://example.com/1", Source: "Example", Published: "2026-11-11T15:00:00Z"},
				{Title: "Sample headline two", Link: "https://example.com/2", Source: "Example", Published: "2026-11-11T14:00:00Z"},
			},
		},
		WikiName: WikiOut{
			Title:   "Capybara",
			Extract: "The capybara is a mammal native to South America. It is the largest living rodent.",
			URL:     "https://en.wikipedia.org/wiki/Capybara",
		},
	}
}
