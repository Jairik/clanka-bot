package wording_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

type testIn struct{}

type testOut struct {
	Text string `json:"text"`
}

// testRegistry covers the four shapes the wording definitions care about: a
// routable Tool, a second routable Tool, a confirm Tool and a Tool without
// Speak.
func testRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(2 * time.Second)
	add := func(name string, permission tool.Permission, speak bool, examples ...string) {
		t.Helper()
		var render func(testOut) string
		if speak {
			render = func(result testOut) string { return result.Text }
		}
		if err := tool.Add(reg, tool.Tool[testIn, testOut]{
			Name:        name,
			Description: "Test tool.",
			Permission:  permission,
			LocalOnly:   tool.LocalOnlyNo,
			Handler: func(context.Context, testIn) (testOut, error) {
				return testOut{Text: name}, nil
			},
			Speak:    render,
			Examples: examples,
		}); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	add("test.clock", tool.PermissionRead, true, "what time is it")
	add("test.weather", tool.PermissionRead, true, "will it rain")
	add("test.confirm", tool.PermissionConfirm, true, "delete everything")
	add("test.silent", tool.PermissionRead, false, "do a silent thing")
	return reg
}

func testConfig() wording.Config {
	return wording.Config{
		ControlPhrases:     map[string]string{"what time is it": "test.clock"},
		RequestPrefixes:    []string{"ask the agent", "hey agent"},
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	}
}

type fixture struct {
	settings *settings.Service
	manager  *wording.Manager
	registry *tool.Registry
	setup    wording.Setup
}

func newFixture(t *testing.T, initial *speech.Filler, synth wording.FillerSynth) *fixture {
	return newFixtureWith(t, initial, synth, nil)
}

