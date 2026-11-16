package alerts

import "time"

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	base := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	return map[string]any{
		Name: Out{Items: []Item{
			{Source: "timer", ID: "1", At: base.Add(4 * time.Minute), Text: "the pasta timer has 4 minutes left"},
			{Source: "reminder", ID: "2", At: base.Add(2 * time.Hour), Text: "a reminder to call mum is at 10 am"},
			{Source: "alarm", ID: "3", At: base.Add(6 * time.Hour), Text: "your 7:00 pm alarm is next"},
		}},
	}
}
