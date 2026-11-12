package quiet_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	quietcore "github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/quiet"
)

// fixture is the real settings service plus the Quiet service the Tools write.
type fixture struct {
	service  *quietcore.Service
	settings *settings.Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := settings.New(ctx, st, quietcore.Definitions(quietcore.DefaultConfig())...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	fixed := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	service := quietcore.New(quietcore.Options{
		Reader:   svc,
		Now:      func() time.Time { return fixed },
		Location: func() *time.Location { return time.UTC },
	})
	service.BindWriter(svc)
	return fixture{service: service, settings: svc}
}

func TestSetWritesTheSameStoredOverrideAsTheDashboard(t *testing.T) {
	f := newFixture(t)
	set := quiet.NewSet(f.service)
	out, err := set.Handler(context.Background(), quiet.SetIn{Start: "10:30 pm", End: "7"})
	if err != nil {
		t.Fatalf("set handler: %v", err)
	}
	if out.Start != "22:30" || out.End != "07:00" {
		t.Fatalf("out = %+v, want the normalised window", out)
	}
	enabled, _ := f.settings.Get(quietcore.EnabledKey)
	if enabled.Value != true || enabled.Source != settings.SourceOverride {
		t.Fatalf("enabled = %+v, want a stored override", enabled)
	}
	start, _ := f.settings.Get(quietcore.StartKey)
	if start.Value != "22:30" || start.Source != settings.SourceOverride {
		t.Fatalf("start = %+v, want the stored override", start)
	}
	// The same service is what the Dashboard edits; a later Dashboard-style
	// write is visible without rebuilding the Tool.
	if _, err := f.settings.Set(context.Background(), quietcore.EndKey, "06:30"); err != nil {
		t.Fatalf("Dashboard Set: %v", err)
	}
	status := f.service.Status()
	if status.End != "06:30" || !status.Quiet {
		t.Fatalf("status = %+v, want the Dashboard end applied live", status)
	}
	if got := set.Speak(out); !strings.Contains(got, "10:30 pm") || !strings.Contains(got, "7:00 am") {
		t.Fatalf("Speak = %q, want the friendly window", got)
	}
}

func TestOffClearsDNDAndAwake(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Awake(context.Background()); err != nil {
		t.Fatalf("Awake: %v", err)
	}
	if _, err := f.service.StartDND(context.Background(), time.Hour); err != nil {
		t.Fatalf("StartDND: %v", err)
	}
	off := quiet.NewOff(f.service)
	out, err := off.Handler(context.Background(), quiet.OffIn{})
	if err != nil {
		t.Fatalf("off handler: %v", err)
	}
	if !out.Off || off.Speak(out) != "Quiet hours off." {
		t.Fatalf("off = %+v / %q", out, off.Speak(out))
	}
	if f.service.IsQuiet() {
		t.Fatal("quiet is still active after quiet.off")
	}
	for _, key := range []string{quietcore.DNDUntilKey, quietcore.AwakeUntilKey} {
		if value := f.settings.String(key); value != "" {
			t.Fatalf("%s = %q, want cleared", key, value)
		}
	}
}

func TestDNDStoresAnAbsoluteExpiry(t *testing.T) {
	f := newFixture(t)
	dnd := quiet.NewDND(f.service)
	out, err := dnd.Handler(context.Background(), quiet.DNDIn{DurationSeconds: 2 * 60 * 60})
	if err != nil {
		t.Fatalf("dnd handler: %v", err)
	}
	if out.Until != "2026-11-13T01:00:00Z" {
		t.Fatalf("until = %q, want two hours after the injected clock", out.Until)
	}
	if got := dnd.Speak(out); got != "Do not disturb for 2 hours." {
		t.Fatalf("Speak = %q", got)
	}
	if !f.service.IsQuiet() {
		t.Fatal("do not disturb did not make Quiet active")
	}
	if _, err := dnd.Handler(context.Background(), quiet.DNDIn{}); err == nil {
		t.Fatal("dnd accepted a missing duration")
	}
}

func TestAwakeBypassesTheWindowAndSaysSo(t *testing.T) {
	f := newFixture(t)
	// The fixture starts at 23:00 UTC with 22:30-07:00 configured but disabled;
	// enable the window through the same override the Dashboard writes.
	if _, err := f.settings.Set(context.Background(), quietcore.EnabledKey, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	awake := quiet.NewAwake(f.service)
	out, err := awake.Handler(context.Background(), quiet.AwakeIn{})
	if err != nil {
		t.Fatalf("awake handler: %v", err)
	}
	if !out.Bypassed {
		t.Fatalf("out = %+v, want a bypassed window", out)
	}
	if got := awake.Speak(out); got != "Okay, I'm awake. Quiet hours are off until the window ends." {
		t.Fatalf("Speak = %q", got)
	}
	if f.service.IsQuiet() {
		t.Fatal("quiet is still active after quiet.awake")
	}
}

func TestExtractSetWindow(t *testing.T) {
	cases := map[string][2]string{
		"set quiet hours from 10:30 to 7":                       {"22:30", "07:00"},
		"set quiet hours from 11 pm to 6 am":                    {"23:00", "06:00"},
		"set quiet hours from 9 at night to 6 in the morning":   {"21:00", "06:00"},
		"set my quiet hours from 10 pm to 7 am":                 {"22:00", "07:00"},
		"set quiet hours from 10:30 to 7 am":                    {"22:30", "07:00"},
		"quiet hours from 11:30 pm until 6:30 am":               {"23:30", "06:30"},
		"quiet hours should run from 11 to 7":                   {"23:00", "07:00"},
		"start quiet hours at 10 pm and end at 7":               {"22:00", "07:00"},
		"quiet hours begin at 10 and end at 6":                  {"22:00", "06:00"},
		"quiet hours from 9 in the evening to 6 in the morning": {"21:00", "06:00"},
		"quiet hours from 12 to 6":                              {"00:00", "06:00"},
	}
	f := newFixture(t)
	set := quiet.NewSet(f.service)
	for utterance, want := range cases {
		in, ok := set.Extract(utterance)
		if !ok {
			t.Errorf("Extract(%q) did not parse a window", utterance)
			continue
		}
		if in.Start != want[0] || in.End != want[1] {
			t.Errorf("Extract(%q) = %s-%s, want %s-%s", utterance, in.Start, in.End, want[0], want[1])
		}
	}
	for _, utterance := range []string{
		"set quiet hours for two hours",
		"turn off quiet hours",
		"play some quiet music at 10",
		"quiet hours",
	} {
		if _, ok := set.Extract(utterance); ok {
			t.Errorf("Extract(%q) parsed a window, want a miss", utterance)
		}
	}
}

func TestExtractOffDNDAwake(t *testing.T) {
	f := newFixture(t)
	off := quiet.NewOff(f.service)
	for _, utterance := range []string{
		"turn off quiet hours",
		"disable quiet hours",
		"quiet hours are over",
		"I don't want quiet hours anymore",
		"get rid of quiet hours",
		"turn off the quiet window",
	} {
		if _, ok := off.Extract(utterance); !ok {
			t.Errorf("off Extract(%q) missed", utterance)
		}
	}
	for _, utterance := range []string{
		"set quiet hours from 10 to 7",
		"turn off the lights",
		"turn off the quiet music",
		"do not disturb for two hours",
	} {
		if _, ok := off.Extract(utterance); ok {
			t.Errorf("off Extract(%q) matched", utterance)
		}
	}

	dnd := quiet.NewDND(f.service)
	for utterance, want := range map[string]int{
		"do not disturb for two hours": 7200,
		"dnd for 45 minutes":           2700,
		"mute everything for an hour":  3600,
		"quiet down for the next hour": 3600,
	} {
		in, ok := dnd.Extract(utterance)
		if !ok || in.DurationSeconds != want {
			t.Errorf("dnd Extract(%q) = %+v, %v; want %d", utterance, in, ok, want)
		}
	}
	for _, utterance := range []string{
		"do not disturb me",
		"quiet hours from 10 to 7",
		"set a timer for two hours",
		"give me a timer for five minutes",
	} {
		if _, ok := dnd.Extract(utterance); ok {
			t.Errorf("dnd Extract(%q) matched", utterance)
		}
	}

	awake := quiet.NewAwake(f.service)
	for _, utterance := range []string{"I'm awake", "I am awake", "I'm up", "awake now", "stop whispering, I'm awake"} {
		if _, ok := awake.Extract(utterance); !ok {
			t.Errorf("awake Extract(%q) missed", utterance)
		}
	}
	for _, utterance := range []string{"turn off quiet hours", "set quiet hours from 10 to 7", "play some music"} {
		if _, ok := awake.Extract(utterance); ok {
			t.Errorf("awake Extract(%q) matched", utterance)
		}
	}
}

func TestEveryQuietToolIsActAndLocalOnly(t *testing.T) {
	reg := tool.NewRegistry(2 * time.Second)
	if err := quiet.NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range []string{quiet.SetName, quiet.OffName, quiet.DNDName, quiet.AwakeName} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if info.Permission != tool.PermissionAct {
			t.Errorf("%s Permission = %q, want act", name, info.Permission)
		}
		if !info.LocalOnly {
			t.Errorf("%s is not LocalOnlyYes", name)
		}
		if !info.HasSpeak || !info.HasExtract {
			t.Errorf("%s must Speak and Extract to stay on the Fast path", name)
		}
		if len(info.Examples) < 8 {
			t.Errorf("%s has %d Examples, want a useful set", name, len(info.Examples))
		}
	}
}
