package announce_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
)

// TestSpokenLineIsRecordedOnce proves an announcement enters the ring as queued
// and updates to spoken in place without creating a duplicate record.
func TestSpokenLineIsRecordedOnce(t *testing.T) {
	speaker := newRecordingSpeaker()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	queue := announce.New(speaker, testLogger(), announce.WithNow(func() time.Time { return now }))
	t.Cleanup(queue.Close)

	if !queue.Enqueue("Dinner is ready.") {
		t.Fatal("Enqueue failed")
	}

	expectPhrases(t, speaker, "Dinner is ready.")

	// Wait briefly for the worker to update the line state after SayText finishes.
	var lines []announce.Line
	for range 20 {
		lines = queue.Recent()
		if len(lines) == 1 && lines[0].State == announce.StateSpoken {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(lines) != 1 {
		t.Fatalf("Recent() returned %d lines, want 1", len(lines))
	}
	got := lines[0]
	if got.Text != "Dinner is ready." {
		t.Errorf("Text = %q, want Dinner is ready.", got.Text)
	}
	if got.Kind != announce.KindGeneric {
		t.Errorf("Kind = %q, want generic", got.Kind)
	}
	if got.State != announce.StateSpoken {
		t.Errorf("State = %q, want spoken", got.State)
	}
	if !got.EnqueuedAt.Equal(now) {
		t.Errorf("EnqueuedAt = %v, want %v", got.EnqueuedAt, now)
	}
}

// TestHeldLineMarkedHeldAndReplacedByOneSummary proves lines enqueued under a
// hold are marked held, and on release with summary are replaced in the ring by
// a single summary line.
func TestHeldLineMarkedHeldAndReplacedByOneSummary(t *testing.T) {
	speaker := newRecordingSpeaker()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	queue := announce.New(speaker, testLogger(),
		announce.WithHoldSummary(),
		announce.WithNow(func() time.Time { return now }),
	)
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.Enqueue("First held announcement.")
	queue.Enqueue("Second held announcement.")

	heldLines := queue.Recent()
	if len(heldLines) != 2 {
		t.Fatalf("Recent() during hold returned %d lines, want 2", len(heldLines))
	}
	for i, l := range heldLines {
		if l.State != announce.StateHeld {
			t.Errorf("line %d state = %q, want held", i, l.State)
		}
	}

	queue.Release()

	// Wait for playback of the summary line.
	expectPhrases(t, speaker, "While it was quiet: First held announcement. Second held announcement.")

	var releasedLines []announce.Line
	for range 20 {
		releasedLines = queue.Recent()
		if len(releasedLines) == 1 && releasedLines[0].State == announce.StateSpoken {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(releasedLines) != 1 {
		t.Fatalf("Recent() after release returned %d lines, want 1 summary line", len(releasedLines))
	}
	summaryLine := releasedLines[0]
	if summaryLine.Kind != announce.KindSummary {
		t.Errorf("summary line kind = %q, want summary", summaryLine.Kind)
	}
	if summaryLine.Text != "While it was quiet: First held announcement. Second held announcement." {
		t.Errorf("summary line text = %q, want collapsed text", summaryLine.Text)
	}
	if summaryLine.State != announce.StateSpoken {
		t.Errorf("summary line state = %q, want spoken", summaryLine.State)
	}
}

// TestTurnStartedMarksPlayingAnnouncementInterrupted proves Barge-in cancels
// the active playback and marks the ring entry interrupted.
func TestTurnStartedMarksPlayingAnnouncementInterrupted(t *testing.T) {
	speaker := &blockingSpeaker{started: make(chan string, 1), stopped: make(chan string, 1)}
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Enqueue("Long playing announcement.")

	select {
	case <-speaker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback did not start")
	}

	queue.TurnStarted()

	var lines []announce.Line
	for range 20 {
		lines = queue.Recent()
		if len(lines) == 1 && lines[0].State == announce.StateInterrupted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(lines) != 1 {
		t.Fatalf("Recent() returned %d lines, want 1", len(lines))
	}
	if lines[0].State != announce.StateInterrupted {
		t.Errorf("State = %q, want interrupted", lines[0].State)
	}
}

// TestRingCapsAtTwentyAndEvictsOldestFirst proves the ring holds at most 20
// entries and evicts oldest items as new ones arrive.
func TestRingCapsAtTwentyAndEvictsOldestFirst(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	// Keep the queue held so items accumulate in the ring without finishing immediately.
	queue.Hold()

	for i := 1; i <= 25; i++ {
		queue.Enqueue(time.Duration(i).String())
	}

	lines := queue.Recent()
	if len(lines) != announce.RingSize {
		t.Fatalf("Recent() returned %d lines, want RingSize (%d)", len(lines), announce.RingSize)
	}

	// First entry should be the 6th item (evicting items 1 to 5).
	if lines[0].Text != (6 * time.Nanosecond).String() {
		t.Errorf("oldest line text = %q, want %q", lines[0].Text, (6 * time.Nanosecond).String())
	}
	// Last entry should be the 25th item.
	if lines[len(lines)-1].Text != (25 * time.Nanosecond).String() {
		t.Errorf("newest line text = %q, want %q", lines[len(lines)-1].Text, (25 * time.Nanosecond).String())
	}
}

// TestRecentReturnsCopies proves mutations to the returned slice do not affect the internal ring.
func TestRecentReturnsCopies(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	queue.Hold()
	queue.Enqueue("Original announcement.")

	first := queue.Recent()
	if len(first) != 1 {
		t.Fatalf("Recent() returned %d lines, want 1", len(first))
	}
	first[0].Text = "Mutated text."
	first[0].State = announce.StateInterrupted

	second := queue.Recent()
	if second[0].Text != "Original announcement." {
		t.Errorf("Text was mutated in ring: %q", second[0].Text)
	}
	if second[0].State != announce.StateHeld {
		t.Errorf("State was mutated in ring: %q", second[0].State)
	}
}

// TestSoundPreviewIsNotRecorded proves EnqueuePlay does not add entries to the ring
// and the Line type contains no closure or raw audio.
func TestSoundPreviewIsNotRecorded(t *testing.T) {
	speaker := newRecordingSpeaker()
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)

	// The preview runs on the queue worker. A channel is the handoff, so the
	// test does not read a bool the worker is still writing.
	played := make(chan struct{}, 1)
	queue.EnqueuePlay(func(context.Context) error {
		played <- struct{}{}
		return nil
	})

	select {
	case <-played:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("audio item did not play")
	}

	lines := queue.Recent()
	if len(lines) != 0 {
		t.Fatalf("Recent() has %d lines after sound preview, want 0", len(lines))
	}

	// Verify Line struct fields contain no func or []byte.
	lineType := reflect.TypeOf(announce.Line{})
	for i := 0; i < lineType.NumField(); i++ {
		field := lineType.Field(i)
		kind := field.Type.Kind()
		if kind == reflect.Func || (kind == reflect.Slice && field.Type.Elem().Kind() == reflect.Uint8) {
			t.Errorf("Line has prohibited field %q of type %v", field.Name, field.Type)
		}
	}
}
