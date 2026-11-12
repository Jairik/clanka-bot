package pipeline_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
)

// focusHolder is a pipeline-level stand-in for the reply or music holder.
type focusHolder struct {
	name    string
	intents []focus.Intent

	mu    sync.Mutex
	calls []focus.Intent
}

func (h *focusHolder) Intents() []focus.Intent { return h.intents }

func (h *focusHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	h.mu.Lock()
	h.calls = append(h.calls, intent)
	h.mu.Unlock()
	for _, declared := range h.intents {
		if declared == intent {
			return h.name + " " + string(intent) + ".", true
		}
	}
	return "", false
}

func (h *focusHolder) seen() []focus.Intent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]focus.Intent(nil), h.calls...)
}

// newFocusPipeline wires the audio Tools and a deterministic semantic router so
// a fall-through can be observed as the Fast path, not only the Agent path.
func newFocusPipeline(t *testing.T, phrases map[string]string, registry *focus.Registry) (*pipeline.Pipeline, *audioRecorder) {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	controls := &audioRecorder{}
	if err := audiocontrol.Register(reg, controls); err != nil {
		t.Fatalf("register audio: %v", err)
	}
	router := pipeline.NewSemanticRouter(reg, keywordEmbedder{}, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	logger, _ := testLogger()
	opts := []pipeline.Option{pipeline.WithSemanticRouter(router)}
	if registry != nil {
		opts = append(opts, pipeline.WithFocus(registry))
	}
	p, err := pipeline.New(reg, phrases, logger, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p, controls
}

// TestControlPhraseIntentReachesTheCurrentHolder covers the acceptance case:
// "stop" during a reply goes to the reply holder, not to a Tool.
func TestControlPhraseIntentReachesTheCurrentHolder(t *testing.T) {
	var registry focus.Registry
	reply := &focusHolder{name: "Stopped", intents: []focus.Intent{focus.Stop}}
	claim := registry.Claim(focus.Reply, reply)
	defer claim.Release()

	p, controls := newFocusPipeline(t, map[string]string{"stop": "stop"}, &registry)
	result := p.Handle(context.Background(), request(), "Stop.")

	if result.Path != tool.PathFocusIntent {
		t.Fatalf("path = %q, want %q", result.Path, tool.PathFocusIntent)
	}
	if result.Text != "Stopped stop." {
		t.Fatalf("text = %q, want the holder's line", result.Text)
	}
	if result.Tool != "" {
		t.Fatalf("tool = %q, want no Tool for a Focus intent", result.Tool)
	}
	if got := reply.seen(); len(got) != 1 || got[0] != focus.Stop {
		t.Fatalf("holder saw %v, want one stop", got)
	}
	if controls.stopped != 0 {
		t.Fatalf("audio.stop ran %d times, want the Focus holder only", controls.stopped)
	}
}

// TestControlPhraseIntentFallsThroughWithNothingInFocus is the other half of
// the acceptance case: the same word still reaches the semantic Fast path.
func TestControlPhraseIntentFallsThroughWithNothingInFocus(t *testing.T) {
	p, controls := newFocusPipeline(t, map[string]string{"stop": "stop"}, &focus.Registry{})
	result := p.Handle(context.Background(), request(), "stop")

	if result.Path != tool.PathFast || result.Tool != audiocontrol.StopName {
		t.Fatalf("reply = %+v, want the semantic Fast path to audio.stop", result)
	}
	if controls.stopped != 1 {
		t.Fatalf("audio.stop ran %d times, want one", controls.stopped)
	}
}

// TestControlPhraseIntentWithoutFocusWiringFallsThrough keeps the seam optional.
func TestControlPhraseIntentWithoutFocusWiringFallsThrough(t *testing.T) {
	p, controls := newFocusPipeline(t, map[string]string{"stop": "stop"}, nil)
	result := p.Handle(context.Background(), request(), "stop")

	if result.Path != tool.PathFast || result.Tool != audiocontrol.StopName {
		t.Fatalf("reply = %+v, want the semantic Fast path", result)
	}
	if controls.stopped != 1 {
		t.Fatalf("audio.stop ran %d times, want one", controls.stopped)
	}
}

// TestControlPhraseIntentFallsToALowerHolder proves the priority walk at the
// routing seam: the reply does not pause, so music gets "pause".
func TestControlPhraseIntentFallsToALowerHolder(t *testing.T) {
	var registry focus.Registry
	reply := &focusHolder{name: "Reply", intents: []focus.Intent{focus.Stop}}
	music := &focusHolder{name: "Music", intents: []focus.Intent{focus.Pause}}
	replyClaim := registry.Claim(focus.Reply, reply)
	musicClaim := registry.Claim(focus.Music, music)
	defer replyClaim.Release()
	defer musicClaim.Release()

	p, _ := newFocusPipeline(t, map[string]string{"pause": "pause"}, &registry)
	result := p.Handle(context.Background(), request(), "pause")

	if result.Path != tool.PathFocusIntent || result.Text != "Music pause." {
		t.Fatalf("reply = %+v, want music to pause", result)
	}
	if got := reply.seen(); len(got) != 0 {
		t.Fatalf("reply was offered %v, want none", got)
	}
}

// TestUnknownIntentIsRejected keeps a typo in config from becoming a phrase
// that silently never fires.
func TestUnknownIntentIsRejected(t *testing.T) {
	logger, _ := testLogger()
	cases := []struct {
		phrases map[string]string
		want    string
	}{
		{map[string]string{"stop": "stopp"}, "unknown intent"},
		{map[string]string{"stop": ""}, "no target"},
		{map[string]string{"stop": "pause the music"}, "unknown intent"},
	}
	for _, tc := range cases {
		_, err := pipeline.New(tool.NewRegistry(time.Second), tc.phrases, logger)
		if err == nil {
			t.Fatalf("pipeline.New accepted %v, want an error", tc.phrases)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error = %q, want %q", err, tc.want)
		}
	}
}

// TestIntentAndToolPhrasesShareOneTable is the table contract: one phrase names
// a Tool and another names an intent, with the same validation as before.
func TestIntentAndToolPhrasesShareOneTable(t *testing.T) {
	p, controls := newFocusPipeline(t, map[string]string{
		"stop":   "stop",
		"louder": audiocontrol.VolumeName,
	}, &focus.Registry{})

	result := p.Handle(context.Background(), request(), "louder")
	if result.Path != tool.PathControlPhrase || result.Tool != audiocontrol.VolumeName {
		t.Fatalf("louder reply = %+v, want the Tool Control phrase", result)
	}
	if controls.delta != audiocontrol.StepPercent {
		t.Fatalf("delta = %d, want +%d", controls.delta, audiocontrol.StepPercent)
	}

	// With nothing in Focus the intent phrase still falls through, not to a Tool.
	result = p.Handle(context.Background(), request(), "stop")
	if result.Path == tool.PathControlPhrase {
		t.Fatalf("stop reply = %+v, want no Tool Control phrase", result)
	}
}

// TestPendingConfirmationStillOutranksAFocusIntent preserves the fixed routing
// order: a pending Confirmation settles first, even when the utterance is a
// shared phrase that a Focus holder would take.
func TestPendingConfirmationStillOutranksAFocusIntent(t *testing.T) {
	var registry focus.Registry
	reply := &focusHolder{name: "Stopped", intents: []focus.Intent{focus.Stop}}
	claim := registry.Claim(focus.Reply, reply)
	defer claim.Release()

	resolver := &fakeConfirmation{handled: true, reply: pipeline.Reply{Text: "Cancelled."}}
	// A Confirmation resolver is a construction option, so build the pipeline
	// with both seams and the same registry.
	reg := tool.NewRegistry(time.Second)
	logger, _ := testLogger()
	confirmationPipeline, err := pipeline.New(reg, map[string]string{"stop": "stop"}, logger,
		pipeline.WithConfirmationResolver(resolver),
		pipeline.WithFocus(&registry),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	result := confirmationPipeline.Handle(context.Background(), request(), "stop")
	if result.Path != tool.PathConfirmation || result.Text != "Cancelled." {
		t.Fatalf("reply = %+v, want the Confirmation to win", result)
	}
	if got := reply.seen(); len(got) != 0 {
		t.Fatalf("Focus holder saw %v, want the Confirmation to settle first", got)
	}
}

// TestFocusIntentFallsToTheAgentWhenSemanticIsDisabled keeps the last step of
// the order intact: no Focus, no semantic match, so the Agent path answers.
func TestFocusIntentFallsToTheAgentWhenSemanticIsDisabled(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "agent answer"}
	logger, _ := testLogger()
	p, err := pipeline.New(reg, map[string]string{"stop": "stop"}, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithFocus(&focus.Registry{}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	result := p.Handle(context.Background(), request(), "stop")
	if result.Path != tool.PathAgent || result.Text != "agent answer" {
		t.Fatalf("reply = %+v, want the Agent path", result)
	}
}

// focusWording is the settings-backed source the pipeline reads once per request.
type focusWording struct {
	mu    sync.Mutex
	words pipeline.RoutingWords
}

func (w *focusWording) Routing() pipeline.RoutingWords {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.words
}

func (w *focusWording) set(words pipeline.RoutingWords) {
	w.mu.Lock()
	w.words = words
	w.mu.Unlock()
}

// TestLiveWordingIntentAppliesOnTheNextRequest proves a stored phrase override
// reaches the Focus route without rebuilding the pipeline.
func TestLiveWordingIntentAppliesOnTheNextRequest(t *testing.T) {
	var registry focus.Registry
	reply := &focusHolder{name: "Stopped", intents: []focus.Intent{focus.Stop}}
	claim := registry.Claim(focus.Reply, reply)
	defer claim.Release()

	words := &focusWording{}
	reg := tool.NewRegistry(time.Second)
	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithWording(words),
		pipeline.WithFocus(&registry),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	if result := p.Handle(context.Background(), request(), "stop"); result.Path == tool.PathFocusIntent {
		t.Fatalf("reply = %+v, want no Focus route before the override", result)
	}

	words.set(pipeline.RoutingWords{IntentPhrases: map[string]string{"stop": "stop"}})
	result := p.Handle(context.Background(), request(), "stop")
	if result.Path != tool.PathFocusIntent || result.Text != "Stopped stop." {
		t.Fatalf("reply = %+v, want the live intent to reach the holder", result)
	}
}
