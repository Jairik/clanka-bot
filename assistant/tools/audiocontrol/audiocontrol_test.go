package audiocontrol_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
)

// fakeControls records the audio commands the Tools issue.
type fakeControls struct {
	volume   int
	delta    int
	stopped  int
	setCalls int
	adjCalls int
}

func (f *fakeControls) SetVolume(_ context.Context, percent int) error {
	f.volume = percent
	f.setCalls++
	return nil
}

func (f *fakeControls) AdjustVolume(_ context.Context, delta int) error {
	f.delta = delta
	f.adjCalls++
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
	if want := "Volume set to 40 percent."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestVolumeSetAcceptsZeroPercent(t *testing.T) {
	controls := &fakeControls{}
	reg := newRegistry(t, controls)

	// Zero is a normal volume (mute), not a missing argument.
	spoken := runTool(t, reg, audiocontrol.VolumeName, "turn the volume to zero")
	if controls.setCalls != 1 || controls.volume != 0 {
		t.Fatalf("SetVolume calls = %d volume = %d, want one call at 0", controls.setCalls, controls.volume)
	}
	if want := "Volume set to 0 percent."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}

	// The model may pass percent 0 explicitly; it must mute, not step up.
	result, err := reg.Execute(context.Background(), audiocontrol.VolumeName, json.RawMessage(`{"percent":0}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.setCalls != 2 || controls.volume != 0 || controls.adjCalls != 0 {
		t.Fatalf("calls = %d set / %d adjust, want a second absolute set at 0", controls.setCalls, controls.adjCalls)
	}
	if spoken, _ := reg.Speak(audiocontrol.VolumeName, result); spoken != "Volume set to 0 percent." {
		t.Fatalf("Speak = %q, want %q", spoken, "Volume set to 0 percent.")
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
	if want := "Volume up."; spoken != want {
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
