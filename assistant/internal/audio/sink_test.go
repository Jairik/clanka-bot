package audio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// fakePactlAtLevel answers get-sink-volume with percent and appends every call
// to a log, so a test can tell the level read from a write.
func fakePactlAtLevel(t *testing.T, percent int) (pacat, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "pactl-log.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"case \"$1\" in\n" +
		"get-sink-volume) echo \"Volume: front-left: 1 / " + strconv.Itoa(percent) + "% / -1.00 dB\" ;;\n" +
		"get-sink-mute) echo \"Mute: no\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "pactl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}
	return filepath.Join(dir, "pacat"), logPath
}

// pactlCalls returns one argument line per fake pactl invocation.
func pactlCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read pactl log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
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

// A relative step reads the level, caps the target at 0-100 and writes only a
// real change, so "louder" can never reach PulseAudio's amplification range.
func TestPulseSinkAdjustVolumeCapsAtTheCeiling(t *testing.T) {
	for _, test := range []struct {
		name  string
		start int
		delta int
		want  string // the one set-sink-volume line; empty means no write
	}{
		{name: "a step reaching the ceiling", start: 95, delta: 10, want: "set-sink-volume @DEFAULT_SINK@ 100%"},
		{name: "an up step at the ceiling", start: 100, delta: 10},
		{name: "a down step at the floor", start: 0, delta: -10},
		{name: "a down step", start: 40, delta: -10, want: "set-sink-volume @DEFAULT_SINK@ 30%"},
		{name: "an up step above the ceiling", start: 105, delta: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			pacat, logPath := fakePactlAtLevel(t, test.start)
			sink := audio.NewPulseSink(pacat, "")

			if err := sink.AdjustVolume(context.Background(), test.delta); err != nil {
				t.Fatalf("AdjustVolume: %v", err)
			}
			calls := pactlCalls(t, logPath)
			if first := calls[0]; first != "get-sink-volume @DEFAULT_SINK@" {
				t.Fatalf("first call = %q, want a level read", first)
			}
			var writes []string
			for _, call := range calls {
				if strings.HasPrefix(call, "set-sink-volume") {
					writes = append(writes, call)
				}
			}
			if test.want == "" {
				if len(writes) != 0 {
					t.Fatalf("writes = %v, want none", writes)
				}
				return
			}
			if len(writes) != 1 || writes[0] != test.want {
				t.Fatalf("writes = %v, want %q", writes, test.want)
			}
		})
	}
}

// Level must not depend on get-sink-mute: a Bluetooth sink can answer that
// slowly or not at all, and a step still has to work.
func TestPulseSinkLevelSkipsTheMuteQuery(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"get-sink-volume) echo \"Volume: mono: 26214 / 40% / -23.88 dB\" ;;\n" +
		"get-sink-mute) echo \"connection failure\" >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "pactl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}
	sink := audio.NewPulseSink(filepath.Join(dir, "pacat"), "")

	level, err := sink.Level(context.Background())
	if err != nil {
		t.Fatalf("Level: %v", err)
	}
	if level != 40 {
		t.Fatalf("level = %d, want 40", level)
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

// TestPulseSinkVolumeReadsConcurrently proves the level and mute queries overlap
// instead of running back to back. Each verb sleeps 900 ms and the read gets
// 1.6 s: concurrent calls finish with ~700 ms to spare on a loaded runner, while
// serialised calls always need at least 1.8 s (sleep never returns early) and the
// second one is killed. That is the whole point of the test, so do not "round"
// these numbers; it costs about a second.
func TestPulseSinkVolumeReadsConcurrently(t *testing.T) {
	dir := t.TempDir()
	pactl := filepath.Join(dir, "pactl")
	script := `#!/bin/sh
case "$1" in
get-sink-volume) sleep 0.9; echo "Volume: front-left: 42598 /  65% / -11.23 dB" ;;
get-sink-mute) sleep 0.9; echo "Mute: no" ;;
esac
`
	if err := os.WriteFile(pactl, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}
	sink := audio.NewPulseSink(filepath.Join(dir, "pacat"), "")

	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()
	state, err := sink.Volume(ctx)
	if err != nil {
		t.Fatalf("Volume: %v (the two pactl calls did not overlap)", err)
	}
	if state.Percent != 65 || state.Muted {
		t.Fatalf("state = %+v, want 65%% unmuted", state)
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
