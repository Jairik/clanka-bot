package alarm_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// alarmKeywordEmbedder is a deterministic stand-in for MiniLM in the pipeline
// test. It puts set, update, cancel and list Examples on separate axes, so the
// test exercises the real pipeline and Tool extraction without a model.
type alarmKeywordEmbedder struct{}

func (alarmKeywordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		lower := strings.ToLower(text)
		vector := make([]float32, 5)
		switch {
		case strings.Contains(lower, "change") || strings.Contains(lower, "update") ||
			strings.Contains(lower, "switch") || strings.Contains(lower, "use the") ||
			strings.Contains(lower, "make my alarm") || strings.Contains(lower, "remove the sound") ||
			strings.Contains(lower, "clear my alarm") || strings.Contains(lower, "reset my alarm") ||
			strings.Contains(lower, "alarm sound") || strings.Contains(lower, "alarm volume") ||
			strings.Contains(lower, "alarm ramp"):
			vector[0] = 1
		case strings.Contains(lower, "cancel") || strings.Contains(lower, "delete"):
			vector[2] = 1
		case strings.Contains(lower, "wake") || strings.Contains(lower, "set an alarm") ||
			strings.Contains(lower, "set a "):
			vector[1] = 1
		case strings.Contains(lower, "what alarms") || strings.Contains(lower, "list") ||
			strings.Contains(lower, "show") || strings.Contains(lower, "how many"):
			vector[3] = 1
		default:
			vector[4] = 1
		}
		out[i] = vector
	}
	return out, nil
}

// TestPipelineSetsAndUpdatesPerAlarmSound runs the two full utterances through
// the Fast path, the real Tool handlers and the real temporary database.
func TestPipelineSetsAndUpdatesPerAlarmSound(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	registry := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(registry, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	router := pipeline.NewSemanticRouter(registry, alarmKeywordEmbedder{}, pipeline.SemanticConfig{
		Threshold: 0.35,
		Margin:    0.10,
		Joiners:   []string{"and"},
		Timeout:   time.Second,
	})
	if err := router.Build(ctx); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pipeline.New(registry, nil, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	handle := func(utterance string) pipeline.Reply {
		t.Helper()
		return p.Handle(ctx, tool.Request{ID: "req-1", Device: tool.DevicePi}, utterance)
	}

	// "wake me at 7 with the birds sound" must create the Alarm with the
	// chosen sound, not update an existing one.
	setReply := handle("wake me at 7 with the birds sound")
	if setReply.Tool != alarm.SetName || setReply.Path != tool.PathFast {
		t.Fatalf("set reply = %+v, want alarm.set on the Fast path", setReply)
	}
	if !strings.Contains(setReply.Text, "with the birds sound") {
		t.Fatalf("set reply = %q, want the chosen sound", setReply.Text)
	}
	list := manager.List()
	if len(list) != 1 || list[0].Sound != "birds" || list[0].Hour != 7 {
		t.Fatalf("after set = %+v, want one 7:00 alarm with birds", list)
	}
	next := list[0].NextFiresAt

	// "change my 7 am alarm to the rain sound" must update the stored Alarm.
	updateReply := handle("change my 7 am alarm to the rain sound")
	if updateReply.Tool != alarm.UpdateName || updateReply.Path != tool.PathFast {
		t.Fatalf("update reply = %+v, want alarm.update on the Fast path", updateReply)
	}
	if !strings.Contains(updateReply.Text, "the rain sound") {
		t.Fatalf("update reply = %q, want the concrete change", updateReply.Text)
	}
	list = manager.List()
	if len(list) != 1 || list[0].Sound != "rain" {
		t.Fatalf("after update = %+v, want one alarm with rain", list)
	}
	if !list[0].NextFiresAt.Equal(next) {
		t.Fatalf("next fire = %s, want the preserved %s", list[0].NextFiresAt, next)
	}

	// An unknown name falls back to the kind default and the reply says so.
	unknownReply := handle("change my 7 am alarm to the cowbell sound")
	if unknownReply.Tool != alarm.UpdateName || unknownReply.Path != tool.PathFast {
		t.Fatalf("unknown reply = %+v, want alarm.update on the Fast path", unknownReply)
	}
	if !strings.Contains(unknownReply.Text, "couldn't find that sound") {
		t.Fatalf("unknown reply = %q, want the fallback sentence", unknownReply.Text)
	}
	list = manager.List()
	if len(list) != 1 || list[0].Sound != "" {
		t.Fatalf("after unknown = %+v, want the default sound", list)
	}
	// The unknown reply must not name a different library sound.
	for _, name := range []string{"birds", "rain", "chime"} {
		if strings.Contains(unknownReply.Text, name) {
			t.Fatalf("unknown reply named %q: %q", name, unknownReply.Text)
		}
	}

	// A near miss goes through the shared matcher, not a guessed entry.
	nearReply := handle("change my 7 am alarm to the bird sound")
	if nearReply.Tool != alarm.UpdateName || !strings.Contains(nearReply.Text, "the birds sound") {
		t.Fatalf("near-miss reply = %+v, want the birds sound", nearReply)
	}
}
