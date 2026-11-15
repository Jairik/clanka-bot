package reply_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestUnstreamedFallbackIsSpokenAfterAStreamedPreamble covers every
// agent.Result fallback whose Text no delta carried (#193).
func TestUnstreamedFallbackIsSpokenAfterAStreamedPreamble(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		unavailable bool
	}{
		{name: "step limit", text: agent.MaxStepsMessage},
		{name: "empty answer", text: agent.EmptyReplyMessage},
		{name: "first token timeout", text: agent.FirstTokenTimeoutMessage},
		{name: "unavailable", text: "Sorry, I can't answer that right now.", unavailable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			speaker := newFakeSpeaker()
			routed := &fakePipeline{}
			routed.handle = func(events pipeline.Events) pipeline.Reply {
				events.AgentPath()
				events.Delta("Let me check that.")
				return pipeline.Reply{
					Text:            test.text,
					Unavailable:     test.unavailable,
					TextNotStreamed: true,
				}
			}

			session := reply.New(routed, speaker, nil, nil)
			if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			want := []string{"Let me check that.", test.text}
			if got := speaker.phrases(); !equal(got, want) {
				t.Fatalf("spoken = %q, want %q", got, want)
			}
		})
	}
}

// TestUnavailableCuePlaysAfterAStreamedPreamble keeps the error Cue ordered
// before the Unavailable line even when the preamble already streamed (#193).
func TestUnavailableCuePlaysAfterAStreamedPreamble(t *testing.T) {
	log := &eventLog{}
	cues := &fakeCues{
		clip: sound.Clip{PCM: []byte{1, 2, 3, 4}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}},
		ok:   true,
	}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.Delta("Let me check that.")
		return pipeline.Reply{
			Text:            "Sorry, I can't answer that right now.",
			Unavailable:     true,
			TextNotStreamed: true,
		}
	}

	session := reply.New(routed, speakingLog{log}, cueSink{log}, nil, reply.WithCues(cues))
	if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	want := []string{"say:Let me check that.", "error-cue", "say:Sorry, I can't answer that right now."}
	if got := log.all(); !equal(got, want) {
		t.Fatalf("playback order = %q, want %q", got, want)
	}
	if len(cues.roles) != 1 || cues.roles[0] != sound.RoleError {
		t.Fatalf("cue roles = %v, want only the error role", cues.roles)
	}
}

// TestStreamedAnswerIsNotRepeated keeps a normal or multi-step answer from
// being spoken twice now that unstreamed fallbacks are always said (#193).
func TestStreamedAnswerIsNotRepeated(t *testing.T) {
	tests := []struct {
		name   string
		deltas []string
		text   string
		want   []string
	}{
		{
			name:   "normal answer",
			deltas: []string{"It is sunny."},
			text:   "It is sunny.",
			want:   []string{"It is sunny."},
		},
		{
			name:   "multi-step preamble",
			deltas: []string{"Let me check that. ", "It is sunny. "},
			text:   "It is sunny.",
			want:   []string{"Let me check that.", "It is sunny."},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			speaker := newFakeSpeaker()
			routed := &fakePipeline{}
			routed.handle = func(events pipeline.Events) pipeline.Reply {
				events.AgentPath()
				for _, delta := range test.deltas {
					events.Delta(delta)
				}
				return pipeline.Reply{Text: test.text}
			}

			session := reply.New(routed, speaker, nil, nil)
			if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if got := speaker.phrases(); !equal(got, test.want) {
				t.Fatalf("spoken = %q, want %q", got, test.want)
			}
		})
	}
}

// resultAgent scripts one Agent turn for the real pipeline.
type resultAgent struct {
	history *agent.History
	stream  []string
	result  agent.Result
}

func (a *resultAgent) Run(_ context.Context, _ tool.Request, _ string, onDelta func(string)) agent.Result {
	for _, delta := range a.stream {
		if onDelta != nil {
			onDelta(delta)
		}
	}
	return a.result
}

func (a *resultAgent) History() *agent.History { return a.history }

// resultSpeaker records each phrase and marks real reply audio for the
// request outcome.
type resultSpeaker struct{ *fakeSpeaker }

func (s resultSpeaker) SayText(ctx context.Context, text string) error {
	latency.MarkReplyAudio(ctx)
	return s.fakeSpeaker.SayText(ctx, text)
}

