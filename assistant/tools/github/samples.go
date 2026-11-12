package github

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: IssueCreateOut{
			Repo:   "acme/widgets",
			Number: 12,
			Title:  "Timers don't survive restart",
			URL:    "https://github.com/acme/widgets/issues/12",
		},
	}
}
