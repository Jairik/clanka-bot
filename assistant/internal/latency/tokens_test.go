package latency

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestSetTokensPublishesIntegerCountsOnRequestLine(t *testing.T) {
	tl := New("req-tokens", SourceVoice)
	ctx := WithContext(context.Background(), tl)
	SetTokens(ctx, 12000, 34, 12034, 512, 2)

	var rendered bytes.Buffer
	tl.Complete(slog.New(slog.NewJSONHandler(&rendered, nil)), OutcomeNoAudio, nil)
	var line map[string]any
	if err := json.Unmarshal(rendered.Bytes(), &line); err != nil {
		t.Fatalf("request line is not JSON: %v\n%s", err, rendered.String())
	}
	for key, want := range map[string]float64{
		"prompt_tokens":     12000,
		"completion_tokens": 34,
		"total_tokens":      12034,
		"cached_tokens":     512,
		"agent_steps":       2,
	} {
		got, ok := line[key].(float64)
		if !ok || got != want {
			t.Errorf("%s = %v, want %v", key, line[key], want)
		}
	}
}
