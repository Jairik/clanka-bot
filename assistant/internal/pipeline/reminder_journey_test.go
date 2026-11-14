package pipeline_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// TestReminderDurationAndClockJourneyAgainstMiniLM is the acceptance journey
// at the real routing seam: a duration reminder, a clock reminder interpreted
// in the configured timezone, a list and a cancel must all reach the Reminder
// Tools through real parse extraction and the real MiniLM service.
func TestReminderDurationAndClockJourneyAgainstMiniLM(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("ROUTING_EVAL_EMBED_URL"))
	if endpoint == "" {
		t.Skip("set ROUTING_EVAL_EMBED_URL to the MiniLM embed service to run the Reminder journey")
	}
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC) // 16:04 in Berlin
	zone := tz.New("Europe/Berlin", nil)
	speaker := newJourneySpeaker()
	queue := announce.New(speaker, nil)
	t.Cleanup(queue.Close)
	module := reminder.NewModule()
	registry := tool.NewRegistry(5 * time.Second)
	if err := module.Register(registry, tool.Deps{
		Clock: func() time.Time { return fixed }, Zone: zone, Speaker: speaker, Announcements: queue,
	}); err != nil {
		t.Fatalf("register reminder: %v", err)
	}
	manager := module.Manager()
	t.Cleanup(manager.Close)

	cfg := loadRoutingConfig(t)
	router := pipeline.NewSemanticRouter(registry, embed.NewClient(endpoint, "sentence-transformers/all-MiniLM-L6-v2"), pipeline.SemanticConfig{
		Threshold: cfg.Semantic.Threshold,
		Margin:    cfg.Semantic.Margin,
		Joiners:   cfg.Semantic.Joiners,
		Timeout:   10 * time.Second,
	})
	ctx := context.Background()
	if err := router.Build(ctx); err != nil {
		t.Fatalf("build semantic router: %v", err)
	}
	logger, logBuf := testLogger()
	p, err := pipeline.New(registry, nil, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(ctx, request(), "remind me in 20 minutes to check the oven")
	if reply.Tool != reminder.SetName || reply.Path != tool.PathFast {
		t.Fatalf("duration reply = %+v, want reminder.set on the Fast path; log=%s", reply, logBuf.String())
	}
	list := manager.List()
	if len(list) != 1 || list[0].Message != "check the oven" || list[0].Kind != reminder.KindDuration {
		t.Fatalf("reminders after duration set = %+v", list)
	}
	if got, want := list[0].FiresAt, fixed.Add(20*time.Minute); !got.Equal(want) {
		t.Fatalf("duration fires at %s, want %s", got, want)
	}

	reply = p.Handle(ctx, request(), "remind me at 5 pm to call mum")
	if reply.Tool != reminder.SetName || reply.Path != tool.PathFast {
		t.Fatalf("clock reply = %+v, want reminder.set on the Fast path; log=%s", reply, logBuf.String())
	}
	list = manager.List()
	if len(list) != 2 {
		t.Fatalf("reminders after clock set = %+v", list)
	}
	var clock reminder.Reminder
	for _, item := range list {
		if item.Message == "call mum" {
			clock = item
		}
	}
	if want := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC); !clock.FiresAt.Equal(want) {
		t.Fatalf("clock reminder fires at %s, want Berlin 5 pm (%s)", clock.FiresAt, want)
	}

	reply = p.Handle(ctx, request(), "what reminders do I have")
	if reply.Tool != reminder.ListName || reply.Path != tool.PathFast {
		t.Fatalf("list reply = %+v, want reminder.list on the Fast path", reply)
	}
	if !strings.Contains(reply.Text, "2 reminders") {
		t.Fatalf("list text = %q, want both reminders", reply.Text)
	}
	info, ok := registry.Info(reminder.ListName)
	if !ok || !info.LocalOnly {
		t.Fatalf("reminder.list info = %+v, want LocalOnlyYes", info)
	}

	reply = p.Handle(ctx, request(), "cancel the oven reminder")
	if reply.Tool != reminder.CancelName || reply.Path != tool.PathFast {
		t.Fatalf("cancel reply = %+v, want reminder.cancel on the Fast path", reply)
	}
	if got := manager.List(); len(got) != 1 || got[0].Message != "call mum" {
		t.Fatalf("reminders after cancel = %+v, want only call mum", got)
	}
	manager.Cancel("")
}

// newJourneySpeaker records speech without a real TTS service.
type journeySpeaker struct{ said chan string }

func newJourneySpeaker() *journeySpeaker { return &journeySpeaker{said: make(chan string, 8)} }

func (s *journeySpeaker) SayText(_ context.Context, text string) error {
	select {
	case s.said <- text:
	default:
	}
	return nil
}
