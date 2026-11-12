package timer_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// TestModuleDeclaresTheIntentsItsAlertHandles is the module-level contract for
// downstream Focus work: the timer module takes "stop", "cancel" and "snooze"
// while its alert is sounding, and nothing else.
func TestModuleDeclaresTheIntentsItsAlertHandles(t *testing.T) {
	declarer, ok := tool.Module(timer.NewModule()).(tool.IntentDeclarer)
	if !ok {
		t.Fatal("timer module does not declare its Focus intents")
	}
	got := declarer.Intents()
	want := []focus.Intent{focus.Stop, focus.Cancel, focus.Snooze}
	if len(got) != len(want) {
		t.Fatalf("Intents() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Intents()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// blockingAlert holds the alert speech until its context is cancelled and
// reports the cancellation, so a stop that only muted the speaker would fail.
type blockingAlert struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func newBlockingAlert() *blockingAlert {
	return &blockingAlert{started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (a *blockingAlert) SayText(ctx context.Context, _ string) error {
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
	close(a.cancelled)
	return ctx.Err()
}

// waitForAlert fails the test if the alert never starts.
func waitForAlert(t *testing.T, a *blockingAlert) {
	t.Helper()
	select {
	case <-a.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the timer alert never started")
	}
}

// TestRingingAlertHoldsFocusAndStopsOnStop covers the acceptance rule: the
// timer module handles "stop" for its alert while it is ringing.
func TestRingingAlertHoldsFocusAndStopsOnStop(t *testing.T) {
	var registry focus.Registry
	alert := newBlockingAlert()
	manager := timer.NewManager(nil, alert.SayText, nil).WithFocus(&registry)
	manager.Set(10 * time.Millisecond)
	waitForAlert(t, alert)

	text, handled := registry.HandleIntent(context.Background(), focus.Stop)
	if !handled {
		t.Fatal("the ringing alert did not handle stop")
	}
	if text != "Timer stopped." {
		t.Fatalf("text = %q, want the timer stop line", text)
	}
	select {
	case <-alert.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the alert speech kept running after stop")
	}

	// The alert goroutine releases its claim once the speech returns; a later
	// stop must fall through instead of reviving a finished alert.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the finished alert kept Focus")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRingingAlertHandlesCancel keeps "cancel" as the second declared intent.
func TestRingingAlertHandlesCancel(t *testing.T) {
	var registry focus.Registry
	alert := newBlockingAlert()
	manager := timer.NewManager(nil, alert.SayText, nil).WithFocus(&registry)
	manager.Set(10 * time.Millisecond)
	waitForAlert(t, alert)

	text, handled := registry.HandleIntent(context.Background(), focus.Cancel)
	if !handled || text != "Timer cancelled." {
		t.Fatalf("HandleIntent(cancel) = %q, %v; want the timer cancel line", text, handled)
	}
	select {
	case <-alert.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the alert speech kept running after cancel")
	}
}

// TestRingingAlertLeavesOtherIntentsToTheNextHolder keeps the declaration
// honest: "pause" is not something a Timer alert can do, so it reaches the
// next holder rather than being swallowed.
func TestRingingAlertLeavesOtherIntentsToTheNextHolder(t *testing.T) {
	var registry focus.Registry
	alert := newBlockingAlert()
	manager := timer.NewManager(nil, alert.SayText, nil).WithFocus(&registry)
	manager.Set(10 * time.Millisecond)
	waitForAlert(t, alert)
	defer registry.HandleIntent(context.Background(), focus.Stop)

	music := &musicHolder{}
	claim := registry.Claim(focus.Music, music)
	defer claim.Release()
	if text, handled := registry.HandleIntent(context.Background(), focus.Pause); !handled || text != "Music paused." {
		t.Fatalf("pause = %q, %v; want the next holder to take it", text, handled)
	}
	if !music.paused {
		t.Fatal("pause did not fall through to music")
	}
}

// musicHolder is the lower-priority holder in the fall-through test.
type musicHolder struct {
	paused bool
}

func (m *musicHolder) Intents() []focus.Intent { return []focus.Intent{focus.Pause} }

func (m *musicHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	if intent != focus.Pause {
		return "", false
	}
	m.paused = true
	return "Music paused.", true
}

// TestPipelineStopReachesARingingTimer ties the phrase table, Focus and the
// timer alert together: the shared word reaches the ringing alert and the reply
// turn speaks its stop line.
func TestPipelineStopReachesARingingTimer(t *testing.T) {
	var registry focus.Registry
	alert := newBlockingAlert()
	reg := tool.NewRegistry(time.Second)
	manager := timer.NewManager(nil, alert.SayText, nil).WithFocus(&registry)
	if err := timer.Register(reg, manager); err != nil {
		t.Fatalf("register timer: %v", err)
	}
	p, err := pipeline.New(reg, map[string]string{"stop": "stop"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		pipeline.WithFocus(&registry),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	manager.Set(10 * time.Millisecond)
	waitForAlert(t, alert)

	reply := p.Handle(context.Background(), tool.Request{ID: "stop", Device: tool.DevicePi}, "stop")
	if reply.Path != tool.PathFocusIntent {
		t.Fatalf("path = %q, want %q", reply.Path, tool.PathFocusIntent)
	}
	if reply.Text != timer.StopText {
		t.Fatalf("text = %q, want %q", reply.Text, timer.StopText)
	}
	select {
	case <-alert.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the alert speech kept running after the stop phrase")
	}
}

// TestAlertWithoutFocusStillSpeaks keeps the seam optional: a headless or
// pre-#52 deployment still hears its timer.
func TestAlertWithoutFocusStillSpeaks(t *testing.T) {
	rec := newRecorder()
	manager := timer.NewManager(nil, rec.SayText, nil)
	manager.Set(10 * time.Millisecond)

	select {
	case text := <-rec.texts:
		if text != timer.AlertText {
			t.Fatalf("alert = %q, want %q", text, timer.AlertText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the alert never spoke")
	}
}
