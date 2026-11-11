package reply_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// eventLog records playback and streaming in the order they happen.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// logSink records a filler playback in the shared log.
type logSink struct{ log *eventLog }

func (s logSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return logWriter{log: s.log}, nil
}

type logWriter struct{ log *eventLog }

func (w logWriter) Write(p []byte) (int, error) {
	w.log.add("filler-audio")
	return len(p), nil
}

func (w logWriter) Close() error { return nil }

// fakeSpeaker records every phrase it is asked to speak.
type fakeSpeaker struct {
	mu     sync.Mutex
	said   []string
	saidCh chan string
	err    error
}

func newFakeSpeaker() *fakeSpeaker {
	return &fakeSpeaker{saidCh: make(chan string, 8)}
}

func (f *fakeSpeaker) SayText(_ context.Context, text string) error {
	f.mu.Lock()
	f.said = append(f.said, text)
	f.mu.Unlock()
	f.saidCh <- text
	return f.err
}

func (f *fakeSpeaker) phrases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.said...)
}

// fakePipeline stands in for the routing pipeline at the session seam.
type fakePipeline struct {
	handle func(events pipeline.Events) pipeline.Reply
}

func (f *fakePipeline) HandleEvents(_ context.Context, _ tool.Request, _ string, events pipeline.Events) pipeline.Reply {
	return f.handle(events)
}

func testFiller() *speech.Filler {
	return &speech.Filler{
		Text:   "One moment.",
		Format: audio.Format{Rate: 24000, Width: 2, Channels: 1},
		PCM:    []byte{1, 2, 3, 4},
	}
}

func request() tool.Request {
	return tool.Request{ID: "test", Device: tool.DevicePi}
}

func TestAgentAnswerStartsPlayingBeforeGenerationEnds(t *testing.T) {
	log := &eventLog{}
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		events.Delta("The forecast is fine today, but it could rain later.")
		// Generation is still running; the first chunk must already be playing.
		select {
		case chunk := <-speaker.saidCh:
			log.add("streamed:" + chunk)
		case <-time.After(2 * time.Second):
			t.Error("reply audio did not start before generation finished")
		}
		events.Delta(" It is sunny for now.")
		return pipeline.Reply{Text: "The forecast is fine today, but it could rain later. It is sunny for now."}
	}

	session := reply.New(routed, speaker, logSink{log}, testFiller())
	result, err := session.Ask(context.Background(), request(), "will it rain")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Text != "The forecast is fine today, but it could rain later. It is sunny for now." {
		t.Fatalf("reply = %q", result.Text)
	}
	want := []string{"The forecast is fine today,", "but it could rain later.", "It is sunny for now."}
	if got := speaker.phrases(); !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	// The cached filler plays first, then the streamed chunk.
	wantLog := []string{"filler-audio", "streamed:The forecast is fine today,"}
	if got := log.all(); !equal(got, wantLog) {
		t.Fatalf("order = %q, want %q", got, wantLog)
	}
}

func TestAgentFillerPlaysOncePerRequest(t *testing.T) {
	log := &eventLog{}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		events.AgentPath()
		events.Delta("It is sunny.")
		return pipeline.Reply{Text: "It is sunny."}
	}

	session := reply.New(routed, newFakeSpeaker(), logSink{log}, testFiller())
	if _, err := session.Ask(context.Background(), request(), "will it rain"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := log.all(); !equal(got, []string{"filler-audio"}) {
		t.Fatalf("filler played %q, want exactly one", got)
	}
}

func TestFastPathReplyIsSpokenWithoutAFiller(t *testing.T) {
	log := &eventLog{}
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{Text: "It's 3:04 PM.", Path: tool.PathControlPhrase}
	}

	session := reply.New(routed, speaker, logSink{log}, testFiller())
	if _, err := session.Ask(context.Background(), request(), "what time is it"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := speaker.phrases(); !equal(got, []string{"It's 3:04 PM."}) {
		t.Fatalf("spoken = %q", got)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("filler played on the Fast path: %q", got)
	}
}

