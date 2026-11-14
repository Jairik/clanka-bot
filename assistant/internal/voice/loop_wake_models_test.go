package voice_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
	"github.com/Jairik/clanka-bot/assistant/internal/voicecmd"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// waitFor runs check until it reports true or the deadline expires.
func waitFor(t *testing.T, deadline time.Duration, check func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached before the deadline")
}

func TestLoopIgnoresAWakeModelOutsideTheAllowlist(t *testing.T) {
	source := &scriptSource{frames: repeat(silentFrame(), 100)}
	wake := &fakeWake{afterWords: 3, name: "hey_jarvis", maxFires: 1}
	stt := &fakeSTT{text: "should not happen"}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: source, Wake: wake, STT: stt, Responder: responder, WakeWords: []string{"clanker"}})
	cancel, stopped := runLoop(t, loop)

	waitFor(t, 2*time.Second, func() bool {
		_, _, fires := wake.snapshot()
		return fires == 1
	})
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop")
	}

	if opens := stt.openCount(); opens != 0 {
		t.Fatalf("STT opened %d streams for an unlisted wake model", opens)
	}
	if wakes := responder.wakeWords(); len(wakes) != 0 {
		t.Fatalf("responder saw %v for an unlisted wake model", wakes)
	}
}

func TestLoopAcceptsEveryConfiguredWakeModel(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 400)...)}
	wake := &fakeWake{afterWords: 4, name: "okay_nabu", maxFires: 1}
	stt := &fakeSTT{text: "what time is it"}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: source, Wake: wake, STT: stt, Responder: responder, WakeWords: []string{"clanker", "okay_nabu"}})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the second configured wake model did not start a turn")
	}
	if got := responder.wakeWords(); len(got) != 1 || got[0] != "okay_nabu" {
		t.Fatalf("wake words = %v, want [okay_nabu]", got)
	}
}

func TestLoopDispatchesACommandDetectionWithoutStartingATurn(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 400)...)}
	wake := &fakeWake{afterWords: 3, names: []string{"volume_up", "clanker"}, maxFires: 2}
	stt := &fakeSTT{text: "what time is it"}
	responder := &fakeResponder{calls: make(chan string, 1)}
	var commands atomic.Int32

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: stt, Responder: responder,
			WakeWords: []string{"clanker"},
			WakeCommands: map[string]voice.WakeCommand{
				"volume_up": func(_ context.Context, name string) {
					if name != "volume_up" {
						t.Errorf("command name = %q", name)
					}
					commands.Add(1)
				},
			},
		})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the wake word after the command did not start a turn")
	}
	if got := commands.Load(); got != 1 {
		t.Fatalf("command calls = %d, want 1", got)
	}
	// The command never opened STT; only the following clanker turn did.
	if opens := stt.openCount(); opens != 1 {
		t.Fatalf("STT opened %d streams, want only the wake turn", opens)
	}
	if got, want := responder.wakeWords(), []string{"clanker"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("wake words = %v, want %v", got, want)
	}
}

func TestLoopDispatchesACommandDuringAReplyWithoutBargingIn(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 30), repeat(silentFrame(), 2000)...)}
	wake := &fakeWake{afterWords: 2, names: []string{"clanker", "volume_up"}, maxFires: 2}
	stt := &fakeSTT{text: "what time is it"}
	responder := &fakeResponder{calls: make(chan string, 1), block: 300 * time.Millisecond}
	var commands atomic.Int32

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint: voice.EndpointConfig{
			Rate:          16000,
			Silence:       100 * time.Millisecond,
			ThresholdDBFS: -38,
			MaxUtterance:  400 * time.Millisecond,
		},
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Responder: responder,
		WakeWords: []string{"clanker"},
		WakeCommands: map[string]voice.WakeCommand{
			"volume_up": func(context.Context, string) { commands.Add(1) },
		},
	})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply did not finish")
	}
	if got := commands.Load(); got != 1 {
		t.Fatalf("command calls during the reply = %d, want 1", got)
	}
	if responder.sawCancel() {
		t.Fatal("a command detection cancelled the reply")
	}
	if opens := stt.openCount(); opens != 1 {
		t.Fatalf("STT opened %d streams, want 1", opens)
	}
}

// bargeWakeResponder holds the first reply until its context is cancelled and
// records the wake model of every turn, so the test can prove the barge-in
// model starts the next turn.
type bargeWakeResponder struct {
	mu       sync.Mutex
	started  int
	wakes    []string
	speaking chan struct{}
	finished chan struct{}
	once     sync.Once
}

func newBargeWakeResponder() *bargeWakeResponder {
	return &bargeWakeResponder{
		speaking: make(chan struct{}),
		finished: make(chan struct{}, 1),
	}
}

