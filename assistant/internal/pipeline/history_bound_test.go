package pipeline_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// largeBody builds a Tool result body of n bytes with multi-byte runes in it.
func largeBody(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("The report says the crowd stayed to applaud. ☕ 東京 ")
	}
	return b.String()[:n]
}

// TestFastPathBoundsAnOversizedToolResultInHistory is the #202 acceptance test
// for the shared Fast path: history keeps a marked, bounded result while the
// Activity record keeps the spoken answer for the turn that ran. It routes by
// control phrase, which reaches the same recordToolTurn as the semantic Fast
// path.
func TestFastPathBoundsAnOversizedToolResultInHistory(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[struct{}, struct {
		Body string `json:"body"`
	}]{
		Name:        "test.longread",
		Description: "Read a long body of text.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (struct {
			Body string `json:"body"`
		}, error) {
			return struct {
				Body string `json:"body"`
			}{Body: largeBody(40_000)}, nil
		},
		Speak: func(struct {
			Body string `json:"body"`
		}) string {
			return "It's a long report."
		},
	}); err != nil {
		t.Fatalf("register test.longread: %v", err)
	}

	history := agent.NewHistory(10, 0, nil)
	logger, _ := testLogger()
	p, err := pipeline.New(reg, map[string]string{"read the report": "test.longread"}, logger,
		pipeline.WithHistory(history))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	var (
		mu        sync.Mutex
		completed []latency.Completed
	)
	timeline := latency.New("req-long", latency.SourceDashboard, latency.WithObserver(func(c latency.Completed) {
		mu.Lock()
		defer mu.Unlock()
		completed = append(completed, c)
	}))
	ctx := latency.WithContext(context.Background(), timeline)
	reply := p.Handle(ctx, tool.Request{ID: "req-long"}, "read the report")
	latency.Complete(ctx, logger, latency.OutcomeNoAudio, nil)

	if reply.Path != tool.PathControlPhrase || reply.Tool != "test.longread" || reply.Text != "It's a long report." {
		t.Fatalf("reply = %+v, want a Fast path Tool speaking the short answer", reply)
	}

	turns := history.Recent()
	if len(turns) != 1 || len(turns[0].Calls) != 1 {
		t.Fatalf("history = %+v, want one turn with one call", turns)
	}
	stored := turns[0].Calls[0].Result
	if len(stored) > agent.MaxResultBytes {
		t.Fatalf("recorded result is %d bytes, want at most %d", len(stored), agent.MaxResultBytes)
	}
	if !json.Valid(stored) {
		t.Fatalf("recorded result is not valid JSON: %q", stored)
	}
	var envelope struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal(stored, &envelope); err != nil {
		t.Fatalf("recorded result is not the truncation envelope: %v", err)
	}
	if !envelope.Truncated || envelope.Bytes < 40_000 || envelope.Preview == "" {
		t.Fatalf("envelope = %+v, want the original size and a preview", envelope)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(completed) != 1 || completed[0].Detail == nil {
		t.Fatalf("completed = %+v, want one Activity detail", completed)
	}
	detail := completed[0].Detail
	if detail.Reply != reply.Text {
		t.Errorf("Activity reply = %q, want the spoken %q", detail.Reply, reply.Text)
	}
	if len(detail.Calls) != 1 || detail.Calls[0].Name != "test.longread" {
		t.Fatalf("Activity calls = %+v", detail.Calls)
	}
	if detail.Calls[0].Result != reply.Text {
		t.Errorf("Activity call result = %q, want the spoken text %q", detail.Calls[0].Result, reply.Text)
	}
}