func TestPlaybackFailureReturnsTheReplyAndTheError(t *testing.T) {
	speaker := newFakeSpeaker()
	speaker.err = errors.New("tts is down")
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{Text: "It is sunny."}
	}

	session := reply.New(routed, speaker, logSink{&eventLog{}}, testFiller())
	result, err := session.Ask(context.Background(), request(), "will it rain")
	if err == nil {
		t.Fatal("Ask succeeded, want the playback error")
	}
	if result.Text != "It is sunny." {
		t.Fatalf("reply = %q, want the text kept", result.Text)
	}
}

func TestSessionWithoutASpeakerIsTextOnly(t *testing.T) {
	log := &eventLog{}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		if events.Delta != nil {
			events.Delta("It is sunny.")
		}
		return pipeline.Reply{Text: "It is sunny."}
	}

	session := reply.New(routed, nil, logSink{log}, testFiller())
	result, err := session.Ask(context.Background(), request(), "will it rain")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Text != "It is sunny." || len(log.all()) != 0 {
		t.Fatalf("reply = %q, log = %q", result.Text, log.all())
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestCancelledPrefixIsSpokenBeforeAStreamedAnswer(t *testing.T) {
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.Prefix("Cancelled.")
		events.AgentPath()
		events.Delta("It is sunny.")
		return pipeline.Reply{Text: "Cancelled. It is sunny.", Prefix: "Cancelled."}
	}

	session := reply.New(routed, speaker, logSink{&eventLog{}}, testFiller())
	if _, err := session.Ask(context.Background(), request(), "no"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got, want := speaker.phrases(), []string{"Cancelled.", "It is sunny."}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

func TestCancelledSilenceIsSpokenExactlyOnce(t *testing.T) {
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.Prefix("Cancelled.")
		return pipeline.Reply{Text: "Cancelled.", Prefix: "Cancelled.", Path: tool.PathConfirmation}
	}

	session := reply.New(routed, speaker, nil, nil)
	if _, err := session.Ask(context.Background(), request(), "  "); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got, want := speaker.phrases(), []string{"Cancelled."}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

func TestConfirmationQuestionIsSpokenAfterModelPreamble(t *testing.T) {
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		events.Delta("Sure, I can do that.")
		return pipeline.Reply{
			Text:    "Send the email to doctor?",
			Pending: &agent.PendingConfirm{Tool: "mail.send"},
		}
	}

	session := reply.New(routed, speaker, logSink{&eventLog{}}, testFiller())
	if _, err := session.Ask(context.Background(), request(), "email my doctor"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got, want := speaker.phrases(), []string{"Sure, I can do that.", "Send the email to doctor?"}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

// timingSpeaker stands in for speech.Speaker: the production Speaker records
// the first actual reply sample and the TTS stage on the request timeline.
type timingSpeaker struct{}

func (timingSpeaker) SayText(ctx context.Context, _ string) error {
	latency.SetStage(ctx, latency.StageTTSFirstAudio, time.Millisecond)
	latency.MarkReplyAudio(ctx)
	return nil
}

// blockingSpeaker holds playback until the request is cancelled.
type blockingSpeaker struct{ started chan struct{} }

func (s blockingSpeaker) SayText(ctx context.Context, _ string) error {
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

func oneRequestLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var found map[string]any
	for _, line := range lines {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if parsed["msg"] == "request" {
			if found != nil {
				t.Fatalf("more than one request line:\n%s", logs.String())
			}
			found = parsed
		}
	}
	if found == nil {
		t.Fatalf("no request line logged:\n%s", logs.String())
	}
	return found
}

func TestAskLogsOneRequestLineWithStagesAndResponseLatency(t *testing.T) {
	timeline := latency.New("req-27", latency.SourceVoice)
	timeline.SetLastVoice(time.Now().Add(-300 * time.Millisecond))
	ctx := latency.WithContext(context.Background(), timeline)

	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		latency.SetStage(ctx, latency.StageRouting, 20*time.Millisecond)
		latency.AddStage(ctx, latency.StageToolExecute, 5*time.Millisecond)
		latency.SetRoute(ctx, "fast_path", "time.now", "what time is it")
		return pipeline.Reply{Text: "It's 3:04 PM.", Path: tool.PathFast, Tool: "time.now", RequestID: "req-27"}
	}

	var logs bytes.Buffer
	session := reply.New(routed, timingSpeaker{}, nil, nil,
		reply.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	if _, err := session.Ask(ctx, request(), "what time is it"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	line := oneRequestLine(t, &logs)
	if line["request_id"] != "req-27" || line["outcome"] != "ok" || line["path"] != "fast_path" {
		t.Fatalf("unexpected request line: %v", line)
	}
	response, ok := line["response_ms"].(float64)
	if !ok || response < 300 {
		t.Fatalf("response_ms = %v, want at least 300 (since last voiced frame)", line["response_ms"])
	}
	stages := line["stages"].(map[string]any)
	if stages["routing_ms"] != float64(20) || stages["tool_execute_ms"] != float64(5) {
		t.Fatalf("stages = %v", stages)
	}
	if _, exists := stages["endpointing_ms"]; exists {
		t.Fatalf("a fabricated stage leaked into the line: %v", stages)
	}
	checkpoints := line["checkpoints"].(map[string]any)
	if _, ok := checkpoints["first_chunk_ms"]; !ok {
		t.Fatalf("first_chunk checkpoint missing: %v", line)
	}
}

func TestAskLogsCancelledWithoutResponseLatency(t *testing.T) {
	timeline := latency.New("req-cancel", latency.SourceVoice)
	timeline.SetLastVoice(time.Now().Add(-2 * time.Second))
	ctx, cancel := context.WithCancel(latency.WithContext(context.Background(), timeline))

	speaker := blockingSpeaker{started: make(chan struct{})}
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{Text: "A long answer.", Path: tool.PathAgent, RequestID: "req-cancel"}
	}

	var logs bytes.Buffer
	session := reply.New(routed, speaker, nil, nil,
		reply.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	go func() {
		<-speaker.started
		cancel()
	}()
	_, err := session.Ask(ctx, request(), "tell me a story")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask error = %v, want context.Canceled", err)
	}

	line := oneRequestLine(t, &logs)
	if line["outcome"] != "cancelled" {
		t.Fatalf("outcome = %v, want cancelled", line["outcome"])
	}
	if _, exists := line["response_ms"]; exists {
		t.Fatalf("cancelled turn reported response latency: %v", line)
	}
}

// TestAskLogsAFailedAgentTurnAsFailedEvenWhenItSpoke keeps an Unavailable
// message out of the successful-response samples.
func TestAskLogsAFailedAgentTurnAsFailedEvenWhenItSpoke(t *testing.T) {
	timeline := latency.New("req-failed", latency.SourceVoice)
	timeline.SetLastVoice(time.Now().Add(-300 * time.Millisecond))
	ctx := latency.WithContext(context.Background(), timeline)

	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		latency.MarkFailed(ctx)
		latency.SetAttr(ctx, "agent_error", "unavailable")
		return pipeline.Reply{Text: "Sorry, I can't answer that right now.", Path: tool.PathAgent, RequestID: "req-failed"}
	}

	var logs bytes.Buffer
	session := reply.New(routed, timingSpeaker{}, nil, nil,
		reply.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if _, err := session.Ask(ctx, request(), "ask the agent anything"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	line := oneRequestLine(t, &logs)
	if line["outcome"] != "failed" {
		t.Fatalf("outcome = %v, want failed", line["outcome"])
	}
	if line["agent_error"] != "unavailable" {
		t.Fatalf("agent_error = %v", line["agent_error"])
	}
	// The timing is real evidence and is still logged; the summarizer is what
	// keeps failed turns out of the success percentiles.
	if _, ok := line["response_ms"]; !ok {
		t.Fatal("failed turn lost its measured response latency")
	}
}
