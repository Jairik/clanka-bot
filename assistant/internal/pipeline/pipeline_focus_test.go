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
	// delay simulates a holder that does real work, such as pausing Spotify.
	delay time.Duration
	// decline makes the holder take the intent and then report that it could
	// not act, as a holder that finished between the claim and the intent does.
	decline bool

	mu    sync.Mutex
	calls []focus.Intent
}

func (h *focusHolder) Intents() []focus.Intent { return h.intents }

func (h *focusHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	h.mu.Lock()
	h.calls = append(h.calls, intent)
	h.mu.Unlock()
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	if h.decline {
		return "", false
	}
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

// TestStandaloneStopWithNothingInFocusSuspendsWithoutRouting is the other half
// of the acceptance case: the bare word now ends the session instead of
// reaching the semantic Fast path (#501). Matching tolerates STT punctuation
// and case.
func TestStandaloneStopWithNothingInFocusSuspendsWithoutRouting(t *testing.T) {
	for _, transcript := range []string{"stop", "Stop.", "STOP!"} {
		p, controls := newFocusPipeline(t, map[string]string{"stop": "stop"}, &focus.Registry{})
		result := p.Handle(context.Background(), request(), transcript)

		if !result.Suspend {
			t.Fatalf("%q: Suspend = false, want the session to end", transcript)
		}
		if result.Text != "" {
			t.Fatalf("%q: Text = %q, want no spoken words", transcript, result.Text)
		}
		if result.Path != tool.PathFocusIntent {
			t.Fatalf("%q: path = %q, want %q", transcript, result.Path, tool.PathFocusIntent)
		}
		if controls.stopped != 0 {
			t.Fatalf("%q: audio.stop ran %d times, want no Tool", transcript, controls.stopped)
		}
	}
}

// TestStandaloneStopWithoutFocusWiringSuspends keeps the seam optional: with no
// Focus registry at all there is nothing to hold the stop, so it still ends the
// session rather than falling through.
func TestStandaloneStopWithoutFocusWiringSuspends(t *testing.T) {
	p, controls := newFocusPipeline(t, map[string]string{"stop": "stop"}, nil)
	result := p.Handle(context.Background(), request(), "stop")

	if !result.Suspend || result.Text != "" {
		t.Fatalf("reply = %+v, want a silent suspend", result)
	}
	if controls.stopped != 0 {
		t.Fatalf("audio.stop ran %d times, want no Tool", controls.stopped)
	}
}

// TestStandaloneStopWithMusicInFocusStopsMusicAndSuspends covers the other
// acceptance case: the existing Focus intent still runs first, and the session
// then ends as well.
func TestStandaloneStopWithMusicInFocusStopsMusicAndSuspends(t *testing.T) {
	var registry focus.Registry
	music := &focusHolder{name: "Music", intents: []focus.Intent{focus.Stop}}
	claim := registry.Claim(focus.Music, music)
	defer claim.Release()

	p, controls := newFocusPipeline(t, map[string]string{"stop": "stop"}, &registry)
	result := p.Handle(context.Background(), request(), "stop")

	if !result.Suspend {
		t.Fatal("Suspend = false, want the session to end after music stops")
	}
	if result.Text != "Music stop." {
		t.Fatalf("text = %q, want the holder's line", result.Text)
	}
	if got := music.seen(); len(got) != 1 || got[0] != focus.Stop {
		t.Fatalf("holder saw %v, want one stop", got)
	}
	if controls.stopped != 0 {
		t.Fatalf("audio.stop ran %d times, want the Focus holder only", controls.stopped)
	}
}

// TestStopInsideALongerRequestKeepsItsRouting proves only the bare phrase
// suspends: "stop the music in the kitchen" is still an ordinary request.
func TestStopInsideALongerRequestKeepsItsRouting(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Stopped the kitchen music."}
	logger, _ := testLogger()
	p, err := pipeline.New(tool.NewRegistry(time.Second), map[string]string{"stop": "stop"}, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithFocus(&focus.Registry{}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	result := p.Handle(context.Background(), request(), "stop the music in the kitchen")
	if result.Suspend {
		t.Fatal("Suspend = true for a longer request, want ordinary routing")
	}
	if result.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the Agent path", result.Path)
	}
	if got := runner.calls(); len(got) != 1 {
		t.Fatalf("agent calls = %v, want the longer request routed to it", got)
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
		// Two spellings that normalize to one phrase cannot split between a
		// Tool and an intent; the table is ambiguous.
		{map[string]string{"Stop": "stop", "stop": audiocontrol.StopName}, "maps to both"},
	}
	// Register audio.stop so the phrase/Intent collision is what fails, not an
	// unknown Tool found first depending on map iteration order.
	reg := tool.NewRegistry(time.Second)
	if err := audiocontrol.Register(reg, &audioRecorder{}); err != nil {
		t.Fatalf("register audio: %v", err)
	}
	for _, tc := range cases {
		_, err := pipeline.New(reg, tc.phrases, logger)
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

	// With nothing in Focus the intent phrase ends the session, not a Tool.
	result = p.Handle(context.Background(), request(), "stop")
	if result.Path == tool.PathControlPhrase {
		t.Fatalf("stop reply = %+v, want no Tool Control phrase", result)
	}
	if !result.Suspend {
		t.Fatalf("stop reply = %+v, want the session to end", result)
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

// TestStandaloneStopSkipsTheAgentWhenSemanticIsDisabled is the #501 guarantee:
// even as the final fallback, the Agent path never answers a bare "stop".
func TestStandaloneStopSkipsTheAgentWhenSemanticIsDisabled(t *testing.T) {
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
	if !result.Suspend || result.Text != "" {
		t.Fatalf("reply = %+v, want a silent suspend", result)
	}
	if got := runner.calls(); len(got) != 0 {
		t.Fatalf("agent calls = %v, want none for a bare stop", got)
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
