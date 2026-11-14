package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// audioRecorder records the commands the audio Tools issue.
type audioRecorder struct {
	volume  int
	delta   int
	stopped int
}

func (a *audioRecorder) SetVolume(_ context.Context, percent int) error {
	a.volume = percent
	return nil
}

func (a *audioRecorder) AdjustVolume(_ context.Context, delta int) error {
	a.delta = delta
	return nil
}

func (a *audioRecorder) Stop(context.Context) error {
	a.stopped++
	return nil
}

// keywordEmbedder is a deterministic stand-in for MiniLM: examples and
// utterances land on one-hot category vectors.
type keywordEmbedder struct{}

func (keywordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		lower := strings.ToLower(text)
		v := make([]float32, 6)
		switch {
		case strings.Contains(lower, "cancel"):
			v[0] = 1
		case strings.Contains(lower, "left") || strings.Contains(lower, "remaining") || strings.Contains(lower, "until") || strings.Contains(lower, "longer") || strings.Contains(lower, "finish"):
			v[1] = 1
		case strings.Contains(lower, "volume") || strings.Contains(lower, "louder") ||
			strings.Contains(lower, "quieter") || strings.Contains(lower, "mute") || strings.Contains(lower, "turn it"):
			v[2] = 1
		case strings.Contains(lower, "stop") || strings.Contains(lower, "quiet") || strings.Contains(lower, "pause"):
			v[3] = 1
		case strings.Contains(lower, "timers"):
			v[4] = 1
		default:
			v[5] = 1
		}
		out[i] = v
	}
	return out, nil
}

// newToolPipeline wires the real date, timer and audio Tools.
func newToolPipeline(t *testing.T, controls audiocontrol.Controls) (*pipeline.Pipeline, *timer.Manager) {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := audiocontrol.Register(reg, controls); err != nil {
		t.Fatalf("register audio: %v", err)
	}
	manager := timer.NewManager(nil, nil, nil)
	if err := timer.Register(reg, manager); err != nil {
		t.Fatalf("register timer: %v", err)
	}

	phrases := map[string]string{
		"stop":    audiocontrol.StopName,
		"pause":   audiocontrol.StopName,
		"cancel":  timer.CancelName,
		"louder":  audiocontrol.VolumeName,
		"quieter": audiocontrol.VolumeName,
		"mute":    audiocontrol.VolumeName,
	}
	router := pipeline.NewSemanticRouter(reg, keywordEmbedder{}, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	logger, _ := testLogger()
	p, err := pipeline.New(reg, phrases, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p, manager
}

func TestFastPathSetsAndReportsTimerRemaining(t *testing.T) {
	p, manager := newToolPipeline(t, &audioRecorder{})

	reply := p.Handle(context.Background(), request(), "set a timer for five minutes")
	if reply.Path != tool.PathFast || reply.Tool != timer.SetName {
		t.Fatalf("reply = %+v, want timer.set on the Fast path", reply)
	}
	if want := "Timer set for 5 minutes."; reply.Text != want {
		t.Fatalf("reply = %q, want %q", reply.Text, want)
	}
	if timers := manager.List(); len(timers) != 1 {
		t.Fatalf("timers = %v, want one", timers)
	}
	manager.Cancel("")

	reply = p.Handle(context.Background(), request(), "how long is left on my timer")
	if reply.Tool != timer.RemainingName || !strings.Contains(reply.Text, "No timers") {
		t.Fatalf("remaining reply = %+v, want timer.remaining speaking the empty answer", reply)
	}
}

func TestControlPhrasesReachTheAudioAndTimerTools(t *testing.T) {
	controls := &audioRecorder{}
	p, manager := newToolPipeline(t, controls)

	reply := p.Handle(context.Background(), request(), "louder")
	if reply.Path != tool.PathControlPhrase || controls.delta != audiocontrol.StepPercent {
		t.Fatalf("louder reply = %+v delta = %d, want +%d", reply, controls.delta, audiocontrol.StepPercent)
	}

	reply = p.Handle(context.Background(), request(), "quieter")
	if reply.Path != tool.PathControlPhrase || controls.delta != -audiocontrol.StepPercent {
		t.Fatalf("quieter reply = %+v delta = %d, want -%d", reply, controls.delta, audiocontrol.StepPercent)
	}

	reply = p.Handle(context.Background(), request(), "mute")
	if reply.Path != tool.PathControlPhrase || controls.volume != 0 {
		t.Fatalf("mute reply = %+v volume = %d, want 0 through the sink backend", reply, controls.volume)
	}

	reply = p.Handle(context.Background(), request(), "stop")
	if reply.Path != tool.PathControlPhrase || controls.stopped != 1 {
		t.Fatalf("stop reply = %+v stops = %d, want one stop", reply, controls.stopped)
	}

	manager.Set(time.Hour)
	reply = p.Handle(context.Background(), request(), "cancel")
	if reply.Path != tool.PathControlPhrase || !strings.Contains(reply.Text, "cancelled") {
		t.Fatalf("cancel reply = %+v, want the timer cancelled", reply)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("timers = %v, want empty", manager.List())
	}
}

func TestVolumeToFortyExtractsTheNumber(t *testing.T) {
	controls := &audioRecorder{}
	p, _ := newToolPipeline(t, controls)

	reply := p.Handle(context.Background(), request(), "turn the volume to 40")
	if reply.Path != tool.PathFast || reply.Tool != audiocontrol.VolumeName {
		t.Fatalf("reply = %+v, want audio.volume_set on the Fast path", reply)
	}
	if controls.volume != 40 {
		t.Fatalf("volume = %d, want 40", controls.volume)
	}
}

func TestTimerExtractFailureFallsThroughToTheAgent(t *testing.T) {
	controls := &audioRecorder{}
	reg := tool.NewRegistry(time.Second)
	if err := timer.Register(reg, timer.NewManager(nil, nil, nil)); err != nil {
		t.Fatalf("register timer: %v", err)
	}
	if err := audiocontrol.Register(reg, controls); err != nil {
		t.Fatalf("register audio: %v", err)
	}
	router := pipeline.NewSemanticRouter(reg, keywordEmbedder{}, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "agent answer"}
	logger, logs := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgent(runner, nil),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "set a timer")
	if reply.Path != tool.PathAgent || reply.Text != "agent answer" {
		t.Fatalf("reply = %+v, want the Agent fallback", reply)
	}
	if got := logRecord(t, logs)["fallthrough_reason"]; got != "extract_failed" {
		t.Fatalf("fallthrough_reason = %v, want extract_failed", got)
	}
}
