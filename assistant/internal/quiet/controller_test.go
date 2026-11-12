package quiet_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
)

// recorder is one shared speaker/play log, so tests can assert the queue order
// across text summaries and audio previews.
type recorder struct {
	mu    sync.Mutex
	said  []string
	plays int
	ch    chan string
}

func newRecorder() *recorder { return &recorder{ch: make(chan string, 16)} }

// blockingSpeaker holds one line of speech until its context is cancelled, so
// the Barge-in test can observe the interruption.
type blockingSpeaker struct {
	started chan string
	stopped chan string
}

func (s *blockingSpeaker) SayText(ctx context.Context, text string) error {
	s.started <- text
	<-ctx.Done()
	s.stopped <- text
	return ctx.Err()
}

func (r *recorder) SayText(_ context.Context, text string) error {
	r.mu.Lock()
	r.said = append(r.said, text)
	r.mu.Unlock()
	r.ch <- "say:" + text
	return nil
}

func (r *recorder) Play(context.Context) error {
	r.mu.Lock()
	r.plays++
	r.mu.Unlock()
	r.ch <- "play"
	return nil
}

func (r *recorder) phrases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.said...)
}

// expectEvent waits for one queued event.
func expectEvent(t *testing.T, r *recorder, want string) {
	t.Helper()
	select {
	case got := <-r.ch:
		if got != want {
			t.Fatalf("event = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no %q event", want)
	}
}

func expectNoEvent(t *testing.T, r *recorder) {
	t.Helper()
	select {
	case got := <-r.ch:
		t.Fatalf("unexpected event %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestQuietHoursHoldAndReleaseAsOneSummary(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	rec := newRecorder()
	queue := announce.New(rec, nil, announce.WithHoldSummary())
	t.Cleanup(queue.Close)

	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(5 * time.Millisecond)
	controller.Start(context.Background())
	t.Cleanup(controller.Close)

	queue.Enqueue("The wash is done.")
	queue.Enqueue("The bins go out tonight.")
	expectNoEvent(t, rec)
	if queue.Len() != 2 {
		t.Fatalf("queue holds %d items, want the two held announcements", queue.Len())
	}

	fixture.clock.Set(utc(2026, 11, 13, 7, 0))
	expectEvent(t, rec, "say:While it was quiet: The wash is done. The bins go out tonight.")
	expectNoEvent(t, rec)
	if got := len(rec.phrases()); got != 1 {
		t.Fatalf("spoke %d lines, want one summary", got)
	}
	if queue.Len() != 0 {
		t.Fatalf("queue still holds %d items after the summary", queue.Len())
	}
}

func TestAudioPreviewsStayQueuedAndAreNotNarrated(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	rec := newRecorder()
	queue := announce.New(rec, nil, announce.WithHoldSummary())
	t.Cleanup(queue.Close)
	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(5 * time.Millisecond)
	controller.Start(context.Background())
	t.Cleanup(controller.Close)

	queue.Enqueue("The wash is done.")
	queue.EnqueuePlay(rec.Play)
	queue.Enqueue("The bins go out tonight.")
	expectNoEvent(t, rec)

	fixture.clock.Set(utc(2026, 11, 13, 7, 0))
	// The summary takes the first text slot; the audio preview keeps its own
	// slot and is played, never described as a line of speech.
	expectEvent(t, rec, "say:While it was quiet: The wash is done. The bins go out tonight.")
	expectEvent(t, rec, "play")
	expectNoEvent(t, rec)
	if rec.plays != 1 {
		t.Fatalf("audio previews played %d times, want once", rec.plays)
	}
	for _, line := range rec.phrases() {
		if line == "The bins go out tonight." {
			t.Fatal("a held line was played separately instead of inside the summary")
		}
	}
}

func TestSummaryStillWaitsForTheTurnAndConfirmation(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	rec := newRecorder()
	var blocked atomic.Bool
	queue := announce.New(rec, nil,
		announce.WithHoldSummary(),
		announce.WithBlocked(blocked.Load),
	)
	t.Cleanup(queue.Close)
	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(5 * time.Millisecond)
	controller.Start(context.Background())
	t.Cleanup(controller.Close)

	queue.Enqueue("A held reminder.")
	queue.TurnStarted()
	fixture.clock.Set(utc(2026, 11, 13, 7, 0))
	expectNoEvent(t, rec)

	blocked.Store(true)
	queue.TurnFinished()
	expectNoEvent(t, rec)

	blocked.Store(false)
	// WithBlocked is an opaque predicate, so the queue rechecks it on its
	// bounded timer instead of requiring another queue event.
	select {
	case got := <-rec.ch:
		if got != "say:A held reminder." {
			t.Fatalf("event = %q, want the summary after the gate clears", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the summary did not play after the turn and confirmation clear")
	}
}

func TestBargeInCancelsTheHeldSummary(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	speaker := &blockingSpeaker{started: make(chan string, 1), stopped: make(chan string, 1)}
	queue := announce.New(speaker, nil, announce.WithHoldSummary())
	t.Cleanup(queue.Close)
	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(5 * time.Millisecond)
	controller.Start(context.Background())
	t.Cleanup(controller.Close)

	queue.Enqueue("A held reminder.")
	fixture.clock.Set(utc(2026, 11, 13, 7, 0))
	select {
	case <-speaker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the summary never started")
	}
	queue.TurnStarted()
	select {
	case <-speaker.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Barge-in did not cancel the summary")
	}
	if queue.Len() != 0 {
		t.Fatalf("queue holds %d items after Barge-in", queue.Len())
	}
}

func TestDashboardChangeHoldsWithoutARequest(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: false, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	rec := newRecorder()
	queue := announce.New(rec, nil, announce.WithHoldSummary())
	t.Cleanup(queue.Close)
	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(5 * time.Millisecond)
	controller.Start(context.Background())
	t.Cleanup(controller.Close)

	queue.Enqueue("Played before quiet.")
	expectEvent(t, rec, "say:Played before quiet.")

	// The controller learns about a Dashboard/voice write by bounded
	// reevaluation; no request or settings observer is involved.
	if _, err := fixture.settings.Set(context.Background(), quiet.EnabledKey, true); err != nil {
		t.Fatalf("Set enabled: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !queue.Held() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !queue.Held() {
		t.Fatal("the queue was not held after the Dashboard write")
	}
	queue.Enqueue("Held now.")
	expectNoEvent(t, rec)
}

func TestControllerCloseReleasesItsHold(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	rec := newRecorder()
	queue := announce.New(rec, nil, announce.WithHoldSummary())
	t.Cleanup(queue.Close)
	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(5 * time.Millisecond)
	controller.Start(context.Background())
	if !queue.Held() {
		t.Fatal("the controller did not hold the queue")
	}
	controller.Close()
	if queue.Held() {
		t.Fatal("Close left the queue held")
	}
}

func TestConcurrentEnqueueWhileReleasing(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "22:30", End: "07:00", Volume: 35}, utc(2026, 11, 12, 23, 0), time.UTC)
	rec := newRecorder()
	queue := announce.New(rec, nil, announce.WithHoldSummary())
	t.Cleanup(queue.Close)
	controller := quiet.NewController(fixture.service, queue, nil).WithInterval(time.Millisecond)
	controller.Start(context.Background())
	t.Cleanup(controller.Close)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				queue.Enqueue("Announcement.")
			}
		}()
	}
	fixture.clock.Set(utc(2026, 11, 13, 7, 0))
	wg.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for queue.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if queue.Len() != 0 {
		t.Fatalf("queue still holds %d items", queue.Len())
	}
}
