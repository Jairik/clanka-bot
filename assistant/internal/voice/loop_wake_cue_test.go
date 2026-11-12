package voice_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// capturePlayer records every Cue buffer the loop plays.
type capturePlayer struct {
	mu      sync.Mutex
	buffers [][]byte
	formats []audio.Format
}

func (p *capturePlayer) Play(_ context.Context, pcm []byte, format audio.Format) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buffers = append(p.buffers, append([]byte(nil), pcm...))
	p.formats = append(p.formats, format)
	return nil
}

func (p *capturePlayer) snapshot() ([][]byte, []audio.Format) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.buffers...), append([]audio.Format(nil), p.formats...)
}

func TestLoopUsesTheLiveWakeCueInsteadOfTheStaticChime(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{text: "hello"}
	player := &capturePlayer{}
	responder := &fakeResponder{calls: make(chan string, 1)}
	called := 0
	cue := []byte{1, 2, 3, 4}

	loop := newLoop(t, voice.Config{
		FrameBytes:  frameBytes,
		Endpoint:    endpointConfig(500 * time.Millisecond),
		Chime:       []byte{9, 9, 9, 9},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Player: player, Responder: responder,
		WakeCue: func() ([]byte, audio.Format, bool) {
			called++
			return cue, audio.Format{Rate: 16000, Width: 2, Channels: 1}, true
		},
	})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	deadline := time.Now().Add(time.Second)
	for {
		buffers, formats := player.snapshot()
		if len(buffers) > 0 {
			if called != 1 {
				t.Fatalf("WakeCue called %d times, want once per detection", called)
			}
			if !bytes.Equal(buffers[0], cue) {
				t.Fatalf("wake cue = %v, want the live cue %v", buffers[0], cue)
			}
			if formats[0].Rate != 16000 {
				t.Fatalf("wake cue format = %+v", formats[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no wake cue was played")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLoopTreatsNoneWakeCueAsSilence(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{text: "hello"}
	player := &capturePlayer{}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes:  frameBytes,
		Endpoint:    endpointConfig(500 * time.Millisecond),
		Chime:       []byte{9, 9},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Player: player, Responder: responder,
		WakeCue: func() ([]byte, audio.Format, bool) { return nil, audio.Format{}, false },
	})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	time.Sleep(100 * time.Millisecond)
	if buffers, _ := player.snapshot(); len(buffers) != 0 {
		t.Fatalf("none played %d wake cues", len(buffers))
	}
}

// TestWakeCueReachesTheRealSinkProcess wires the real library, live setting,
// raw sink and a fake pacat: the assigned volume WAV is the audio that plays on
// wake. No Pulse server or speaker is involved.
func TestWakeCueReachesTheRealSinkProcess(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "pcm.raw")
	pacat := filepath.Join(dir, "pacat")
	if err := os.WriteFile(pacat, []byte("#!/bin/sh\ncat >> '"+pcmPath+"'\n"), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	volume := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(volume, "sounds"), 0o700); err != nil {
		t.Fatalf("create sounds: %v", err)
	}
	const rate = 16000
	frames := rate / 5 // 200 ms
	raw := make([]byte, 44+frames*2)
	copy(raw[0:4], "RIFF")
	binary.LittleEndian.PutUint32(raw[4:8], uint32(36+frames*2))
	copy(raw[8:12], "WAVE")
	copy(raw[12:16], "fmt ")
	binary.LittleEndian.PutUint32(raw[16:20], 16)
	binary.LittleEndian.PutUint16(raw[20:22], 1)
	binary.LittleEndian.PutUint16(raw[22:24], 1)
	binary.LittleEndian.PutUint32(raw[24:28], rate)
	binary.LittleEndian.PutUint32(raw[28:32], rate*2)
	binary.LittleEndian.PutUint16(raw[32:34], 2)
	binary.LittleEndian.PutUint16(raw[34:36], 16)
	copy(raw[36:40], "data")
	binary.LittleEndian.PutUint32(raw[40:44], uint32(frames*2))
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(raw[44+i*2:], 0x2222)
	}
	if err := os.WriteFile(filepath.Join(volume, "sounds", "birds.wav"), raw, 0o600); err != nil {
		t.Fatalf("write birds.wav: %v", err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cues, err := sound.New(ctx, st, volume, audio.Format{Rate: rate, Width: 2, Channels: 1}, discardVoiceLogger())
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	sett, err := settings.New(ctx, st, cues.Definitions(sound.Config{})...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if _, err := sett.Set(ctx, sound.Key(sound.RoleWake), "birds"); err != nil {
		t.Fatalf("Set wake: %v", err)
	}
	cues.Bind(sett, nil)

	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{text: "hello"}
	responder := &fakeResponder{calls: make(chan string, 1)}
	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint:   endpointConfig(500 * time.Millisecond),
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt,
		Player: audio.NewPCMPlayer(audio.NewPulseSink(pacat, "")), Responder: responder,
		WakeCue: func() ([]byte, audio.Format, bool) {
			clip, ok := cues.Clip(sound.RoleWake)
			return clip.PCM, clip.Format, ok
		},
	})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		played, err := os.ReadFile(pcmPath)
		if err == nil && len(played) >= frames*2 {
			if len(played) != frames*2 {
				t.Fatalf("wake cue wrote %d bytes, want %d", len(played), frames*2)
			}
			for i := 0; i < len(played); i += 2 {
				if got := int16(binary.LittleEndian.Uint16(played[i:])); got != 0x2222 {
					t.Fatalf("wake cue sample %d = %#x, want 0x2222", i/2, got)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("wake cue did not reach the sink (read err %v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func discardVoiceLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
