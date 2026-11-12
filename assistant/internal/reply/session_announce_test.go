package reply_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// hookedSpeaker reacts to a phrase being played, standing in for something
// that finishes in the background while the Assistant is mid-turn.
type hookedSpeaker struct {
	inner   *fakeSpeaker
	onSpeak func(string)
}

func (s *hookedSpeaker) SayText(ctx context.Context, text string) error {
	if s.onSpeak != nil {
		s.onSpeak(text)
	}
	return s.inner.SayText(ctx, text)
}

// confirmationQueueSession wires the real Confirmation flow and the shared
// Announcement queue the way main does: the queue reads the registry's pending
// call as its blocked gate, and the session holds the queue for each turn.
func confirmationQueueSession(t *testing.T, onSpeak func(string)) (*reply.Session, *fakeSpeaker, *announce.Queue, *tool.Registry) {
	t.Helper()
	logger := discardLogger()
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	type mailIn struct {
		To string `json:"to"`
	}
	type mailOut struct {
		Status string `json:"status"`
	}
	if err := tool.Add(reg, tool.Tool[mailIn, mailOut]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Summary:     func(in mailIn) string { return "Send the email to " + in.To + "?" },
		Handler: func(context.Context, mailIn) (mailOut, error) {
			return mailOut{Status: "sent"}, nil
		},
		Speak: func(out mailOut) string { return "Email " + out.Status + "." },
	}); err != nil {
		t.Fatalf("register mail.send: %v", err)
	}

	speaker := newFakeSpeaker()
	queue := announce.New(speaker, logger, announce.WithBlocked(func() bool {
		_, pending := reg.Pending()
		return pending
	}))
	t.Cleanup(queue.Close)

	runner := &proposingRunner{reg: reg, history: agent.NewHistory(10, 5*time.Minute, nil)}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger,
		pipeline.WithAgent(runner, []string{"ask the agent"}),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	hook := &hookedSpeaker{inner: speaker, onSpeak: onSpeak}
	return reply.New(p, hook, nil, nil, reply.WithAnnouncements(queue)), speaker, queue, reg
}

