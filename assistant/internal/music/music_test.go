package music_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/music"
)

type fakeStreams struct {
	mu   sync.Mutex
	sets []int
	err  error
}

func (f *fakeStreams) SetPercent(_ context.Context, p int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, p)
	return f.err
}

func (f *fakeStreams) got() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.sets...)
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDuckLowersThenRestoresOnce(t *testing.T) {
	f := &fakeStreams{}
	d := music.NewDucker(f, 20, nil)
	restore, err := d.Duck(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	restore()
	restore() // idempotent
	if got := f.got(); !equal(got, []int{20, 100}) {
		t.Fatalf("sets = %v, want duck 20 then restore 100", got)
	}
}

func TestOverlappingDucksRestoreOnlyWhenTheLastReleases(t *testing.T) {
	f := &fakeStreams{}
	d := music.NewDucker(f, 20, nil)
	reply, _ := d.Duck(t.Context())
	ring, _ := d.Duck(t.Context()) // a Ring during a reply
	reply()
	if got := f.got(); !equal(got, []int{20}) {
		t.Fatalf("after the reply ends, sets = %v: music must stay ducked for the Ring", got)
	}
	ring()
	if got := f.got(); !equal(got, []int{20, 100}) {
		t.Fatalf("sets = %v, want restore after the last holder", got)
	}
}

func TestRestoreRunsEvenWhenTheCallerContextIsCancelled(t *testing.T) {
	f := &fakeStreams{}
	d := music.NewDucker(f, 20, nil)
	ctx, cancel := context.WithCancel(t.Context())
	restore, _ := d.Duck(ctx)
	cancel()
	restore()
	if got := f.got(); !equal(got, []int{20, 100}) {
		t.Fatalf("sets = %v", got)
	}
}

func TestDuckErrorStillReturnsARestore(t *testing.T) {
	f := &fakeStreams{err: errors.New("pulse down")}
	d := music.NewDucker(f, 20, nil)
	restore, err := d.Duck(t.Context())
	if err == nil || restore == nil {
		t.Fatalf("Duck = restore %v, err %v; want an error and a usable restore", restore != nil, err)
	}
	restore()
	if got := f.got(); !equal(got, []int{20, 100}) {
		t.Fatalf("restore must still be attempted, sets = %v", got)
	}
}

func TestNilDuckerIsANoOp(t *testing.T) {
	var d *music.Ducker
	restore, err := d.Duck(t.Context())
	if err != nil || restore == nil {
		t.Fatalf("nil Duck = %v", err)
	}
	restore()
}

// fakePactl lists two sink inputs (librespot and the assistant's reply) and
// records every set-sink-input-volume call.
func fakePactl(t *testing.T) (pacat, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.txt")
	script := `#!/bin/sh
if [ "$1" = "--format=json" ]; then
cat <<'JSON'
[{"index":7,"properties":{"application.name":"librespot","media.name":"Radiohead"}},
 {"index":9,"properties":{"application.name":"clanka-assistant","media.name":"assistant-reply"}}]
JSON
exit 0
fi
echo "$@" >> '` + log + `'
`
	if err := os.WriteFile(filepath.Join(dir, "pactl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "pacat"), log
}

func TestPulseStreamsSetsOnlyTheLibrespotInput(t *testing.T) {
	pacat, log := fakePactl(t)
	streams := music.NewPulseStreams(pacat, "")
	if err := streams.SetPercent(t.Context(), 25); err != nil {
		t.Fatalf("SetPercent: %v", err)
	}
	raw, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(raw)); got != "set-sink-input-volume 7 25%" {
		t.Fatalf("pactl calls = %q, want only the librespot input 7", got)
	}
}

func TestPulseStreamsWithNoMusicIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho '[]'\n"
	if err := os.WriteFile(filepath.Join(dir, "pactl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	streams := music.NewPulseStreams(filepath.Join(dir, "pacat"), "")
	if err := streams.SetPercent(t.Context(), 25); err != nil {
		t.Fatalf("SetPercent with no streams: %v", err)
	}
}
