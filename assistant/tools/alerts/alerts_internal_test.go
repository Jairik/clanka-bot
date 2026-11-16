package alerts

import (
	"strings"
	"testing"
	"time"
)

// fakeSource is one scheduling seam for the merge tests. ok=false models a
// source that could not be read.
type fakeSource struct {
	items []Item
	ok    bool
}

func (f fakeSource) NextAlerts() ([]Item, bool) { return f.items, f.ok }

func available(items ...Item) fakeSource { return fakeSource{items: items, ok: true} }

func at(base time.Time, d time.Duration, source, text string) Item {
	return Item{Source: source, At: base.Add(d), Text: text}
}

func sources(timer, alarm, reminder Source) []namedSource {
	return []namedSource{
		{name: "timer", source: timer},
		{name: "alarm", source: alarm},
		{name: "reminder", source: reminder},
	}
}

func TestCollectOrdersItemsFromAllThreeSources(t *testing.T) {
	base := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	out := collect(sources(
		available(at(base, 4*time.Minute, "timer", "the pasta timer has 4 minutes left")),
		available(at(base, 6*time.Hour, "alarm", "your 7:00 pm alarm is next")),
		available(at(base, 2*time.Hour, "reminder", "a reminder to call mum is at 10 am")),
	))
	wantOrder := []string{"timer", "reminder", "alarm"}
	if len(out.Items) != len(wantOrder) {
		t.Fatalf("items = %d, want %d: %+v", len(out.Items), len(wantOrder), out.Items)
	}
	for i, want := range wantOrder {
		if out.Items[i].Source != want {
			t.Fatalf("items[%d].Source = %q, want %q", i, out.Items[i].Source, want)
		}
	}
	if len(out.Unavailable) != 0 {
		t.Fatalf("unavailable = %v, want none", out.Unavailable)
	}
	want := "The pasta timer has 4 minutes left; a reminder to call mum is at 10 am; your 7:00 pm alarm is next."
	if got := speak(out); got != want {
		t.Fatalf("speak = %q, want %q", got, want)
	}
}

func TestEmptyStatePerSource(t *testing.T) {
	base := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	timerItem := at(base, time.Minute, "timer", "your timer has 1 minute left")
	alarmItem := at(base, 2*time.Minute, "alarm", "your 8:02 am alarm is next")
	reminderItem := at(base, 3*time.Minute, "reminder", "a reminder to call mum is in 3 minutes")

	// Nothing from any source: the honest empty sentence.
	empty := collect(sources(available(), available(), available()))
	if got := speak(empty); got != "Nothing is scheduled." {
		t.Fatalf("empty speak = %q, want %q", got, "Nothing is scheduled.")
	}

	cases := []struct {
		name                       string
		timer, alarm, reminder     fakeSource
		wantSources                []string
	}{
		{"timer empty", available(), available(alarmItem), available(reminderItem), []string{"alarm", "reminder"}},
		{"alarm empty", available(timerItem), available(), available(reminderItem), []string{"timer", "reminder"}},
		{"reminder empty", available(timerItem), available(alarmItem), available(), []string{"timer", "alarm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := collect(sources(tc.timer, tc.alarm, tc.reminder))
			got := make([]string, 0, len(out.Items))
			for _, item := range out.Items {
				got = append(got, item.Source)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantSources, ",") {
				t.Fatalf("sources = %v, want %v", got, tc.wantSources)
			}
			if len(out.Unavailable) != 0 {
				t.Fatalf("unavailable = %v, want none", out.Unavailable)
			}
			if text := speak(out); strings.Contains(text, "Nothing is scheduled") || !strings.Contains(text, ";") {
				t.Fatalf("speak = %q, want both remaining items in one sentence", text)
			}
		})
	}
}

func TestUnavailableSourceDoesNotHideTheOthers(t *testing.T) {
	base := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	alarmItem := at(base, 2*time.Minute, "alarm", "your 8:02 am alarm is next")
	reminderItem := at(base, 3*time.Minute, "reminder", "a reminder to call mum is in 3 minutes")

	// A failed timer source is named, and the other two items still appear.
	out := collect(sources(fakeSource{ok: false}, available(alarmItem), available(reminderItem)))
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2: %+v", len(out.Items), out.Items)
	}
	if len(out.Unavailable) != 1 || out.Unavailable[0] != "timer" {
		t.Fatalf("unavailable = %v, want [timer]", out.Unavailable)
	}
	text := speak(out)
	for _, want := range []string{"8:02 am alarm is next", "a reminder to call mum is in 3 minutes", "I couldn't check timer"} {
		if !strings.Contains(text, want) {
			t.Fatalf("speak = %q, want it to contain %q", text, want)
		}
	}

	// A nil (unwired) source is treated the same way, and an all-failed answer
	// does not claim nothing is scheduled.
	nilTimer := collect(sources(nil, available(alarmItem), available(reminderItem)))
	if len(nilTimer.Unavailable) != 1 || nilTimer.Unavailable[0] != "timer" {
		t.Fatalf("nil source unavailable = %v, want [timer]", nilTimer.Unavailable)
	}
	allFailed := collect(sources(fakeSource{ok: false}, fakeSource{ok: false}, fakeSource{ok: false}))
	if got := speak(allFailed); !strings.Contains(got, "couldn't check") || strings.Contains(got, "Nothing is scheduled") {
		t.Fatalf("all-failed speak = %q, want an honest failure", got)
	}
}

func TestCollectBoundsTheAnswer(t *testing.T) {
	base := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	many := make([]Item, 0, 5)
	for i := 0; i < 5; i++ {
		many = append(many, at(base, time.Duration(i)*time.Minute, "timer", "a timer"))
	}
	out := collect(sources(available(many...), available(), available()))
	if len(out.Items) != maxItems {
		t.Fatalf("items = %d, want the cap %d", len(out.Items), maxItems)
	}
}
