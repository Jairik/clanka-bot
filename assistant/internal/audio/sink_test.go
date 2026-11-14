package audio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// fakePacat writes the arguments it received and the PCM it read, then exits.
func fakePacat(t *testing.T) (binary, argsPath, pcmPath string) {
	t.Helper()
	dir := t.TempDir()
	argsPath = filepath.Join(dir, "args.txt")
	pcmPath = filepath.Join(dir, "pcm.raw")
	binary = filepath.Join(dir, "pacat")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\ncat > '" + pcmPath + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	return binary, argsPath, pcmPath
}

func TestPulseSinkPlaysPCMThroughPacat(t *testing.T) {
	binary, argsPath, pcmPath := fakePacat(t)
	sink := audio.NewPulseSink(binary, "echo_dot")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	writer, err := sink.Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := writer.Write([]byte{1, 2}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := writer.Write([]byte{3, 4}); err != nil {
		t.Fatalf("Write second chunk: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	for _, want := range []string{"--format=s16le", "--rate=24000", "--channels=1", "--raw", "--device=echo_dot", "--latency-msec="} {
		if !strings.Contains(string(args), want) {
			t.Errorf("pacat args missing %q in:\n%s", want, args)
		}
	}
	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		t.Fatalf("read pcm: %v", err)
	}
	if string(pcm) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("pcm = %v, want [1 2 3 4]", pcm)
	}
}

func TestPulseSinkUsesDefaultDeviceWhenUnset(t *testing.T) {
	binary, argsPath, _ := fakePacat(t)
	sink := audio.NewPulseSink(binary, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	writer, err := sink.Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if strings.Contains(string(args), "--device=") {
		t.Fatalf("unexpected --device in:\n%s", args)
	}
}

func TestPulseSinkRejectsUnsupportedWidth(t *testing.T) {
	binary, _, _ := fakePacat(t)
	sink := audio.NewPulseSink(binary, "")
	if _, err := sink.Open(context.Background(), audio.Format{Rate: 24000, Width: 3, Channels: 1}); err == nil {
		t.Fatal("Open succeeded, want unsupported width error")
	}
}

func TestPulseSinkStopsWhenContextCancelled(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pacat")
	// Hold stdin open without reading so the writer blocks until the process dies.
	script := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	writer, err := audio.NewPulseSink(binary, "").Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := writer.Write(chunk); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Write succeeded after cancel, want error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Write did not return after cancel")
	}
	if err := writer.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
}

func TestPulseSinkStopCancelsActivePlayback(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pacat")
	script := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	sink := audio.NewPulseSink(binary, "")
	writer, err := sink.Open(context.Background(), audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := sink.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := writer.Write(make([]byte, 64)); err == nil {
		t.Fatal("Write succeeded after Stop, want cancellation")
	}
	if err := writer.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
}

func fakePactl(t *testing.T) (binary, argsPath string) {
	t.Helper()
	dir := t.TempDir()
	argsPath = filepath.Join(dir, "pactl-args.txt")
	binary = filepath.Join(dir, "pactl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}
	return binary, argsPath
}

func TestPulseSinkSetVolumeRunsPactl(t *testing.T) {
	// The pactl fake sits beside the pacat path so the sink finds it.
	pactl, argsPath := fakePactl(t)
	pacat := filepath.Join(filepath.Dir(pactl), "pacat")
	if err := os.WriteFile(pacat, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	sink := audio.NewPulseSink(pacat, "echo_dot")

	if err := sink.SetVolume(context.Background(), 40); err != nil {
		t.Fatalf("SetVolume: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	for _, want := range []string{"set-sink-volume", "echo_dot", "40%"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("pactl args missing %q in:\n%s", want, args)
		}
	}
}

func TestPulseSinkAdjustVolumeUsesRelativePercent(t *testing.T) {
	pactl, argsPath := fakePactl(t)
	pacat := filepath.Join(filepath.Dir(pactl), "pacat")
	if err := os.WriteFile(pacat, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	sink := audio.NewPulseSink(pacat, "")

	if err := sink.AdjustVolume(context.Background(), 10); err != nil {
		t.Fatalf("AdjustVolume: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	for _, want := range []string{"set-sink-volume", "@DEFAULT_SINK@", "+10%"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("pactl args missing %q in:\n%s", want, args)
		}
	}
}

func TestPulseSinkVolumeReadsLevelAndMute(t *testing.T) {
	dir := t.TempDir()
	pactl := filepath.Join(dir, "pactl")
	script := `#!/bin/sh
case "$1" in
get-sink-volume) echo "Volume: front-left: 42598 /  65% / -11.23 dB,   front-right: 42598 /  65% / -11.23 dB" ;;
get-sink-mute) echo "Mute: yes" ;;
esac
`
	if err := os.WriteFile(pactl, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}
	sink := audio.NewPulseSink(filepath.Join(dir, "pacat"), "")
	state, err := sink.Volume(context.Background())
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if state.Percent != 65 || !state.Muted {
		t.Fatalf("state = %+v, want 65%% muted", state)
	}
}

func TestPulseSinkSetMuteKeepsTheLevel(t *testing.T) {
	pactl, argsPath := fakePactl(t)
	sink := audio.NewPulseSink(filepath.Join(filepath.Dir(pactl), "pacat"), "echo_dot")
	if err := sink.SetMute(context.Background(), true); err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if got := strings.Fields(string(args)); strings.Join(got, " ") != "set-sink-mute echo_dot 1" {
		t.Fatalf("pactl args = %q", got)
	}
}

// A live EQ changes playback routing without changing the speaker volume destination.
func TestLiveEQDevice(t *testing.T) {
	binary, argsPath, _ := fakePacat(t)
	sink := audio.NewPulseSink(binary, "aec_music_sink")
	sink.Device = func() string { return "clanka_eq_a" }
	writer, err := sink.Open(context.Background(), audio.Format{Rate: 48000, Width: 2, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--device=clanka_eq_a") {
		t.Fatal(string(args))
	}
}

// Volume must address the original AEC/output even after EQ becomes the default.
func TestEQVolumeMaster(t *testing.T) {
	pactl, argsPath := fakePactl(t)
	sink := audio.NewPulseSink(filepath.Join(filepath.Dir(pactl), "pacat"), "")
	sink.VolumeDevice = func() string { return "aec_music_sink" }
	if err := sink.SetVolume(context.Background(), 40); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "aec_music_sink") || strings.Contains(string(args), "@DEFAULT_SINK@") {
		t.Fatal(string(args))
	}
}
