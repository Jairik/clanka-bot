package timer_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// alertRecorder collects the text the timer alert speaks.
type alertRecorder struct {
	texts chan string
}

func newRecorder() *alertRecorder {
	return &alertRecorder{texts: make(chan string, 4)}
}

func (a *alertRecorder) SayText(_ context.Context, text string) error {
	a.texts <- text
	return nil
}

func newManager(rec *alertRecorder) *timer.Manager {
	var say func(context.Context, string) error
	if rec != nil {
		say = rec.SayText
	}
	return timer.NewManager(nil, say, nil)
}

func TestSetAndListReportsRemainingTime(t *testing.T) {
	m := newManager(nil)
	fixed := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	m = timer.NewManager(func() time.Time { return fixed }, nil, nil)

	tm := m.Set(5 * time.Minute)
	if tm.ID == "" {
		t.Fatal("Set returned an empty id")
	}
	if got, want := tm.FiresAt, fixed.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("FiresAt = %s, want %s", got, want)
	}

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("List returned %d timers, want 1", len(list))
	}
	if list[0].RemainingSeconds != 300 {
		t.Fatalf("RemainingSeconds = %d, want 300", list[0].RemainingSeconds)
	}
	m.Cancel("")
}

func TestCancelRemovesPendingTimer(t *testing.T) {
	m := newManager(nil)
	m.Set(time.Hour)
	if cancelled := m.Cancel(""); cancelled != 1 {
		t.Fatalf("Cancel = %d, want 1", cancelled)
	}
	if list := m.List(); len(list) != 0 {
		t.Fatalf("List after cancel = %v, want empty", list)
	}
	if cancelled := m.Cancel(""); cancelled != 0 {
		t.Fatalf("second Cancel = %d, want 0", cancelled)
	}
}

func TestTimerFiresAnAudibleAlert(t *testing.T) {
	rec := newRecorder()
	m := newManager(rec)
	m.Set(20 * time.Millisecond)

	select {
	case text := <-rec.texts:
		if text != timer.AlertText {
			t.Fatalf("alert = %q, want %q", text, timer.AlertText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire an alert")
	}
	// The fired timer is gone from the active list.
	deadline := time.Now().Add(time.Second)
	for len(m.List()) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if list := m.List(); len(list) != 0 {
		t.Fatalf("List after firing = %v, want empty", list)
	}
}

func TestCancelledTimerDoesNotFire(t *testing.T) {
	rec := newRecorder()
	m := newManager(rec)
	m.Set(20 * time.Millisecond)
	m.Cancel("")

	select {
	case text := <-rec.texts:
		t.Fatalf("cancelled timer alerted: %q", text)
	case <-time.After(100 * time.Millisecond):
	}
}

func newRegistry(t *testing.T, m *timer.Manager) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := timer.Register(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

func TestSetToolExtractsSpokenDuration(t *testing.T) {
	m := newManager(nil)
	reg := newRegistry(t, m)

	args, ok := reg.Extract(timer.SetName, "set a timer for five minutes")
	if !ok {
		t.Fatal("Extract failed")
	}
	result, err := reg.Execute(context.Background(), timer.SetName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, _ := reg.Speak(timer.SetName, result)
	if want := "Timer set for 5 minutes."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
	if len(m.List()) != 1 {
		t.Fatalf("timers = %v, want one", m.List())
	}
	m.Cancel("")
}

func TestSetToolExtractFailsWithoutADuration(t *testing.T) {
	reg := newRegistry(t, newManager(nil))
	if _, ok := reg.Extract(timer.SetName, "set a timer"); ok {
		t.Fatal("Extract succeeded, want failure so the request falls through")
	}
}

func TestListToolSpeaksWhenEmpty(t *testing.T) {
	m := newManager(nil)
	reg := newRegistry(t, m)
	result, err := reg.Execute(context.Background(), timer.ListName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, _ := reg.Speak(timer.ListName, result)
	if want := "No timers are running."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestCancelToolSpeaks(t *testing.T) {
	m := newManager(nil)
	reg := newRegistry(t, m)
	m.Set(time.Hour)
	result, err := reg.Execute(context.Background(), timer.CancelName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, _ := reg.Speak(timer.CancelName, result)
	if want := "Timer cancelled."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
	if len(m.List()) != 0 {
		t.Fatalf("timers = %v, want empty", m.List())
	}
}

func TestTimerToolsAreFastPathEligible(t *testing.T) {
	reg := newRegistry(t, newManager(nil))
	for _, name := range []string{timer.SetName, timer.CancelName, timer.RemainingName, timer.ListName} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if info.Permission != tool.PermissionAct && info.Permission != tool.PermissionRead {
			t.Errorf("%s Permission = %q, want read or act", name, info.Permission)
		}
		if !info.HasSpeak || len(info.Examples) == 0 {
			t.Errorf("%s metadata = %+v", name, info)
		}
	}
}

// TestListIsLocalOnly keeps the privacy declaration explicit: a timer label can
// name something private, so the list never reaches the Agent path.
func TestListIsLocalOnly(t *testing.T) {
	reg := newRegistry(t, newManager(nil))
	info, ok := reg.Info(timer.ListName)
	if !ok {
		t.Fatal("timer.list not registered")
	}
	if !info.LocalOnly {
		t.Fatal("timer.list is not LocalOnlyYes")
	}
	for _, name := range []string{timer.SetName, timer.CancelName, timer.RemainingName} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if info.LocalOnly {
			t.Errorf("%s is LocalOnlyYes, want LocalOnlyNo", name)
		}
	}
}