func newFixtureWith(t *testing.T, initial *speech.Filler, synth wording.FillerSynth, extra []settings.Definition, opts ...wording.Option) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	reg := testRegistry(t)
	setup, err := wording.Build(reg, testConfig())
	if err != nil {
		t.Fatalf("wording.Build: %v", err)
	}
	defs := append(append([]settings.Definition(nil), setup.Definitions...), extra...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := wording.NewManager(svc, setup, initial, synth, logger, opts...)
	if err != nil {
		t.Fatalf("wording.NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)
	return &fixture{settings: svc, manager: mgr, registry: reg, setup: setup}
}

func definitionsByKey(defs []settings.Definition) map[string]settings.Definition {
	out := make(map[string]settings.Definition, len(defs))
	for _, def := range defs {
		out[def.Key] = def
	}
	return out
}

func TestBuildDefinesLiveWordingForEveryRoutableTool(t *testing.T) {
	setup, err := wording.Build(testRegistry(t), testConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defs := definitionsByKey(setup.Definitions)

	if _, ok := defs[wording.ControlPhrasesKey("test.clock")]; !ok {
		t.Error("test.clock has no control phrase setting")
	}
	if _, ok := defs[wording.ExamplesKey("test.clock")]; !ok {
		t.Error("test.clock has no Examples setting")
	}
	if _, ok := defs[wording.ExamplesKey("test.weather")]; !ok {
		t.Error("test.weather has no Examples setting")
	}
	for _, name := range []string{"test.confirm", "test.silent"} {
		if _, ok := defs[wording.ControlPhrasesKey(name)]; ok {
			t.Errorf("%s has a control phrase setting it can never route", name)
		}
		if _, ok := defs[wording.ExamplesKey(name)]; ok {
			t.Errorf("%s has an Examples setting it can never route", name)
		}
	}
	for key, def := range defs {
		if def.Kind != settings.Live {
			t.Errorf("%s kind = %q, want live", key, def.Kind)
		}
	}
	if got := defs[wording.ExamplesKey("test.clock")].Default; !reflect.DeepEqual(got, []string{"what time is it"}) {
		t.Errorf("test.clock Examples default = %#v, want the code declaration", got)
	}
}

func TestBuildRejectsConfigPhrasesThatCannotRoute(t *testing.T) {
	cases := map[string]struct {
		phrases map[string]string
	}{
		"unknown tool":   {map[string]string{"now": "test.missing"}},
		"confirm tool":   {map[string]string{"now": "test.confirm"}},
		"no speak tool":  {map[string]string{"now": "test.silent"}},
		"blank phrase":   {map[string]string{"  ": "test.clock"}},
		"duplicate text": {map[string]string{"now": "test.clock", "Now!": "test.weather"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.ControlPhrases = tc.phrases
			if _, err := wording.Build(testRegistry(t), cfg); err == nil {
				t.Fatal("Build accepted a Control phrase that cannot route")
			}
		})
	}
}

func TestResetRestoresTheConfigWording(t *testing.T) {
	f := newFixture(t, nil, nil)
	ctx := context.Background()
	key := wording.ControlPhrasesKey("test.clock")

	if _, err := f.settings.Set(ctx, key, []string{"current time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, _ := f.settings.Get(key)
	if !reflect.DeepEqual(value.Value, []string{"current time"}) || value.Source != settings.SourceOverride {
		t.Fatalf("after Set = %+v", value)
	}
	reset, err := f.settings.Reset(ctx, key)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !reflect.DeepEqual(reset.Value, []string{"what time is it"}) || reset.Source != settings.SourceConfig {
		t.Fatalf("after Reset = %+v, want the config phrase", reset)
	}
}

func TestRoutingDropsAPhraseClaimedByTwoTools(t *testing.T) {
	f := newFixture(t, nil, nil)
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	words := f.manager.Routing()
	if words.ControlPhrases["what time is it"] != "test.clock" {
		t.Fatalf("control phrases = %#v", words.ControlPhrases)
	}
	if !reflect.DeepEqual(words.RequestPrefixes, []string{"ask the agent", "hey agent"}) {
		t.Fatalf("prefixes = %#v", words.RequestPrefixes)
	}

	if err := f.settings.SetMany(ctx, map[string]any{
		wording.ControlPhrasesKey("test.clock"):   []string{"shared words"},
		wording.ControlPhrasesKey("test.weather"): []string{"shared words"},
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	words = f.manager.Routing()
	if owner, ok := words.ControlPhrases["shared words"]; ok {
		t.Fatalf("ambiguous phrase routed to %q, want it dropped", owner)
	}
	if _, ok := words.ControlPhrases["what time is it"]; ok {
		t.Fatal("replaced phrase still routes")
	}
}

func TestUnavailableMessageAppliesOnTheNextRead(t *testing.T) {
	f := newFixture(t, nil, nil)
	ctx := context.Background()
	if got := f.manager.UnavailableMessage(); got != "Sorry, I can't answer that right now." {
		t.Fatalf("initial = %q", got)
	}
	if _, err := f.settings.Set(ctx, wording.UnavailableKey, "Try again in a moment."); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := f.manager.UnavailableMessage(); got != "Try again in a moment." {
		t.Fatalf("after Set = %q", got)
	}
	if _, err := f.settings.Reset(ctx, wording.UnavailableKey); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := f.manager.UnavailableMessage(); got != "Sorry, I can't answer that right now." {
		t.Fatalf("after Reset = %q", got)
	}
}

// fakeEmbedder maps phrases to directions so a test can tell which example set
// is live. Phrases containing "new" point one way, rain points the opposite
// way, and everything else points at the old clock example.
type fakeEmbedder struct {
	mu       sync.Mutex
	calls    int
	nextGate chan struct{}
	failErr  error
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	gate := f.nextGate
	f.nextGate = nil
	failErr := f.failErr
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failErr != nil {
		return nil, failErr
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		lower := strings.ToLower(text)
		switch {
		case strings.Contains(lower, "new"):
			vectors[i] = []float32{0, 1}
		case strings.Contains(lower, "rain"):
			vectors[i] = []float32{-1, 0}
		default:
			vectors[i] = []float32{1, 0}
		}
	}
	return vectors, nil
}

// gateNext holds the next Embed call open until the returned channel closes.
// Tests call it before the write that triggers the rebuild.
func (f *fakeEmbedder) gateNext() chan struct{} {
	gate := make(chan struct{})
	f.mu.Lock()
	f.nextGate = gate
	f.mu.Unlock()
	return gate
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeEmbedder) waitCalls(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := f.calls
		f.mu.Unlock()
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("embed calls = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func newSemanticFixture(t *testing.T) (*fixture, *pipeline.SemanticRouter, *fakeEmbedder) {
	t.Helper()
	f := newFixture(t, nil, nil)
	embedder := &fakeEmbedder{}
	router := pipeline.NewSemanticRouter(f.registry, embedder, pipeline.SemanticConfig{
		Threshold: 0.7,
		Margin:    0,
		Timeout:   time.Second,
	})
	router.SetExamplesSource(f.manager)
	examples := f.manager.Examples()
	set, err := router.BuildSet(context.Background(), examples)
	if err != nil {
		t.Fatalf("BuildSet: %v", err)
	}
	router.Swap(set)
	f.manager.MarkExamplesBuilt(examples)
	f.manager.Start(context.Background(), router)
	return f, router, embedder
}

func TestExamplesRebuildKeepsOldVectorsUntilTheSwap(t *testing.T) {
	f, router, embedder := newSemanticFixture(t)
	ctx := context.Background()

	// Before the change: the old example routes, the new wording does not.
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("old phrase routed to %q", got.Tool)
	}
	if got := router.Match(ctx, "tell me the new time"); got.Tool != "" {
		t.Fatalf("unreleased wording routed to %q", got.Tool)
	}

	gate := embedder.gateNext()
	before := embedder.callCount()
	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the new time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	embedder.waitCalls(t, before+1)

	// The rebuild is blocked in the embedder: the old vectors must still route.
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("old vectors stopped routing during the rebuild: %q", got.Tool)
	}
	if got := router.Match(ctx, "tell me the new time"); got.Tool != "" {
		t.Fatalf("new vectors visible before the swap: %q", got.Tool)
	}

	close(gate)
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.Match(ctx, "tell me the new time"); got.Tool != "test.clock" {
		t.Fatalf("new phrase routed to %q after the swap", got.Tool)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("replaced example still routes: %q", got.Tool)
	}

	// Reset restores the code Examples and rebuilds again.
	resetGate := embedder.gateNext()
	beforeReset := embedder.callCount()
	if _, err := f.settings.Reset(ctx, wording.ExamplesKey("test.clock")); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	embedder.waitCalls(t, beforeReset+1)
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("reset swapped before the rebuild was ready: %q", got.Tool)
	}
	close(resetGate)
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("reset Examples did not restore the code default: %q", got.Tool)
	}
}

func TestExamplesRebuildDiscardsAStaleSet(t *testing.T) {
	f, router, embedder := newSemanticFixture(t)
	ctx := context.Background()

	// First change starts an embed that the test holds open.
	first := embedder.gateNext()
	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the new time"}); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	embedder.waitCalls(t, 2)

	// A newer override arrives while the first rebuild is still embedding.
	second := embedder.gateNext()
	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the newest time"}); err != nil {
		t.Fatalf("Set second: %v", err)
	}

	// Release the first build: it is stale and must be discarded, not swapped.
	close(first)
	embedder.waitCalls(t, 3)
	if got := router.Match(ctx, "tell me the new time"); got.Tool != "" {
		t.Fatalf("a stale set was installed: %q", got.Tool)
	}
	if got := router.Match(ctx, "quite what time is it"); got.Tool != "test.clock" {
		t.Fatalf("old vectors were dropped during the stale rebuild: %q", got.Tool)
	}

	close(second)
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.Match(ctx, "tell me the newest time"); got.Tool != "test.clock" {
		t.Fatalf("newest Examples did not take over: %q", got.Tool)
	}
}

func TestExamplesRebuildFailureKeepsOldVectors(t *testing.T) {
	f, router, embedder := newSemanticFixture(t)
	ctx := context.Background()

	// The Embed calls fail while the test holds failErr; the old index must
	// stay live. Clear the failure before matching, because Match also embeds
	// its query through the same fake.
	embedder.mu.Lock()
	embedder.failErr = errors.New("embed service down")
	embedder.mu.Unlock()

	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the new time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	embedder.mu.Lock()
	embedder.failErr = nil
	embedder.mu.Unlock()
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("old vectors were lost after a failed rebuild: %q", got.Tool)
	}
}

func TestFillerReSynthesizesOnlyWhenTheTextChanges(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	failText := ""
	synth := func(_ context.Context, text string) (*speech.Filler, error) {
		mu.Lock()
		texts = append(texts, text)
		shouldFail := text == failText
		mu.Unlock()
		if shouldFail {
			return nil, errors.New("tts down")
		}
		return &speech.Filler{Text: text, PCM: []byte(text)}, nil
	}
	initial := &speech.Filler{Text: "One moment.", PCM: []byte("initial")}
	f := newFixture(t, initial, synth)
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	if got := f.manager.Filler().Text; got != "One moment." {
		t.Fatalf("initial filler = %q", got)
	}

	if _, err := f.settings.Set(ctx, wording.FillerKey, "Two moments."); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := f.manager.Filler().Text; got != "Two moments." {
		t.Fatalf("after Set = %q", got)
	}

	// Same text again: no second synthesis.
	if _, err := f.settings.Set(ctx, wording.FillerKey, "Two moments."); err != nil {
		t.Fatalf("Set same: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	mu.Lock()
	count := len(texts)
	mu.Unlock()
	if count != 1 {
		t.Fatalf("synthesis calls = %d, want one for one text change", count)
	}

	// A failed synthesis keeps the old audio and does not retry the same text.
	mu.Lock()
	failText = "Bad phrase."
	mu.Unlock()
	if _, err := f.settings.Set(ctx, wording.FillerKey, "Bad phrase."); err != nil {
		t.Fatalf("Set failing: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := f.manager.Filler().Text; got != "Two moments." {
		t.Fatalf("failed synthesis replaced the filler with %q", got)
	}
	if _, err := f.settings.Set(ctx, wording.FillerKey, "Bad phrase."); err != nil {
		t.Fatalf("Set same failing: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	mu.Lock()
	count = len(texts)
	failText = ""
	mu.Unlock()
	if count != 2 {
		t.Fatalf("synthesis calls = %d, want no retry for an unchanged failing text", count)
	}

	if _, err := f.settings.Set(ctx, wording.FillerKey, "Three moments."); err != nil {
		t.Fatalf("Set after recovery: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := f.manager.Filler().Text; got != "Three moments." {
		t.Fatalf("after recovery = %q", got)
	}

	// Reset returns to the config Filler and re-synthesizes that audio.
	if _, err := f.settings.Reset(ctx, wording.FillerKey); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := f.manager.Filler().Text; got != wording.DefaultFiller {
		t.Fatalf("after reset = %q, want the config/default filler", got)
	}
}

func TestManagerConcurrentReadsAndWritesAreSafe(t *testing.T) {
	f := newFixture(t, &speech.Filler{Text: "One moment.", PCM: []byte{1}}, func(_ context.Context, text string) (*speech.Filler, error) {
		return &speech.Filler{Text: text, PCM: []byte(text)}, nil
	})
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = f.manager.Routing()
				_ = f.manager.Examples()
				_ = f.manager.Filler()
				_ = f.manager.UnavailableMessage()
				if i%2 == 0 {
					if _, err := f.settings.Set(ctx, wording.FillerKey, "Moment "+string(rune('A'+j%3))); err != nil {
						t.Errorf("Set filler: %v", err)
						return
					}
					if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"phrase " + string(rune('A'+j%3))}); err != nil {
						t.Errorf("Set examples: %v", err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestBuildCollapsesDuplicateSpellingsForOneTool(t *testing.T) {
	cfg := testConfig()
	cfg.ControlPhrases = map[string]string{"now": "test.clock", "NOW!": "test.clock"}
	setup, err := wording.Build(testRegistry(t), cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var clockDef settings.Definition
	for _, def := range setup.Definitions {
		if def.Key == wording.ControlPhrasesKey("test.clock") {
			clockDef = def
		}
	}
	if clockDef.Key == "" {
		t.Fatal("test.clock has no control phrase setting")
	}
	// The config layer is applied by settings.New, which validates it; a
	// duplicate spelling would fail there. Building the service proves the
	// list was collapsed.
	if _, err := settings.New(context.Background(), openStore(t), clockDef); err != nil {
		t.Fatalf("settings.New with the deduplicated config: %v", err)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func TestManagerCloseIsIdempotentAndWaitReportsClosed(t *testing.T) {
	f := newFixture(t, nil, nil)
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	done := make(chan struct{})
	go func() {
		f.manager.Close()
		f.manager.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return")
	}
	if err := f.manager.Wait(ctx); !errors.Is(err, wording.ErrClosed) {
		t.Fatalf("Wait after Close = %v, want ErrClosed", err)
	}
}

// TestFillerReSynthesizesWhenTheVoiceOrRateChanges proves the one settings
// observer and worker also invalidate the cached cue for a voice/rate change.
// Without this, "use the jarvis voice" would leave the old voice cached forever.
func TestFillerReSynthesizesWhenTheVoiceOrRateChanges(t *testing.T) {
	var mu sync.Mutex
	var signatures []string
	current := "alba@1.00"

	synth := func(_ context.Context, text string) (*speech.Filler, error) {
		mu.Lock()
		signature := current
		signatures = append(signatures, signature)
		mu.Unlock()
		return &speech.Filler{Text: text, PCM: []byte(signature)}, nil
	}
	extra := []settings.Definition{
		settings.String("voice.name", "alba"),
		settings.Float("voice.rate", 1.0),
	}
	signature := func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	initial := &speech.Filler{Text: "One moment.", PCM: []byte("alba@1.00")}
	f := newFixtureWith(t, initial, synth, extra,
		wording.WithFillerKeys("voice.name", "voice.rate"),
		wording.WithFillerSignature(signature))
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	mu.Lock()
	current = "jarvis@1.00"
	mu.Unlock()
	if _, err := f.settings.Set(ctx, "voice.name", "jarvis"); err != nil {
		t.Fatalf("Set voice: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := string(f.manager.Filler().PCM); got != "jarvis@1.00" {
		t.Fatalf("filler after voice change = %q", got)
	}

	mu.Lock()
	current = "jarvis@1.20"
	mu.Unlock()
	if _, err := f.settings.Set(ctx, "voice.rate", 1.2); err != nil {
		t.Fatalf("Set rate: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := string(f.manager.Filler().PCM); got != "jarvis@1.20" {
		t.Fatalf("filler after rate change = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(signatures) != 2 {
		t.Fatalf("synthesis calls = %d, want one per signature change", len(signatures))
	}
}
