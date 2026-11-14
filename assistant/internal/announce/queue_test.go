package announce_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
)

// recordingSpeaker records what it was asked to say and notices overlapping
// playback, which the single speaker must never allow.
type recordingSpeaker struct {
	mu      sync.Mutex
	said    []string
	active  int
	overlap bool
	saidCh  chan string
}

func newRecordingSpeaker() *recordingSpeaker {
	return &recordingSpeaker{saidCh: make(chan string, 16)}
}

func (s *recordingSpeaker) SayText(_ context.Context, text string) error {
	s.mu.Lock()
	s.active++
	if s.active > 1 {
		s.overlap = true
	}
	s.mu.Unlock()

	s.mu.Lock()
	s.said = append(s.said, text)
	s.mu.Unlock()
	s.saidCh <- text

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return nil
}

func (s *recordingSpeaker) phrases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.said...)
}

func (s *recordingSpeaker) overlapped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overlap
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func expectPhrases(t *testing.T, speaker *recordingSpeaker, want ...string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for range want {
		select {
		case phrase := <-speaker.saidCh:
			got = append(got, phrase)
		case <-time.After(2 * time.Second):
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}
	if !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

func expectSilence(t *testing.T, speaker *recordingSpeaker) {
	t.Helper()
	select {
	case phrase := <-speaker.saidCh:
		t.Fatalf("spoke %q, want silence", phrase)
	case <-time.After(100 * time.Millisecond):
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

func TestQueuedAnnouncementsPlayInOrderOneAtATime(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Enqueue("The bins go out tonight.")
	queue.Enqueue("The wash is done.")
	queue.Enqueue("Hermes finished your job.")

	expectPhrases(t, speaker, "The bins go out tonight.", "The wash is done.", "Hermes finished your job.")
	if speaker.overlapped() {
		t.Fatal("announcements overlapped on the one speaker")
	}
	if queue.Len() != 0 {
		t.Fatalf("queue still holds %d announcements after playing", queue.Len())
	}
}

// blockingSpeaker models TTS playback that lasts until its context is
// cancelled, so a test can hold one announcement in flight.
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

func TestTurnStopsAnAnnouncementInFlightAndHoldsTheRest(t *testing.T) {
	speaker := &blockingSpeaker{started: make(chan string, 1), stopped: make(chan string, 1)}
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Enqueue("A long reminder.")
	queue.Enqueue("The next one.")
	select {
	case got := <-speaker.started:
		if got != "A long reminder." {
			t.Fatalf("started %q, want the first announcement", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first announcement never started")
	}

	queue.TurnStarted()
	select {
	case <-speaker.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the reply did not stop the announcement in flight")
	}
	select {
	case got := <-speaker.started:
		t.Fatalf("spoke %q during the reply", got)
	case <-time.After(100 * time.Millisecond):
	}

	queue.TurnFinished()
	select {
	case got := <-speaker.started:
		if got != "The next one." {
			t.Fatalf("started %q, want the queued second announcement", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the next announcement did not play after the reply")
	}
}

func TestCloseCancelsPlaybackAndDropsTheQueue(t *testing.T) {
	speaker := &blockingSpeaker{started: make(chan string, 1), stopped: make(chan string, 1)}
	queue := announce.New(speaker, testLogger())

	queue.Enqueue("A long reminder.")
	queue.Enqueue("Never played.")
	select {
	case <-speaker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the announcement never started")
	}

	closed := make(chan struct{})
	go func() {
		queue.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for playback instead of cancelling it")
	}
	if queue.Enqueue("After close.") {
		t.Fatal("Enqueue accepted an announcement after Close")
	}
	if queue.Len() != 0 {
		t.Fatalf("queue holds %d announcements after Close", queue.Len())
	}
}

func TestNilSpeakerKeepsTheQueueInert(t *testing.T) {
	queue := announce.New(nil, testLogger())
	defer queue.Close()

	if queue.Enqueue("Nobody hears this.") {
		t.Fatal("Enqueue accepted an announcement with no speaker")
	}
	if queue.Len() != 0 {
		t.Fatalf("queue holds %d announcements without a speaker", queue.Len())
	}
}

func TestHoldKeepsAnnouncementsQueuedUntilRelease(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.Enqueue("A held reminder.")
	expectSilence(t, speaker)
	if !queue.Held() {
		t.Fatal("Held() = false after Hold()")
	}
	if queue.Len() != 1 {
		t.Fatalf("queue holds %d announcements, want the held one", queue.Len())
	}

	queue.Release()
	expectPhrases(t, speaker, "A held reminder.")
	if queue.Held() {
		t.Fatal("Held() = true after Release()")
	}
}

// TestUrgentAlertBypassesTheHold proves the Quiet-hours Reminder seam: an
// urgent alert sounds while ordinary announcements stay held.
func TestUrgentAlertBypassesTheHold(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.Enqueue("A held announcement.")
	queue.EnqueueAlert(func(context.Context) error {
		return speaker.SayText(context.Background(), "Your reminder: check the oven.")
	})

	expectPhrases(t, speaker, "Your reminder: check the oven.")
	expectSilence(t, speaker)
	if queue.Len() != 1 {
		t.Fatalf("queue holds %d items, want the ordinary one held", queue.Len())
	}
	queue.Release()
	expectPhrases(t, speaker, "A held announcement.")
}

// TestUrgentAlertWaitsForTheTurn keeps the priority order: Quiet hours do not
// let a Reminder interrupt a reply.
func TestUrgentAlertWaitsForTheTurn(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.TurnStarted()
	queue.EnqueueAlert(func(context.Context) error {
		return speaker.SayText(context.Background(), "Your reminder: check the oven.")
	})
	expectSilence(t, speaker)

	queue.TurnFinished()
	expectPhrases(t, speaker, "Your reminder: check the oven.")
}

// TestUrgentAlertWaitsForABlockedConfirmation keeps a pending Confirmation
// ahead of an urgent alert without lifting the Quiet hold.
func TestUrgentAlertWaitsForABlockedConfirmation(t *testing.T) {
	speaker := newRecordingSpeaker()
	var blocked atomic.Bool
	blocked.Store(true)
	queue := announce.New(speaker, testLogger(), announce.WithBlocked(func() bool { return blocked.Load() }))
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.EnqueueAlert(func(context.Context) error {
		return speaker.SayText(context.Background(), "Your reminder: check the oven.")
	})
	expectSilence(t, speaker)

	blocked.Store(false)
	// The blocked predicate is opaque, so the worker rechecks it.
	expectPhrasesWithTimeout(t, speaker, 2*time.Second, "Your reminder: check the oven.")
}

// TestUrgentAlertIsNotCollapsedIntoTheHeldSummary keeps a Reminder out of the
// morning digest: only ordinary text items are summarised.
func TestUrgentAlertIsNotCollapsedIntoTheHeldSummary(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger(), announce.WithHoldSummary())
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.Enqueue("The bins go out tonight.")
	queue.EnqueueAlert(func(context.Context) error {
		return speaker.SayText(context.Background(), "Your reminder: check the oven.")
	})
	queue.Enqueue("The wash is done.")

	expectPhrases(t, speaker, "Your reminder: check the oven.")
	queue.Release()
	expectPhrases(t, speaker, "While it was quiet: The bins go out tonight. The wash is done.")
}

// expectPhrasesWithTimeout is expectPhrases with a caller-chosen bound for a
// gate that only rechecks on a timer.
func expectPhrasesWithTimeout(t *testing.T, speaker *recordingSpeaker, timeout time.Duration, want ...string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for range want {
		select {
		case phrase := <-speaker.saidCh:
			got = append(got, phrase)
		case <-time.After(timeout):
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}
	if !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}
