package dashboard

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	listcore "github.com/Jairik/clanka-bot/assistant/tools/list"
	notecore "github.com/Jairik/clanka-bot/assistant/tools/note"
)

// fakeContextManager blocks reads on ctx.Done() to prove that callers pass their
// request context down to storage instead of substituting context.Background().
type fakeContextManager struct {
	started chan string
}

// newFakeContextManager constructs a manager with an event channel for read starts.
func newFakeContextManager() *fakeContextManager {
	return &fakeContextManager{started: make(chan string, 4)}
}

// List simulates a note query that blocks until the caller's context completes.
func (f *fakeContextManager) List(ctx context.Context) ([]notecore.Note, error) {
	f.started <- "notes"
	<-ctx.Done()
	return nil, ctx.Err()
}

// All simulates a list query that blocks until the caller's context completes.
func (f *fakeContextManager) All(ctx context.Context) ([]listcore.List, error) {
	f.started <- "lists"
	<-ctx.Done()
	return nil, ctx.Err()
}

// QueryPage simulates an activity query that blocks until the caller's context completes.
func (f *fakeContextManager) QueryPage(ctx context.Context, q activity.Query) (activity.Page, error) {
	f.started <- "activity"
	<-ctx.Done()
	return activity.Page{}, ctx.Err()
}

// TestRequestContextCancellationPromptlyAbortsNotesAndActivityReads verifies that
// cancelling a request context unblocks notes and activity view builders when backed
// by a fake manager that waits on ctx.
func TestRequestContextCancellationPromptlyAbortsNotesAndActivityReads(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Verify notes view builder aborts promptly on cancelled context.
	fakeNotes := newFakeContextManager()
	notesHandler := &Handler{
		logger: logger,
		now:    time.Now,
		notesDepsOverride: func() notesDeps {
			return notesDeps{notes: fakeNotes, lists: fakeNotes}
		},
	}

	notesCtx, cancelNotes := context.WithCancel(context.Background())
	go func() {
		// Wait for one of the note or list reads to start before cancelling.
		select {
		case <-fakeNotes.started:
			cancelNotes()
		case <-time.After(2 * time.Second):
			t.Errorf("notes read never started")
			cancelNotes()
		}
	}()

	startNotes := time.Now()
	notesView := notesHandler.buildNotesView(notesCtx, "", "")
	elapsedNotes := time.Since(startNotes)

	if elapsedNotes > 500*time.Millisecond {
		t.Fatalf("buildNotesView took %v, want prompt return after context cancellation", elapsedNotes)
	}
	// Existing fallback leaves the page rendered despite read errors.
	if !notesView.HasStore {
		t.Errorf("notesView.HasStore = false, want true")
	}

	// Verify activity view builder aborts promptly on cancelled context.
	fakeActivity := newFakeContextManager()
	activityHandler := &Handler{
		logger:                   logger,
		now:                      time.Now,
		activity:                 activity.New(10),
		activityHistoryOverride: fakeActivity,
	}

	actCtx, cancelAct := context.WithCancel(context.Background())
	go func() {
		// Wait for activity history query to start before cancelling.
		select {
		case <-fakeActivity.started:
			cancelAct()
		case <-time.After(2 * time.Second):
			t.Errorf("activity query never started")
			cancelAct()
		}
	}()

	startAct := time.Now()
	actView := activityHandler.buildActivityView(actCtx, "", "", "", "")
	elapsedAct := time.Since(startAct)

	if elapsedAct > 500*time.Millisecond {
		t.Fatalf("buildActivityView took %v, want prompt return after context cancellation", elapsedAct)
	}
	// Fallback to ring leaves activity view populated.
	if !actView.HasActivity {
		t.Errorf("actView.HasActivity = false, want true")
	}
}
