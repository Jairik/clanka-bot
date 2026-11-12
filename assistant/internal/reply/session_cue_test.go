package reply_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// cueSink marks a Cue playback in the shared event log.
type cueSink struct{ log *eventLog }

func (s cueSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return cueWriter{log: s.log}, nil
}

type cueWriter struct{ log *eventLog }

func (w cueWriter) Write(p []byte) (int, error) {
	w.log.add("error-cue")
	return len(p), nil
}

func (w cueWriter) Close() error { return nil }

// speakingLog records a spoken phrase in the shared event log.
type speakingLog struct{ log *eventLog }

func (s speakingLog) SayText(ctx context.Context, text string) error {
	if hook := speech.FirstAudioHook(ctx); hook != nil {
		hook()
	}
	s.log.add("say:" + text)
	return nil
}

// fakeCues returns one canned clip and records the roles it was asked for.
type fakeCues struct {
	roles []sound.Role
	clip  sound.Clip
	ok    bool
}

func (f *fakeCues) Clip(role sound.Role) (sound.Clip, bool) {
	f.roles = append(f.roles, role)
	if !f.ok {
		return sound.Clip{}, false
	}
	return f.clip, true
}

func cueReply() *fakePipeline {
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{
			Text:        "Sorry, I can't answer that right now.",
			Path:        "agent",
			Unavailable: true,
		}
	}
	return routed
}

func TestUnavailableMessagePlaysTheErrorCueFirst(t *testing.T) {
	log := &eventLog{}
	cues := &fakeCues{
		clip: sound.Clip{PCM: []byte{1, 2, 3, 4}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}},
		ok:   true,
	}
	session := reply.New(cueReply(), speakingLog{log}, cueSink{log}, nil, reply.WithCues(cues))
	if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	want := []string{"error-cue", "say:Sorry, I can't answer that right now."}
	got := log.all()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("playback order = %v, want %v", got, want)
	}
	if len(cues.roles) != 1 || cues.roles[0] != sound.RoleError {
		t.Fatalf("cue roles = %v, want only the error role", cues.roles)
	}
}

func TestNoneErrorCueStaysSilentBeforeTheUnavailableMessage(t *testing.T) {
	log := &eventLog{}
	cues := &fakeCues{ok: false}
	session := reply.New(cueReply(), speakingLog{log}, cueSink{log}, nil, reply.WithCues(cues))
	if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	want := []string{"say:Sorry, I can't answer that right now."}
	if got := log.all(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("playback = %v, want %v", got, want)
	}
	if len(cues.roles) != 1 || cues.roles[0] != sound.RoleError {
		t.Fatalf("cue roles = %v", cues.roles)
	}
}

func TestNoCueSourceKeepsUnavailablePlaybackUnchanged(t *testing.T) {
	log := &eventLog{}
	session := reply.New(cueReply(), speakingLog{log}, cueSink{log}, nil)
	if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := log.all(); len(got) != 1 || got[0] != "say:Sorry, I can't answer that right now." {
		t.Fatalf("playback = %v", got)
	}
}

// sinkSpeaker writes one marker buffer through the same serial sink the Cue
// uses, so a fake pacat records the real playback order.
type sinkSpeaker struct {
	sink   speech.Sink
	format audio.Format
}

func (s sinkSpeaker) SayText(ctx context.Context, _ string) error {
	writer, err := s.sink.Open(ctx, s.format)
	if err != nil {
		return err
	}
	if _, err := writer.Write([]byte{0xAA, 0xAA, 0x55, 0x55}); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

// TestErrorCueReachesTheRealSinkProcessBeforeSpeech wires the real sound
// service, the real serial sink and a fake pacat: the WAV samples are written
// first, then the reply PCM, with no overlap. This is software evidence only;
// no Pulse server or speaker is involved.
func TestErrorCueReachesTheRealSinkProcessBeforeSpeech(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "pcm.raw")
	pacat := filepath.Join(dir, "pacat")
	script := "#!/bin/sh\ncat >> '" + pcmPath + "'\n"
	if err := os.WriteFile(pacat, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	soundDir := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(soundDir, "sounds"), 0o700); err != nil {
		t.Fatalf("create sounds: %v", err)
	}
	// A 400 ms mono PCM16 WAV; the test only needs its decoded length.
	rate := 16000
	frames := rate * 2 / 5
	raw := make([]byte, 44+frames*2)
	copy(raw[0:4], "RIFF")
	put := func(offset int, value uint32) {
		raw[offset] = byte(value)
		raw[offset+1] = byte(value >> 8)
		raw[offset+2] = byte(value >> 16)
		raw[offset+3] = byte(value >> 24)
	}
	put(4, uint32(36+frames*2))
	copy(raw[8:12], "WAVE")
	copy(raw[12:16], "fmt ")
	put(16, 16)
	raw[20], raw[21] = 1, 0
	raw[22] = 1
	put(24, uint32(rate))
	put(28, uint32(rate*2))
	raw[32] = 2
	raw[34] = 16
	copy(raw[36:40], "data")
	put(40, uint32(frames*2))
	for i := 0; i < frames; i++ {
		raw[44+i*2] = 0x34
		raw[44+i*2+1] = 0x12
	}
	if err := os.WriteFile(filepath.Join(soundDir, "sounds", "birds.wav"), raw, 0o600); err != nil {
		t.Fatalf("write birds.wav: %v", err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cues, err := sound.New(ctx, st, soundDir, audio.Format{Rate: rate, Width: 2, Channels: 1}, discardTestLogger())
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	sett, err := settings.New(ctx, st, cues.Definitions(sound.Config{})...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if _, err := sett.Set(ctx, sound.Key(sound.RoleError), "birds"); err != nil {
		t.Fatalf("Set error Cue: %v", err)
	}
	sink := speech.NewSerialSink(audio.NewPulseSink(pacat, ""))
	cues.Bind(sett, sink)

	speaker := sinkSpeaker{sink: sink, format: audio.Format{Rate: rate, Width: 2, Channels: 1}}
	session := reply.New(cueReply(), speaker, sink, nil, reply.WithCues(cues))
	if _, err := session.Ask(ctx, request(), "ask the agent"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	played, err := os.ReadFile(pcmPath)
	if err != nil {
		t.Fatalf("read captured PCM: %v", err)
	}
	cueBytes := frames * 2
	if len(played) != cueBytes+4 {
		t.Fatalf("captured %d bytes, want %d cue bytes plus 4 speech bytes", len(played), cueBytes)
	}
	if !bytes.Equal(played[cueBytes:], []byte{0xAA, 0xAA, 0x55, 0x55}) {
		t.Fatalf("speech marker = %v, want it after the cue", played[cueBytes:])
	}
	for i := 0; i < cueBytes; i += 2 {
		if got := int16(binary.LittleEndian.Uint16(played[i:])); got != 0x1234 {
			t.Fatalf("cue sample %d = %#x, want 0x1234", i/2, got)
		}
	}
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
