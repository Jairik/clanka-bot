package alarm_test

import (
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// TestListSpeakNamesEveryAlarm covers issue #245: alarm.list must read the
// scheduled Alarms aloud instead of only the count and the next one. The
// ringing wording keeps priority, so these cases use only pending Alarms.
func TestListSpeakNamesEveryAlarm(t *testing.T) {
	// 0 alarms.
	zero := alarm.NewList(nil).Speak(alarm.ListOut{Alarms: nil})
	if zero != "No alarms are set." {
		t.Fatalf("0 alarms Speak = %q, want %q", zero, "No alarms are set.")
	}

	// 1 alarm: unchanged describeAlarm wording.
	one := alarm.NewList(nil).Speak(alarm.ListOut{Alarms: []alarm.Alarm{
		{Label: "work", Hour: 7, Schedule: alarm.Weekdays},
	}})
	if one != "Work alarm is set for 7:00 am on weekdays." {
		t.Fatalf("1 alarm Speak = %q, want the single describeAlarm wording", one)
	}

	// 2 alarms: both named, with labels and repeats.
	two := alarm.NewList(nil).Speak(alarm.ListOut{Alarms: []alarm.Alarm{
		{Label: "work", Hour: 6, Minute: 30, Schedule: alarm.Weekdays},
		{Label: "gym", Hour: 7, Schedule: alarm.Daily},
	}})
	wantTwo := "You have 2 alarms: work at 6:30 am on weekdays and gym at 7:00 am every day."
	if two != wantTwo {
		t.Fatalf("2 alarms Speak = %q, want %q", two, wantTwo)
	}

	// 6 alarms: the count line, the next two, and "and N more" — the reply
	// must stay bounded and must not name every one.
	six := []alarm.Alarm{
		{Label: "work", Hour: 6, Minute: 30, Schedule: alarm.Weekdays},
		{Label: "gym", Hour: 7, Schedule: alarm.Daily},
		{Label: "tea", Hour: 8, Schedule: alarm.Weekends},
		{Label: "meds", Hour: 9, Schedule: alarm.Daily},
		{Hour: 12, Schedule: alarm.Daily},
		{Label: "last", Hour: 18, Schedule: alarm.Daily},
	}
	gotSix := alarm.NewList(nil).Speak(alarm.ListOut{Alarms: six})
	if !strings.HasPrefix(gotSix, "You have 6 alarms. The next two are ") {
		t.Fatalf("6 alarms Speak = %q, want the count line and next two", gotSix)
	}
	if !strings.Contains(gotSix, "and 4 more.") {
		t.Fatalf("6 alarms Speak = %q, want %q", gotSix, "and 4 more.")
	}
	if strings.Contains(gotSix, "last") {
		t.Fatalf("6 alarms Speak = %q, must not name alarms beyond the next two", gotSix)
	}
}

// TestUpdateSpeakAudioChange covers issue #247: an audio-only (or audio-clear)
// update must not be spoken as "Nothing changed". The table walks every value
// Changed can carry, including audio set and cleared.
func TestUpdateSpeakAudioChange(t *testing.T) {
	base := func(changed ...string) alarm.UpdateOut {
		return alarm.UpdateOut{
			Alarm: alarm.Alarm{ID: "1", Label: "", Hour: 7, Minute: 0},
			Matched: 1,
			Changed: changed,
		}
	}
	cases := []struct {
		name    string
		out     alarm.UpdateOut
		want    string
	}{
		{"label", base("label"), "Renamed your 7:00 am alarm"},
		{"time", base("time"), "moved to"},
		{"sound set", func() alarm.UpdateOut { o := base("sound"); o.Alarm.Sound = "birds"; return o }(), "the birds sound"},
		{"sound cleared", func() alarm.UpdateOut { o := base("sound"); o.Alarm.Sound = ""; return o }(), "the default sound"},
		{"volume set", func() alarm.UpdateOut { v := 40; o := base("volume"); o.Alarm.Volume = &v; return o }(), "volume 40"},
		{"volume cleared", func() alarm.UpdateOut { o := base("volume"); o.Alarm.Volume = nil; return o }(), "the default volume"},
		{"ramp set", func() alarm.UpdateOut { r := 10; o := base("ramp"); o.Alarm.RampSeconds = &r; return o }(), "a 10 second ramp"},
		{"ramp cleared", func() alarm.UpdateOut { o := base("ramp"); o.Alarm.RampSeconds = nil; return o }(), "the default ramp"},
		{"audio set", func() alarm.UpdateOut { o := base("audio"); o.Alarm.Audio = "99 Luftballons"; return o }(), "Changed your 7:00 am alarm to the completion audio."},
		{"audio cleared", func() alarm.UpdateOut { o := base("audio"); o.Alarm.Audio = ""; return o }(), "Changed your 7:00 am alarm to no completion audio."},
		{"audio and sound", func() alarm.UpdateOut { o := base("audio", "sound"); o.Alarm.Audio = "song"; o.Alarm.Sound = "birds"; return o }(), "completion audio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alarm.NewUpdate(nil).Speak(tc.out)
			if strings.Contains(got, "Nothing changed") {
				t.Fatalf("Changed=%v Speak = %q, must not say 'Nothing changed'", tc.out.Changed, got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Changed=%v Speak = %q, want it to contain %q", tc.out.Changed, got, tc.want)
			}
		})
	}
}
