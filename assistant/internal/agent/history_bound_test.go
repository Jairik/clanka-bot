package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// largeText builds a Tool result body of exactly n bytes that mixes ASCII with
// multi-byte runes, so a byte cut near the cap lands mid-character.
func largeText(n int) string {
	const unit = "The report says the crowd stayed to applaud. ☕ 東京 "
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(unit)
	}
	return b.String()[:n]
}

type truncatedEnvelope struct {
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
	Preview   string `json:"preview"`
}

// decodeEnvelope asserts a stored result is the truncated envelope, not raw
// cut-off JSON, and returns it.
func decodeEnvelope(t *testing.T, stored json.RawMessage) truncatedEnvelope {
	t.Helper()
	if !json.Valid(stored) {
		t.Fatalf("stored result is not valid JSON: %q", stored)
	}
	var envelope truncatedEnvelope
	if err := json.Unmarshal(stored, &envelope); err != nil {
		t.Fatalf("stored result is not the envelope: %v (%q)", err, stored)
	}
	if !envelope.Truncated {
		t.Fatalf("stored result carries no truncation marker: %q", stored)
	}
	return envelope
}

// TestHistoryBoundsAnOversizedToolResult is the #202 regression: a 200 KB Tool
// result must not stay in the shared history at full size, and what replaces it
// must be a marked, valid JSON envelope that still says how big the answer was.
func TestHistoryBoundsAnOversizedToolResult(t *testing.T) {
	full, err := json.Marshal(map[string]string{"text": largeText(200_000)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(full) <= agent.MaxResultBytes {
		t.Fatalf("fixture is only %d bytes, want more than the cap", len(full))
	}

	history := agent.NewHistory(10, 0, nil)
	history.Add(agent.Turn{
		Utterance: "read me that article",
		Reply:     "It's a long one.",
		Calls:     []agent.ToolCall{{Name: "web.fetch", Result: full}},
	})

	turns := history.Recent()
	if len(turns) != 1 || len(turns[0].Calls) != 1 {
		t.Fatalf("turns = %+v, want one turn with one call", turns)
	}
	stored := turns[0].Calls[0].Result
	if len(stored) > agent.MaxResultBytes {
		t.Fatalf("stored result is %d bytes, want at most %d", len(stored), agent.MaxResultBytes)
	}
	envelope := decodeEnvelope(t, stored)
	if envelope.Bytes != len(full) {
		t.Errorf("envelope bytes = %d, want the original %d", envelope.Bytes, len(full))
	}
	if envelope.Preview == "" {
		t.Error("envelope preview is empty; the model cannot tell what the turn returned")
	}
	if len(envelope.Preview) > agent.MaxPreviewBytes {
		t.Errorf("preview is %d bytes, want at most %d", len(envelope.Preview), agent.MaxPreviewBytes)
	}
	if !utf8.ValidString(envelope.Preview) {
		t.Errorf("preview is not valid UTF-8: %q", envelope.Preview)
	}
	if !strings.HasPrefix(string(full), envelope.Preview) {
		t.Error("preview is not a prefix of the original result")
	}
	if !strings.Contains(string(stored), `"truncated":true`) {
		t.Errorf("stored result carries no truncation marker: %q", stored)
	}
}

// TestHistoryKeepsASmallResultByteForByte guards the common case: nothing about
// a normal turn changes on the way into history.
func TestHistoryKeepsASmallResultByteForByte(t *testing.T) {
	small := json.RawMessage(`{"summary":"sunny","temperature_c":14}`)
	history := agent.NewHistory(10, 0, nil)
	history.Add(agent.Turn{
		Utterance: "what's the weather today",
		Reply:     "It's sunny.",
		Calls: []agent.ToolCall{{
			Name:   "weather.current",
			Args:   json.RawMessage(`{"day":"today"}`),
			Result: small,
		}},
	})

	stored := history.Recent()[0].Calls[0].Result
	if string(stored) != string(small) {
		t.Fatalf("stored result = %q, want the original %q", stored, small)
	}

	// The rendered history part is byte-for-byte the pre-#202 text.
	messages := agent.NewPromptBuilder("system").Messages("and tomorrow?", history.Recent())
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(wire) != 4 || wire[2]["role"] != "assistant" {
		t.Fatalf("wire = %v, want system, user, assistant, user", wire)
	}
	wantContent := `It's sunny.` + "\n" +
		`[{"tool":"weather.current","args":{"day":"today"},"result":{"summary":"sunny","temperature_c":14}}]`
	if got, _ := wire[2]["content"].(string); got != wantContent {
		t.Fatalf("history content changed:\nwant %q\ngot  %q", wantContent, got)
	}
}

// TestHistoryTruncationStaysValidJSONOnARuneBoundary feeds a payload whose byte
// cut would land inside a multi-byte rune and requires a clean preview.
func TestHistoryTruncationStaysValidJSONOnARuneBoundary(t *testing.T) {
	// Every rune here is three bytes, so the 500-byte cut is always mid-rune.
	full, err := json.Marshal(map[string]string{"text": strings.Repeat("東", 1000)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	history := agent.NewHistory(10, 0, nil)
	history.Add(agent.Turn{Utterance: "read it", Reply: "Long.", Calls: []agent.ToolCall{{
		Name: "web.fetch", Result: full,
	}}})

	stored := history.Recent()[0].Calls[0].Result
	envelope := decodeEnvelope(t, stored)
	if !utf8.ValidString(envelope.Preview) || strings.ContainsRune(envelope.Preview, utf8.RuneError) {
		t.Fatalf("preview is not clean UTF-8: %q", envelope.Preview)
	}
	if len(stored) > agent.MaxResultBytes {
		t.Fatalf("escaped envelope is %d bytes, want at most %d", len(stored), agent.MaxResultBytes)
	}
}

// TestHistoryCompactionNeverMutatesTheCallersResult proves the cap applies to
// what history keeps, not to the payload the current turn and Activity use.
func TestHistoryCompactionNeverMutatesTheCallersResult(t *testing.T) {
	full, err := json.Marshal(map[string]string{"text": largeText(20_000)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	calls := []agent.ToolCall{{Name: "web.fetch", Result: full}}

	history := agent.NewHistory(10, 0, nil)
	history.Add(agent.Turn{Utterance: "read it", Reply: "Long.", Calls: calls})

	if string(calls[0].Result) != string(full) {
		t.Fatal("Add mutated the caller's Tool result")
	}
}

// TestAgentBoundsALargeToolResultInHistoryWhileTheTurnKeepsIt is the Agent-path
// #202 acceptance test: the live request still carries the whole result, the
// recorded turn is bounded, and Result.Calls (which feeds Activity) keeps the
// full payload.
func TestAgentBoundsALargeToolResultInHistoryWhileTheTurnKeepsIt(t *testing.T) {
	const body = 8000
	registry := newRegistry(t)
	addTool(t, registry, tool.Tool[struct{}, struct {
		Text string `json:"text"`
	}]{
		Name:        "test.large",
		Description: "Return a large body of text.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (struct {
			Text string `json:"text"`
		}, error) {
			return struct {
				Text string `json:"text"`
			}{Text: largeText(body)}, nil
		},
	})

	fake := newFakeProvider(t,
		toolCallReply("", fakeCall{ID: "call-large", Name: "test__large", Args: "{}"}),
		textReply("It is a long report."),
		textReply("Still the same report."),
	)
	a := newTestAgent(t, testSettings(fake.start().URL), registry, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-large"}, "read me the report", nil)
	if len(result.Calls) != 1 {
		t.Fatalf("calls = %+v, want one", result.Calls)
	}
	if len(result.Calls[0].Result) < body {
		t.Fatalf("Result.Calls result is %d bytes, want the full payload for Activity", len(result.Calls[0].Result))
	}

	// The result the running turn needs reaches the model unchanged: the
	// second request in this turn is the Tool message for the fetch.
	fullPayload := bodyString(t, fake.request(t, 1).Body)
	if !strings.Contains(fullPayload, largeText(body)[body-400:body]) {
		t.Fatal("the current turn's Tool message was elided before the model used it")
	}
	if strings.Contains(fullPayload, `"truncated":true`) {
		t.Fatal("the current turn's Tool message was replaced by the truncation envelope")
	}

	turns := a.History().Recent()
	if len(turns) != 1 || len(turns[0].Calls) != 1 {
		t.Fatalf("history = %+v, want one turn with one call", turns)
	}
	stored := turns[0].Calls[0].Result
	if len(stored) > agent.MaxResultBytes {
		t.Fatalf("recorded result is %d bytes, want at most %d", len(stored), agent.MaxResultBytes)
	}
	decodeEnvelope(t, stored)

	// A later turn resends the bounded history, not the full page.
	if result := a.Run(context.Background(), tool.Request{ID: "req-follow"}, "and the weather?", nil); result.Text != "Still the same report." {
		t.Fatalf("follow-up reply = %q", result.Text)
	}
	followUp := bodyString(t, fake.request(t, 2).Body)
	if !strings.Contains(followUp, `\"truncated\":true`) && !strings.Contains(followUp, `"truncated":true`) {
		t.Fatalf("follow-up request is missing the truncation marker: %s", followUp)
	}
	if len(followUp) > 32*1024 {
		t.Fatalf("follow-up request is %d bytes; one capped turn must not carry the page", len(followUp))
	}
}

// TestPromptStaysInsideTheDocumentedBudgetForAFullHistory is the byte budget
// the cap exists to guarantee: ten turns of three capped results each.
func TestPromptStaysInsideTheDocumentedBudgetForAFullHistory(t *testing.T) {
	const (
		turns = 10
		calls = 3
		// Per call: the capped result plus room for the Tool name, args and the
		// JSON field names. The tail covers utterances, replies and the system
		// message.
		perCall = agent.MaxResultBytes + 128
		budget  = turns*calls*perCall + 1024
	)
	full, err := json.Marshal(map[string]string{"text": largeText(50_000)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	history := agent.NewHistory(turns, 0, nil)
	for i := 0; i < turns; i++ {
		turn := agent.Turn{Utterance: "read me another article", Reply: "It's a long one."}
		for j := 0; j < calls; j++ {
			turn.Calls = append(turn.Calls, agent.ToolCall{
				Name:   "web.fetch",
				Args:   json.RawMessage(`{"url":"https://news.example.test/report"}`),
				Result: full,
			})
		}
		history.Add(turn)
	}

	messages := agent.NewPromptBuilder("system").Messages("what about tomorrow?", history.Recent())
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	if len(raw) > budget {
		t.Fatalf("prompt is %d bytes, want at most the documented %d", len(raw), budget)
	}
	// The fixture must still be a real history, not an emptied one.
	if strings.Count(string(raw), `\"truncated\":true`) != turns*calls {
		t.Fatalf("prompt carries %d truncation markers, want %d",
			strings.Count(string(raw), `\"truncated\":true`), turns*calls)
	}
}
