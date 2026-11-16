package assistant_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/assistant"
)

// extractRate runs the Tool's Extract and decodes its input for assertions.
func extractRate(t *testing.T, reg *tool.Registry, utterance string) (assistant.RepeatIn, bool) {
	t.Helper()
	raw, ok := reg.Extract(assistant.RepeatName, utterance)
	if !ok {
		return assistant.RepeatIn{}, false
	}
	var in assistant.RepeatIn
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decode Extract(%q) = %s: %v", utterance, raw, err)
	}
	return in, true
}

func TestRepeatExtractRatePhrasings(t *testing.T) {
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(lastreply.New())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cases := []struct {
		utterance string
		want      string
		ok        bool
	}{
		{"say that again slower", assistant.RepeatSlower, true},
		{"again but slower", assistant.RepeatSlower, true},
		{"a bit slower", assistant.RepeatSlower, true},
		{"say it again more slowly", assistant.RepeatSlower, true},
		{"say that again faster", assistant.RepeatFaster, true},
		{"again but faster", assistant.RepeatFaster, true},
		{"repeat that faster please", assistant.RepeatFaster, true},
		{"at half speed", assistant.RepeatHalf, true},
		{"half as fast", assistant.RepeatHalf, true},
		{"at double speed", assistant.RepeatDouble, true},
		{"twice as fast", assistant.RepeatDouble, true},
		// A repeat with no rate word keeps the setting.
		{"say that again", "", true},
		{"what did you say", "", true},
		// Playback speed is another Tool's intent.
		{"make the music slower", "", false},
		{"turn the volume down", "", false},
	}
	for _, tc := range cases {
		in, ok := extractRate(t, reg, tc.utterance)
		if ok != tc.ok {
			t.Errorf("Extract(%q) ok = %v, want %v", tc.utterance, ok, tc.ok)
			continue
		}
		if ok && in.Rate != tc.want {
			t.Errorf("Extract(%q).Rate = %q, want %q", tc.utterance, in.Rate, tc.want)
		}
	}
}

func TestRepeatRateChangesOnlyTheReplayRate(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "It's 3:04 PM.", Tool: "time.now"})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A plain repeat keeps the setting (rate 0 means "no override").
	normal := execute(t, reg, assistant.RepeatName).(assistant.RepeatOut)
	if normal.Rate != 0 || normal.SpeechRate() != 0 {
		t.Fatalf("plain repeat rate = %v/%v, want 0", normal.Rate, normal.SpeechRate())
	}
	if normal.Text != "It's 3:04 PM." {
		t.Fatalf("plain repeat text = %q", normal.Text)
	}

	// A slower replay speaks the same text at a lower rate; the provenance and
	// the persistent rate setting are untouched.
	slower := executeArgs(t, reg, `{"rate":"slower"}`).(assistant.RepeatOut)
	if slower.Rate != 0.8 {
		t.Fatalf("slower rate = %v, want 0.8", slower.Rate)
	}
	if slower.SpeechRate() != slower.Rate {
		t.Fatal("SpeechRate() does not report the replay rate")
	}
	if slower.Text != normal.Text || slower.Tool != "time.now" {
		t.Fatalf("slower replay = %+v, want the same text and provenance", slower)
	}
	if slower.LocalOnly {
		t.Fatal("slower replay invented Local-only provenance")
	}

	faster := executeArgs(t, reg, `{"rate":"faster"}`).(assistant.RepeatOut)
	if faster.Rate != 1.2 {
		t.Fatalf("faster rate = %v, want 1.2", faster.Rate)
	}
}

func TestRepeatRateClampsToTheTTSRange(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "previous"})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cases := []struct {
		name string
		args string
		want float64
	}{
		{"half", `{"rate":"half"}`, 0.5},
		{"double", `{"rate":"double"}`, 2.0},
	}
	for _, tc := range cases {
		out := executeArgs(t, reg, tc.args).(assistant.RepeatOut)
		if out.Rate != tc.want {
			t.Errorf("%s rate = %v, want %v", tc.name, out.Rate, tc.want)
		}
	}

	// A step at the boundary stays clamped: store a boundary rate and step past it.
	for _, tc := range []struct {
		previous  float64
		direction string
		want      float64
	}{
		{0.5, "slower", 0.5},
		{2.0, "faster", 2.0},
	} {
		store := lastreply.New()
		store.Record(lastreply.Reply{Text: "boundary", SpeechRate: tc.previous})
		boundaryReg := newRegistry(t)
		if err := tool.Add(boundaryReg, assistant.NewRepeat(store)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		args, _ := json.Marshal(assistant.RepeatIn{Rate: tc.direction})
		out := executeArgs(t, boundaryReg, string(args)).(assistant.RepeatOut)
		if out.Rate != tc.want {
			t.Errorf("previous %v %s = %v, want %v", tc.previous, tc.direction, out.Rate, tc.want)
		}
	}
}

func TestRepeatStepUsesPreviousRateNotTheSetting(t *testing.T) {
	// The last reply used an override; the current setting is irrelevant to the
	// step and must not be consulted or changed.
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "previous", SpeechRate: 1.5})
	reg := newRegistry(t)
	called := false
	current := func() float64 {
		called = true
		return 1.0
	}
	if err := tool.Add(reg, assistant.NewRepeat(store, current)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := executeArgs(t, reg, `{"rate":"faster"}`).(assistant.RepeatOut)
	want := 1.5 * 1.2 // 1.8, stepped from 1.5, not from 1.0
	if out.Rate != want {
		t.Fatalf("rate = %v, want %v (stepped from the previous override)", out.Rate, want)
	}
	if called {
		t.Fatal("the live voice.rate source was consulted despite a stored override")
	}
}

func executeArgs(t *testing.T, reg *tool.Registry, args string) any {
	t.Helper()
	result, err := reg.Execute(context.Background(), assistant.RepeatName, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute %s %s: %v", assistant.RepeatName, args, err)
	}
	return result
}