func TestAnnouncementQueuedDuringAReplyPlaysAfterItFinishes(t *testing.T) {
	speaker := newFakeSpeaker()
	queue := announce.New(speaker, discardLogger())
	t.Cleanup(queue.Close)

	inTurn := make(chan struct{})
	release := make(chan struct{})
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		queue.Enqueue("The bins go out tonight.")
		close(inTurn)
		<-release
		return pipeline.Reply{Text: "It's 3:04 PM.", Path: tool.PathControlPhrase}
	}
	session := reply.New(routed, speaker, nil, nil, reply.WithAnnouncements(queue))

	done := make(chan error, 1)
	go func() {
		_, err := session.Ask(context.Background(), request(), "what time is it")
		done <- err
	}()
	<-inTurn
	select {
	case phrase := <-speaker.saidCh:
		t.Fatalf("spoke %q while the reply was in flight", phrase)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Ask: %v", err)
	}

	select {
	case phrase := <-speaker.saidCh:
		if phrase != "It's 3:04 PM." {
			t.Fatalf("first phrase = %q, want the reply", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply was never spoken")
	}
	select {
	case phrase := <-speaker.saidCh:
		if phrase != "The bins go out tonight." {
			t.Fatalf("second phrase = %q, want the announcement", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the announcement did not play after the reply")
	}
}

func TestAnnouncementQueuedWhileAConfirmationPendsWaitsForIt(t *testing.T) {
	const reminder = "Hermes finished your job."
	var queue *announce.Queue
	session, speaker, built, reg := confirmationQueueSession(t, func(text string) {
		// A background job finishes while the Confirmation question is being
		// spoken: the announcement is queued inside the reply turn.
		if text == "Send the email to doctor?" {
			queue.Enqueue(reminder)
		}
	})
	queue = built

	if _, err := session.Ask(context.Background(), request(), "email my doctor"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if pending, ok := reg.Pending(); !ok || pending.Tool != "mail.send" {
		t.Fatalf("pending = %+v, %v, want mail.send", pending, ok)
	}
	// Drain the question, then prove the queued announcement stays silent
	// while the Confirmation is still waiting for an answer.
	select {
	case phrase := <-speaker.saidCh:
		if phrase != "Send the email to doctor?" {
			t.Fatalf("spoken = %q, want the Confirmation question", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the Confirmation question was never spoken")
	}
	select {
	case phrase := <-speaker.saidCh:
		t.Fatalf("spoke %q while the Confirmation was pending", phrase)
	case <-time.After(150 * time.Millisecond):
	}

	if _, err := session.Ask(context.Background(), request(), "yes"); err != nil {
		t.Fatalf("Ask yes: %v", err)
	}
	if _, ok := reg.Pending(); ok {
		t.Fatal("pending survived the yes")
	}
	select {
	case phrase := <-speaker.saidCh:
		if phrase != "Email sent." {
			t.Fatalf("spoken = %q, want the confirmed reply first", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the confirmed reply was never spoken")
	}
	select {
	case phrase := <-speaker.saidCh:
		if phrase != reminder {
			t.Fatalf("spoken = %q, want the queued announcement", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the announcement did not play after the Confirmation resolved")
	}
	if got, want := speaker.phrases(), []string{"Send the email to doctor?", "Email sent.", reminder}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

// TestAnnouncementQueuedByTheProposingTurnWaitsForTheConfirmation is the race
// where the announcement arrives before the call is proposed, so the queue must
// re-read the pending call after the turn instead of trusting an idle check.
func TestAnnouncementQueuedByTheProposingTurnWaitsForTheConfirmation(t *testing.T) {
	const reminder = "The wash is done."
	speaker := newFakeSpeaker()

	reg := tool.NewRegistry(time.Second)
	type mailIn struct {
		To string `json:"to"`
	}
	type mailOut struct {
		Status string `json:"status"`
	}
	if err := tool.Add(reg, tool.Tool[mailIn, mailOut]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Summary:     func(in mailIn) string { return "Send the email to " + in.To + "?" },
		Handler: func(context.Context, mailIn) (mailOut, error) {
			return mailOut{Status: "sent"}, nil
		},
		Speak: func(out mailOut) string { return "Email " + out.Status + "." },
	}); err != nil {
		t.Fatalf("register mail.send: %v", err)
	}

	// The queue's blocked gate reads the registry, exactly as main wires it.
	queue := announce.New(speaker, discardLogger(), announce.WithBlocked(func() bool {
		_, pending := reg.Pending()
		return pending
	}))
	t.Cleanup(queue.Close)

	// The runner enqueues before it proposes, so the queue wakes while the turn
	// is still running and must not play once the call is armed.
	runner := &enqueueingRunner{reg: reg, queue: queue, text: reminder, history: agent.NewHistory(10, 5*time.Minute, nil)}
	p, err := pipeline.New(reg, nil, discardLogger(),
		pipeline.WithAgent(runner, []string{"ask the agent"}),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, discardLogger())),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	session := reply.New(p, speaker, nil, nil, reply.WithAnnouncements(queue))

	if _, err := session.Ask(context.Background(), request(), "email my doctor"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, ok := reg.Pending(); !ok {
		t.Fatal("the proposing turn armed no Confirmation")
	}
	select {
	case phrase := <-speaker.saidCh:
		if phrase != "Send the email to doctor?" {
			t.Fatalf("spoken = %q, want the Confirmation question", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the Confirmation question was never spoken")
	}
	select {
	case phrase := <-speaker.saidCh:
		t.Fatalf("spoke %q with the Confirmation still pending", phrase)
	case <-time.After(150 * time.Millisecond):
	}

	if _, err := session.Ask(context.Background(), request(), "yes"); err != nil {
		t.Fatalf("Ask yes: %v", err)
	}
	select {
	case phrase := <-speaker.saidCh:
		if phrase != "Email sent." {
			t.Fatalf("spoken = %q, want the confirmed reply first", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the confirmed reply was never spoken")
	}
	select {
	case phrase := <-speaker.saidCh:
		if phrase != reminder {
			t.Fatalf("spoken = %q, want the queued announcement", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the announcement did not play after the Confirmation resolved")
	}
	if got, want := speaker.phrases(), []string{"Send the email to doctor?", "Email sent.", reminder}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

// enqueueingRunner queues an announcement and then arms a confirm Tool in the
// same turn.
type enqueueingRunner struct {
	reg     *tool.Registry
	queue   *announce.Queue
	text    string
	history *agent.History
}

func (r *enqueueingRunner) Run(_ context.Context, _ tool.Request, utterance string, _ func(string)) agent.Result {
	if utterance != "email my doctor" {
		return agent.Result{Text: "I can answer that."}
	}
	r.queue.Enqueue(r.text)
	pending, err := r.reg.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`))
	if err != nil {
		return agent.Result{Text: "could not propose"}
	}
	return agent.Result{Text: pending.Summary, Pending: &pending}
}

func (r *enqueueingRunner) History() *agent.History { return r.history }