func (r *bargeWakeResponder) Respond(ctx context.Context, req voice.Request) error {
	r.mu.Lock()
	r.started++
	r.wakes = append(r.wakes, req.WakeWord)
	first := r.started == 1
	r.mu.Unlock()
	r.once.Do(func() { close(r.speaking) })
	if first {
		<-ctx.Done()
		return ctx.Err()
	}
	r.finished <- struct{}{}
	return nil
}

func (r *bargeWakeResponder) wakeWords() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.wakes...)
}

func TestLoopCarriesTheBargeInModelIntoTheNextTurn(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 2000)}
	wake := &fakeWake{afterWords: 2, names: []string{"clanker", "okay_nabu"}, maxFires: 2}
	stt := &fakeSTT{text: "one"}
	responder := newBargeWakeResponder()

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		// Loud frames never trigger the silence endpointer, so the utterance
		// cap ends each question and the test moves on to the watch stream.
		Endpoint: voice.EndpointConfig{Rate: 16000, Silence: 100 * time.Millisecond, ThresholdDBFS: -38, MaxUtterance: 200 * time.Millisecond},
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Responder: responder,
		WakeWords: []string{"clanker", "okay_nabu"},
	})
	runLoop(t, loop)

	select {
	case <-responder.speaking:
	case <-time.After(5 * time.Second):
		t.Fatal("the first reply never started")
	}
	select {
	case <-responder.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not start a fresh turn after the barge-in")
	}
	got := responder.wakeWords()
	want := []string{"clanker", "okay_nabu"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("wake words = %v, want %v (the barge-in model starts the next turn)", got, want)
	}
}

// fakeWakeServer answers one detect stream with a fixed detection name.
func fakeWakeServer(t *testing.T, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for range 2 { // detect, audio-start
			if _, err := r.ReadBytes('\n'); err != nil {
				return
			}
		}
		line, err := r.ReadBytes('\n') // audio-chunk header
		if err != nil {
			return
		}
		var header struct {
			PayloadLength int `json:"payload_length"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return
		}
		if _, err := io.ReadFull(r, make([]byte, header.PayloadLength)); err != nil {
			return
		}
		_, _ = fmt.Fprintf(conn, `{"type":"detection","data":{"name":%q,"timestamp":0}}`+"\n", name)
	}()
	return ln.Addr().String()
}

func TestWyomingWakeDropsADetectionOutsideTheAllowlist(t *testing.T) {
	addr := fakeWakeServer(t, "hey_jarvis")
	wake := voice.NewWyomingWake(wyoming.NewClient(addr), []string{"clanker"},
		wyoming.Format{Rate: 16000, Width: 2, Channels: 1},
		map[string]float64{"clanker": 0.5})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := wake.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	if err := stream.Write(make([]byte, 1024)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case detection, ok := <-stream.Detections():
		if ok {
			t.Fatalf("detection %+v outside the allowlist was delivered", detection)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detections did not close")
	}
}

// commandSink is the volume seam for a real voicecmd.Handler in a loop test.
type commandSink struct {
	mu      sync.Mutex
	percent int
	sets    []int
}

func (s *commandSink) Volume(context.Context) (audio.VolumeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return audio.VolumeState{Percent: s.percent}, nil
}

func (s *commandSink) SetVolume(_ context.Context, percent int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.percent = percent
	s.sets = append(s.sets, percent)
	return nil
}

func (s *commandSink) writes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.sets...)
}

func TestLoopRunsARealVolumeCommandWithoutSTTOrRouting(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 400)...)}
	wake := &fakeWake{afterWords: 3, names: []string{"volume_up", "clanker"}, maxFires: 2}
	stt := &fakeSTT{text: "what time is it"}
	responder := &fakeResponder{calls: make(chan string, 1)}
	sink := &commandSink{percent: 40}

	handler, err := voicecmd.New(
		voicecmd.Config{Step: 10, Cooldown: time.Second},
		voicecmd.Deps{
			Sink:  sink,
			Focus: func() (bool, bool) { return true, false },
			Cue:   func(context.Context) {},
		},
	)
	if err != nil {
		t.Fatalf("voicecmd.New: %v", err)
	}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: stt, Responder: responder,
			WakeWords:    []string{"clanker"},
			WakeCommands: map[string]voice.WakeCommand{"volume_up": handler.Handle},
		})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the following wake word did not start a turn")
	}
	if got := sink.writes(); len(got) != 1 || got[0] != 50 {
		t.Fatalf("volume writes = %v, want one step to 50", got)
	}
	// The command opened no STT stream and routed nothing; only the clanker
	// turn after it did.
	if opens := stt.openCount(); opens != 1 {
		t.Fatalf("STT opened %d streams, want only the wake turn", opens)
	}
	if got := responder.wakeWords(); len(got) != 1 || got[0] != "clanker" {
		t.Fatalf("wake words = %v, want [clanker]", got)
	}
}
