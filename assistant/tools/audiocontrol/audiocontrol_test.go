package audiocontrol_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
)

// fakeControls records the audio commands the Tools issue.
type fakeControls struct {
	volume     int
	delta      int
	stopped    int
	setCalls   int
	adjCalls   int
	level      int
	levelCalls int
	muteCalls  int
	muted      bool
	// adjustNoop makes a step report that it hit the ceiling or floor and
	// wrote nothing, without the fake touching a device.
	adjustNoop bool
}

func (f *fakeControls) SetVolume(_ context.Context, percent int) error {
	f.volume = percent
	f.setCalls++
	return nil
}

func (f *fakeControls) AdjustVolume(_ context.Context, delta int) (bool, error) {
	f.delta = delta
	f.adjCalls++
	return !f.adjustNoop, nil
}

func (f *fakeControls) Level(context.Context) (int, error) {
	f.levelCalls++
	return f.level, nil
}

func (f *fakeControls) SetMute(_ context.Context, muted bool) error {
	f.muted = muted
	f.muteCalls++
	return nil
}

func (f *fakeControls) Stop(context.Context) error {
	f.stopped++
	return nil
}

func newRegistry(t *testing.T, controls audiocontrol.Controls) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := audiocontrol.Register(reg, controls); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// runTool extracts arguments from an utterance, executes the Tool and speaks
// the result, matching what the Fast path does.
func runTool(t *testing.T, reg *tool.Registry, name, utterance string) string {
	t.Helper()
	args, ok := reg.Extract(name, utterance)
	if !ok {
		t.Fatalf("Extract(%q) failed", utterance)
	}
	result, err := reg.Execute(context.Background(), name, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, ok := reg.Speak(name, result)
	if !ok {
		t.Fatal("Speak returned false")
	}
	return spoken
}

func TestVolumeSetExtractsAbsolutePercent(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "turn the volume to 40")
	if controls.setCalls != 1 || controls.volume != 40 {
		t.Fatalf("SetVolume calls = %d volume = %d, want one call at 40", controls.setCalls, controls.volume)
	}
	// An absolute level above zero clears a stale mute, as the Dashboard does.
	if controls.muteCalls != 1 || controls.muted {
		t.Fatalf("SetMute calls = %d muted = %v, want one clear", controls.muteCalls, controls.muted)
	}
	if want := "Volume set to 40 percent."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeSetAcceptsZeroPercent(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	// Zero is a normal volume (makes it silent), not a missing argument, and it
	// only moves the level: the mute flag keeps its state.
	spoken := runTool(t, reg, audiocontrol.VolumeName, "turn the volume to zero")
	if controls.setCalls != 1 || controls.volume != 0 {
		t.Fatalf("SetVolume calls = %d volume = %d, want one call at 0", controls.setCalls, controls.volume)
	}
	if controls.muteCalls != 0 {
		t.Fatalf("SetMute calls = %d at an absolute zero, want none", controls.muteCalls)
	}
	if want := "Volume set to 0 percent."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}

	// The model may pass percent 0 explicitly; it must set the level, not step up.
	result, err := reg.Execute(context.Background(), audiocontrol.VolumeName, json.RawMessage(`{"percent":0}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.setCalls != 2 || controls.volume != 0 || controls.adjCalls != 0 || controls.muteCalls != 0 {
		t.Fatalf("calls = %d set / %d adjust / %d mute, want a second absolute set at 0",
			controls.setCalls, controls.adjCalls, controls.muteCalls)
	}
	if spoken, _ := reg.Speak(audiocontrol.VolumeName, result); spoken != "Volume set to 0 percent." {
		t.Fatalf("Speak = %q, want %q", spoken, "Muted.")
	}
}

func TestVolumeSetRejectsOutOfRangePercent(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	_, err := reg.Execute(context.Background(), audiocontrol.VolumeName, json.RawMessage(`{"percent":101}`))
	if err == nil {
		t.Fatal("Execute succeeded, want an out-of-range error")
	}
	if controls.setCalls != 0 {
		t.Fatalf("SetVolume calls = %d, want none", controls.setCalls)
	}
	if got := tool.FailureMessage(err); got != "I can only set the volume between 0 and 100." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

func TestVolumeSetExtractsLouderAsAStep(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "louder")
	if controls.adjCalls != 1 || controls.delta != 10 {
		t.Fatalf("AdjustVolume calls = %d delta = %d, want one call at +10", controls.adjCalls, controls.delta)
	}
	// Raising the level does not clear a genuine mute; that needs "unmute".
	if controls.muteCalls != 0 {
		t.Fatalf("SetMute calls = %d after a step, want none", controls.muteCalls)
	}
	if want := "Volume up."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeSetExtractsQuieterAsANegativeStep(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "quieter")
	if controls.adjCalls != 1 || controls.delta != -10 {
		t.Fatalf("AdjustVolume calls = %d delta = %d, want one call at -10", controls.adjCalls, controls.delta)
	}
	if want := "Volume down."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeLouderAtTheCeilingReportsNoChange(t *testing.T) {
	controls := &fakeControls{adjustNoop: true}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "louder")
	if controls.adjCalls != 1 || controls.delta != 10 {
		t.Fatalf("AdjustVolume calls = %d delta = %d, want one call at +10", controls.adjCalls, controls.delta)
	}
	// A step that hit the ceiling must not fall back to an absolute write or a
	// mute change; the level was left alone.
	if controls.setCalls != 0 || controls.muteCalls != 0 {
		t.Fatalf("set calls = %d mute calls = %d, want no write", controls.setCalls, controls.muteCalls)
	}
	if want := "Volume is already at 100 percent."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeQuieterAtTheFloorReportsNoChange(t *testing.T) {
	controls := &fakeControls{adjustNoop: true}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "quieter")
	if controls.adjCalls != 1 || controls.delta != -10 {
		t.Fatalf("AdjustVolume calls = %d delta = %d, want one call at -10", controls.adjCalls, controls.delta)
	}
	if controls.setCalls != 0 || controls.muteCalls != 0 {
		t.Fatalf("set calls = %d mute calls = %d, want no write", controls.setCalls, controls.muteCalls)
	}
	if want := "Volume is already at 0 percent."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeSetMutesThroughTheSinkFlag(t *testing.T) {
	controls := &fakeControls{level: 40}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "mute")
	// Mute keeps the level, so unmuting restores what was playing.
	if controls.muteCalls != 1 || !controls.muted {
		t.Fatalf("SetMute calls = %d muted = %v, want one mute", controls.muteCalls, controls.muted)
	}
	if controls.setCalls != 0 || controls.adjCalls != 0 {
		t.Fatalf("set calls = %d adjust = %d, want no level change", controls.setCalls, controls.adjCalls)
	}
	if want := "Muted."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}

	// The model may pass the flag explicitly too.
	result, err := reg.Execute(context.Background(), audiocontrol.VolumeName, json.RawMessage(`{"mute":true}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.muteCalls != 2 || !controls.muted || controls.setCalls != 0 {
		t.Fatalf("calls = %d mute / %d set, want a second mute flag write only",
			controls.muteCalls, controls.setCalls)
	}
	if spoken, _ := reg.Speak(audiocontrol.VolumeName, result); spoken != "Muted." {
		t.Fatalf("Speak = %q, want %q", spoken, "Muted.")
	}
}

func TestUnmuteClearsTheFlagAndKeepsTheLevel(t *testing.T) {
	controls := &fakeControls{level: 40}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "unmute")
	if controls.muteCalls != 1 || controls.muted {
		t.Fatalf("SetMute calls = %d muted = %v, want one clear", controls.muteCalls, controls.muted)
	}
	if controls.setCalls != 0 || controls.adjCalls != 0 {
		t.Fatalf("set calls = %d adjust = %d, want no level change above zero",
			controls.setCalls, controls.adjCalls)
	}
	if want := "Unmuted."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestUnmuteFromZeroAppliesOneStep(t *testing.T) {
	// An earlier absolute zero left nothing to restore, so one step is the
	// honest recovery instead of silence.
	controls := &fakeControls{level: 0}
	reg := newRegistry(t, controls)

	spoken := runTool(t, reg, audiocontrol.VolumeName, "unmute")
	if controls.muteCalls != 1 || controls.muted {
		t.Fatalf("SetMute calls = %d muted = %v, want one clear", controls.muteCalls, controls.muted)
	}
	if controls.adjCalls != 1 || controls.delta != 10 || controls.setCalls != 0 {
		t.Fatalf("set calls = %d adjust = %d delta = %d, want one +10 step",
			controls.setCalls, controls.adjCalls, controls.delta)
	}
	// The reply names the action, not the side effect.
	if want := "Unmuted."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeSetWithoutArgumentsTurnsItUp(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	result, err := reg.Execute(context.Background(), audiocontrol.VolumeName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.adjCalls != 1 || controls.delta != 10 {
		t.Fatalf("AdjustVolume calls = %d delta = %d, want one call at +10", controls.adjCalls, controls.delta)
	}
	if spoken, _ := reg.Speak(audiocontrol.VolumeName, result); spoken != "Volume up." {
		t.Fatalf("Speak = %q, want %q", spoken, "Volume up.")
	}
}

func TestAudioStopStopsPlayback(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	result, err := reg.Execute(context.Background(), audiocontrol.StopName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.stopped != 1 {
		t.Fatalf("Stop calls = %d, want 1", controls.stopped)
	}
	spoken, ok := reg.Speak(audiocontrol.StopName, result)
	if !ok {
		t.Fatal("Speak returned false")
	}
	if want := "Stopped."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeExtractFailsWithoutANumberOrDirection(t *testing.T) {
	reg := newRegistry(t, &fakeControls{})
	if _, ok := reg.Extract(audiocontrol.VolumeName, "what's the weather"); ok {
		t.Fatal("Extract succeeded, want failure so the request falls through")
	}
}

// TestVolumeExtractGrammar exercises the shared direction-plus-number grammar
// through the real Extract path: a number after "to" is absolute, a number
// after a bare direction is relative, and a bare direction is one step.
func TestVolumeExtractGrammar(t *testing.T) {
	reg := newRegistry(t, &fakeControls{})
	cases := []struct {
		text string
		want audiocontrol.VolumeIn
	}{
		{"turn the volume down to 20", audiocontrol.VolumeIn{Percent: pi(20)}},
		{"turn it up to 80", audiocontrol.VolumeIn{Percent: pi(80)}},
		{"turn the volume down by 30 percent", audiocontrol.VolumeIn{Delta: -30}},
		{"volume up to 70", audiocontrol.VolumeIn{Percent: pi(70)}},
		{"set the volume to 5 percent", audiocontrol.VolumeIn{Percent: pi(5)}},
		{"volume 100", audiocontrol.VolumeIn{Percent: pi(100)}},
		{"turn the volume to 40", audiocontrol.VolumeIn{Percent: pi(40)}},
		{"louder", audiocontrol.VolumeIn{Delta: 10}},
		{"quieter", audiocontrol.VolumeIn{Delta: -10}},
	}
	for _, tc := range cases {
		args, ok := reg.Extract(audiocontrol.VolumeName, tc.text)
		if !ok {
			t.Fatalf("Extract(%q) failed", tc.text)
		}
		var got audiocontrol.VolumeIn
		if err := json.Unmarshal(args, &got); err != nil {
			t.Fatalf("Extract(%q) unmarshal: %v", tc.text, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Extract(%q) = %+v, want %+v", tc.text, got, tc.want)
		}
	}
}

// TestVolumeExtractKeepsMuteAndUnmute confirms the extractor still drives the
// sink mute flag ahead of the number grammar.
func TestVolumeExtractKeepsMuteAndUnmute(t *testing.T) {
	reg := newRegistry(t, &fakeControls{})
	cases := []struct {
		text string
		want audiocontrol.VolumeIn
	}{
		{"mute", audiocontrol.VolumeIn{Mute: pb(true)}},
		{"mute the volume", audiocontrol.VolumeIn{Mute: pb(true)}},
		{"unmute", audiocontrol.VolumeIn{Mute: pb(false)}},
	}
	for _, tc := range cases {
		args, ok := reg.Extract(audiocontrol.VolumeName, tc.text)
		if !ok {
			t.Fatalf("Extract(%q) failed", tc.text)
		}
		var got audiocontrol.VolumeIn
		if err := json.Unmarshal(args, &got); err != nil {
			t.Fatalf("Extract(%q) unmarshal: %v", tc.text, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Extract(%q) = %+v, want %+v", tc.text, got, tc.want)
		}
	}
}

func pi(n int) *int  { return &n }
func pb(b bool) *bool { return &b }

func TestVolumeToolsAreFastPathEligible(t *testing.T) {
	reg := newRegistry(t, &fakeControls{})
	for _, name := range []string{audiocontrol.VolumeName, audiocontrol.StopName} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if info.Permission != tool.PermissionAct {
			t.Errorf("%s Permission = %q, want act", name, info.Permission)
		}
		if info.LocalOnly || !info.HasSpeak || len(info.Examples) == 0 {
			t.Errorf("%s metadata = %+v, want non-local, spoken Tool with Examples", name, info)
		}
	}
	if info, _ := reg.Info(audiocontrol.VolumeName); !info.HasExtract {
		t.Error("audio.volume_set has no Extract")
	}
	if info, _ := reg.Info(audiocontrol.StopName); info.HasExtract {
		t.Error("audio.stop should not need Extract")
	}
}
