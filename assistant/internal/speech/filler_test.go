package speech_test

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

func TestSynthesizeFillerCachesTheWholePhrase(t *testing.T) {
	pcmOne := bytes.Repeat([]byte{9, 8}, 6000)
	pcmTwo := bytes.Repeat([]byte{7, 6}, 6000)
	addr := serveOnce(t, func(conn net.Conn) {
		readLine(t, conn)
		writeEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, conn, pcmOne)
		writeAudio(t, conn, pcmTwo)
		writeEvent(t, conn, "audio-stop", nil)
	})

	filler, err := speech.SynthesizeFiller(context.Background(), wyoming.NewClient(addr), wyoming.Voice{Name: "alba"}, "One moment.")
	if err != nil {
		t.Fatalf("SynthesizeFiller: %v", err)
	}
	if filler.Text != "One moment." {
		t.Fatalf("text = %q", filler.Text)
	}
	if want := append(append([]byte(nil), pcmOne...), pcmTwo...); !bytes.Equal(filler.PCM, want) {
		t.Fatalf("cached %d bytes, want %d", len(filler.PCM), len(want))
	}
	if filler.Format != (audio.Format{Rate: 24000, Width: 2, Channels: 1}) {
		t.Fatalf("format = %+v", filler.Format)
	}
}

func TestFillerPlayWritesTheCachedPCMWithoutTTS(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 1000)
	addr := serveOnce(t, func(conn net.Conn) {
		readLine(t, conn)
		writeEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, conn, pcm)
		writeEvent(t, conn, "audio-stop", nil)
	})
	filler, err := speech.SynthesizeFiller(context.Background(), wyoming.NewClient(addr), wyoming.Voice{}, "One moment.")
	if err != nil {
		t.Fatalf("SynthesizeFiller: %v", err)
	}

	sink := newRecorder()
	if err := filler.Play(context.Background(), sink); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if got := sink.audio(); !bytes.Equal(got, pcm) {
		t.Fatalf("played %d bytes, want the cached %d", len(got), len(pcm))
	}
	if sink.opened != (audio.Format{Rate: 24000, Width: 2, Channels: 1}) {
		t.Fatalf("sink format = %+v", sink.opened)
	}
	if !sink.closed {
		t.Fatal("sink was not closed")
	}
}

func TestSynthesizeFillerRequiresText(t *testing.T) {
	if _, err := speech.SynthesizeFiller(context.Background(), wyoming.NewClient("127.0.0.1:1"), wyoming.Voice{}, "  "); err == nil {
		t.Fatal("SynthesizeFiller succeeded with empty text, want error")
	}
}
