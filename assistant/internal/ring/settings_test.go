package ring_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openSettings builds the real live settings service over a temporary
// database, exactly as main does for the Dashboard and voice writers.
func openSettings(t *testing.T) *settings.Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := settings.New(context.Background(), st, ring.Definitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc
}

// TestDefaultsPerKind pins the spec defaults: 5 min, absolute volume, Timer
// ramp 0 s, Alarm ramp 30 s, 1 s gap, label at the start.
func TestDefaultsPerKind(t *testing.T) {
	timer := ring.Defaults(ring.Timer)
	if timer.MaxRing != 5*time.Minute || timer.Volume != 80 || timer.Ramp != 0 || timer.Gap != time.Second || timer.SpeakLabel != ring.SpeakStart || timer.Snooze != 0 {
		t.Fatalf("timer defaults = %+v", timer)
	}
	alarm := ring.Defaults(ring.Alarm)
	if alarm.MaxRing != 5*time.Minute || alarm.Volume != 80 || alarm.Ramp != 30*time.Second || alarm.Gap != time.Second || alarm.SpeakLabel != ring.SpeakStart || alarm.Snooze != 9*time.Minute {
		t.Fatalf("alarm defaults = %+v", alarm)
	}
}

// TestDefinitionsAreLiveAndPerKind proves a Dashboard/voice write changes only
// one kind and applies on the next read, with no restart.
func TestDefinitionsAreLiveAndPerKind(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t)
	before := ring.ReadSettings(svc, ring.Timer)
	if before != ring.Defaults(ring.Timer) {
		t.Fatalf("timer before = %+v", before)
	}
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "volume"), []byte(`50`)); err != nil {
		t.Fatalf("set timer volume: %v", err)
	}
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "gap"), []byte(`"2s"`)); err != nil {
		t.Fatalf("set timer gap: %v", err)
	}
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Timer, "speak_label"), []byte(`"never"`)); err != nil {
		t.Fatalf("set timer speak_label: %v", err)
	}
	after := ring.ReadSettings(svc, ring.Timer)
	if after.Volume != 50 || after.Gap != 2*time.Second || after.SpeakLabel != ring.SpeakNever {
		t.Fatalf("timer after = %+v", after)
	}
	if alarm := ring.ReadSettings(svc, ring.Alarm); alarm != ring.Defaults(ring.Alarm) {
		t.Fatalf("alarm changed with the Timer: %+v", alarm)
	}
	if _, err := svc.Reset(ctx, ring.Key(ring.Timer, "volume")); err != nil {
		t.Fatalf("reset volume: %v", err)
	}
	if got := ring.ReadSettings(svc, ring.Timer).Volume; got != 80 {
		t.Fatalf("volume after reset = %d, want the default", got)
	}
}

// TestDefinitionsValidateEveryField keeps a typo or an out-of-range Ring value
// out of the live service.
func TestDefinitionsValidateEveryField(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t)
	cases := []struct {
		key   string
		value string
		want  string
	}{
		{ring.Key(ring.Timer, "max_ring"), `"0s"`, "between 1s and 1h"},
		{ring.Key(ring.Timer, "max_ring"), `"2h"`, "between 1s and 1h"},
		{ring.Key(ring.Timer, "volume"), `101`, "between 0 and 100"},
		{ring.Key(ring.Timer, "volume"), `-1`, "between 0 and 100"},
		{ring.Key(ring.Timer, "ramp"), `"-1s"`, "must not be negative"},
		{ring.Key(ring.Timer, "gap"), `"-1s"`, "must not be negative"},
		{ring.Key(ring.Timer, "speak_label"), `"sometimes"`, "start, every_loop or never"},
		{ring.Key(ring.Alarm, "snooze"), `"0s"`, "between 1s and 1h"},
		{ring.Key(ring.Alarm, "snooze"), `"2h"`, "between 1s and 1h"},
	}
	for _, test := range cases {
		_, err := svc.SetJSON(ctx, test.key, []byte(test.value))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s=%s error = %v, want %q", test.key, test.value, err, test.want)
		}
	}
	// The boundary values are valid.
	for _, test := range []struct {
		key   string
		value string
	}{
		{ring.Key(ring.Timer, "max_ring"), `"1s"`},
		{ring.Key(ring.Timer, "max_ring"), `"1h"`},
		{ring.Key(ring.Timer, "volume"), `0`},
		{ring.Key(ring.Timer, "volume"), `100`},
		{ring.Key(ring.Timer, "ramp"), `"0s"`},
		{ring.Key(ring.Timer, "gap"), `"0s"`},
		{ring.Key(ring.Timer, "speak_label"), `"every_loop"`},
		{ring.Key(ring.Alarm, "snooze"), `"1s"`},
		{ring.Key(ring.Alarm, "snooze"), `"1h"`},
	} {
		if _, err := svc.SetJSON(ctx, test.key, []byte(test.value)); err != nil {
			t.Errorf("%s=%s rejected: %v", test.key, test.value, err)
		}
	}
}

// TestSettingsKeysListThePerKindSurface names the live keys #48 renders and
// #56 edits, including the #55 Cue role, without a duplicate sound key.
func TestSettingsKeysListThePerKindSurface(t *testing.T) {
	keys := ring.SettingsKeys(ring.Timer)
	want := []string{
		"sounds.timer_ring",
		"ring.timer.max_ring",
		"ring.timer.volume",
		"ring.timer.ramp",
		"ring.timer.gap",
		"ring.timer.speak_label",
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("timer keys = %v, want %v", keys, want)
	}
	if got := ring.SoundKey(ring.Alarm); got != "sounds.alarm_ring" {
		t.Fatalf("alarm sound key = %q", got)
	}
	alarmKeys := ring.SettingsKeys(ring.Alarm)
	wantAlarm := []string{
		"sounds.alarm_ring",
		"ring.alarm.max_ring",
		"ring.alarm.snooze",
		"ring.alarm.volume",
		"ring.alarm.ramp",
		"ring.alarm.gap",
		"ring.alarm.speak_label",
	}
	if strings.Join(alarmKeys, ",") != strings.Join(wantAlarm, ",") {
		t.Fatalf("alarm keys = %v, want %v", alarmKeys, wantAlarm)
	}
}