// TestResultVariantsSpeakOnceAndLogTheirOutcome drives the real pipeline and
// reply session for every agent.Result shape: the exact phrase list and the
// request-line outcome must both be right (#193).
func TestResultVariantsSpeakOnceAndLogTheirOutcome(t *testing.T) {
	const preamble = "Let me check that. "
	tests := []struct {
		name           string
		stream         []string
		result         agent.Result
		wantPhrases    []string
		wantOutcome    string
		wantAgentError string
	}{
		{
			name:        "normal answer",
			stream:      []string{"It is sunny. "},
			result:      agent.Result{Text: "It is sunny."},
			wantPhrases: []string{"It is sunny."},
			wantOutcome: "ok",
		},
		{
			name:        "preamble then answer",
			stream:      []string{preamble, "It is sunny. "},
			result:      agent.Result{Text: "It is sunny."},
			wantPhrases: []string{"Let me check that.", "It is sunny."},
			wantOutcome: "ok",
		},
		{
			name:        "pending confirmation",
			stream:      []string{preamble},
			result:      agent.Result{Text: "Send the email?", Pending: &agent.PendingConfirm{Tool: "mail.send"}},
			wantPhrases: []string{"Let me check that.", "Send the email?"},
			wantOutcome: "ok",
		},
		{
			name:           "step limit",
			stream:         []string{preamble},
			result:         agent.Result{Text: agent.MaxStepsMessage, MaxSteps: true, TextNotStreamed: true},
			wantPhrases:    []string{"Let me check that.", agent.MaxStepsMessage},
			wantOutcome:    "failed",
			wantAgentError: "max_steps",
		},
		{
			name:        "empty answer",
			stream:      []string{preamble},
			result:      agent.Result{Text: agent.EmptyReplyMessage, TextNotStreamed: true},
			wantPhrases: []string{"Let me check that.", agent.EmptyReplyMessage},
			wantOutcome: "ok",
		},
		{
			name:           "first token timeout",
			stream:         []string{preamble},
			result:         agent.Result{Text: agent.FirstTokenTimeoutMessage, TimedOut: true, TextNotStreamed: true},
			wantPhrases:    []string{"Let me check that.", agent.FirstTokenTimeoutMessage},
			wantOutcome:    "failed",
			wantAgentError: "timeout",
		},
		{
			name:           "provider failure",
			stream:         []string{preamble},
			result:         agent.Result{Text: "Sorry, I can't answer that right now.", Unavailable: true, TextNotStreamed: true},
			wantPhrases:    []string{"Let me check that.", "Sorry, I can't answer that right now."},
			wantOutcome:    "failed",
			wantAgentError: "unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			timeline := latency.New("req-"+strings.ReplaceAll(test.name, " ", "-"), latency.SourceVoice)
			ctx := latency.WithContext(context.Background(), timeline)
			speaker := resultSpeaker{newFakeSpeaker()}
			runner := &resultAgent{
				history: agent.NewHistory(10, time.Minute, nil),
				stream:  test.stream,
				result:  test.result,
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger, pipeline.WithAgent(runner, nil))
			if err != nil {
				t.Fatalf("pipeline.New: %v", err)
			}
			session := reply.New(p, speaker, nil, nil, reply.WithLogger(logger))

			if _, err := session.Ask(ctx, request(), "ask the agent anything"); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if got := speaker.phrases(); !equal(got, test.wantPhrases) {
				t.Fatalf("spoken = %q, want %q", got, test.wantPhrases)
			}
			line := oneRequestLine(t, &logs)
			if line["outcome"] != test.wantOutcome {
				t.Fatalf("outcome = %v, want %q in %v", line["outcome"], test.wantOutcome, line)
			}
			if test.wantAgentError == "" {
				if _, ok := line["agent_error"]; ok {
					t.Fatalf("agent_error = %v, want none in %v", line["agent_error"], line)
				}
				return
			}
			if line["agent_error"] != test.wantAgentError {
				t.Fatalf("agent_error = %v, want %q in %v", line["agent_error"], test.wantAgentError, line)
			}
		})
	}
}

// cancellingAgent streams a preamble, waits until it is heard, then cancels the
// turn the way Barge-in does.
type cancellingAgent struct {
	history *agent.History
	heard   <-chan string
	cancel  context.CancelFunc
}

func (a *cancellingAgent) Run(ctx context.Context, _ tool.Request, _ string, onDelta func(string)) agent.Result {
	onDelta("Let me check that. ")
	select {
	case <-a.heard:
	case <-ctx.Done():
	}
	a.cancel()
	return agent.Result{Cancelled: true}
}

func (a *cancellingAgent) History() *agent.History { return a.history }

// TestCancelledAgentAfterAPreambleNeverSpeaksUnavailable keeps Barge-in silent:
// the user interrupted, so no Unavailable line follows the fragment (#193).
func TestCancelledAgentAfterAPreambleNeverSpeaksUnavailable(t *testing.T) {
	speaker := newFakeSpeaker()
	timeline := latency.New("req-cancelled", latency.SourceVoice)
	ctx, cancel := context.WithCancel(latency.WithContext(context.Background(), timeline))
	defer cancel()
	runner := &cancellingAgent{
		history: agent.NewHistory(10, time.Minute, nil),
		heard:   speaker.saidCh,
		cancel:  cancel,
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger, pipeline.WithAgent(runner, nil))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	session := reply.New(p, speaker, nil, nil, reply.WithLogger(logger))

	result, err := session.Ask(ctx, request(), "ask the agent anything")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Text != "" {
		t.Fatalf("reply = %q, want no text on cancellation", result.Text)
	}
	phrases := speaker.phrases()
	if want := []string{"Let me check that."}; !equal(phrases, want) {
		t.Fatalf("spoken = %q, want the preamble only", phrases)
	}
	for _, phrase := range phrases {
		if strings.Contains(phrase, "can't answer") {
			t.Fatalf("an Unavailable line followed a cancelled turn: %q", phrases)
		}
	}
	line := oneRequestLine(t, &logs)
	if line["outcome"] != "cancelled" {
		t.Fatalf("outcome = %v, want cancelled in %v", line["outcome"], line)
	}
	if _, ok := line["agent_error"]; ok {
		t.Fatalf("cancelled turn carried agent_error = %v", line["agent_error"])
	}
}
