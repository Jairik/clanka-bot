package quiet_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// synthServer answers every Wyoming synthesize request with the same PCM16
// frame, so a real speech.Speaker can be used without a TTS service.
func synthServer(t *testing.T, pcm []byte) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				reader := bufio.NewReader(conn)
				if _, err := reader.ReadString('\n'); err != nil {
					return
				}
				writeSynthEvent(conn, "audio-start", map[string]any{"rate": 16000, "width": 2, "channels": 1})
				writeSynthEvent(conn, "audio-chunk", pcm)
				writeSynthEvent(conn, "audio-stop", nil)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().String()
}

// writeSynthEvent writes one event; a byte slice is an audio payload.
func writeSynthEvent(w io.Writer, typ string, data any) {
	if pcm, ok := data.([]byte); ok {
		header, _ := json.Marshal(map[string]any{"type": typ, "payload_length": len(pcm)})
		_, _ = w.Write(append(header, '\n'))
		_, _ = w.Write(pcm)
		return
	}
	event, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	_, _ = w.Write(append(event, '\n'))
}

// signalSink records the PCM a real speech.Speaker or cue player writes.
type signalSink struct {
	mu  sync.Mutex
	pcm []byte
}

func (s *signalSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return &signalWriter{sink: s}, nil
}

type signalWriter struct{ sink *signalSink }

func (w *signalWriter) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	w.sink.pcm = append(w.sink.pcm, p...)
	w.sink.mu.Unlock()
	return len(p), nil
}

func (w *signalWriter) Close() error { return nil }

func (s *signalSink) signal() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.pcm...)
}

// signalPeak returns the largest absolute PCM16 sample.
func signalPeak(pcm []byte) int16 {
	var peak int16
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	return peak
}

// constantPCM builds a mono PCM16 buffer of one repeated sample.
func constantPCM(sample int16, count int) []byte {
	out := make([]byte, count*2)
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(sample))
	}
	return out
}

// TestRingPCMFollowsQuietVolumeAndExemptsAlarms runs the real Ring engine with
// the production Quiet seams: the Timer cue and label are scaled on the actual
// PCM, the Alarm keeps both at its normal volume, the shared clip is not
// mutated and a second Timer ring is not scaled twice.
func TestRingPCMFollowsQuietVolumeAndExemptsAlarms(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "00:00", End: "23:59", Volume: 25}, utc(2026, 11, 12, 12, 0), time.UTC)
	ttsPCM := constantPCM(10000, 8)
	addr := synthServer(t, ttsPCM)
	cue := sound.Clip{PCM: constantPCM(10000, 8), Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}}

	runRing := func(kind ring.Kind, label string) (int16, int16) {
		t.Helper()
		speakerSink := &signalSink{}
		speaker := speech.New(wyoming.NewClient(addr), speakerSink, wyoming.Voice{}, nil)
		played := make(chan []byte, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		engine := ring.New(ring.Deps{
			Speaker:    speaker,
			SpeechGain: fixture.service.RingSpeechGain,
			Settings: func(kind ring.Kind) ring.Settings {
				normal := ring.Settings{MaxRing: 5 * time.Second, Volume: 80, SpeakLabel: ring.SpeakStart}
				return fixture.service.RingSettings(kind, normal)
			},
			Clip: func(ring.Kind) (sound.Clip, bool) { return cue, true },
			Play: func(_ context.Context, clip sound.Clip) error {
				played <- append([]byte(nil), clip.PCM...)
				// One pass is enough; cancelling ends the Ring after this play.
				cancel()
				return nil
			},
		})
		engine.Ring(ctx, ring.Request{Kind: kind, ID: string(kind), Label: label})
		select {
		case pcm := <-played:
			return signalPeak(speakerSink.signal()), signalPeak(pcm)
		default:
			t.Fatalf("%s Ring did not play its cue", kind)
			return 0, 0
		}
	}

	// The Timer's actual label and cue PCM use quiet.volume (25% of 10000).
	label, played := runRing(ring.Timer, "pasta")
	if label != 2500 || played != 2500 {
		t.Fatalf("Timer label/cue peak = %d/%d, want 2500/2500", label, played)
	}
	if peak := signalPeak(cue.PCM); peak != 10000 {
		t.Fatalf("shared cue was mutated to %d, want 10000", peak)
	}
	// A second Timer ring starts from the shared clip again, not the scaled copy.
	label, played = runRing(ring.Timer, "pasta")
	if label != 2500 || played != 2500 {
		t.Fatalf("second Timer label/cue peak = %d/%d, want 2500/2500", label, played)
	}
	// The Alarm is exempt: its label stays at normal speech and its cue uses
	// the configured ring.timer-style normal volume (80), not quiet.volume.
	label, played = runRing(ring.Alarm, "wake")
	if label != 10000 || played != 8000 {
		t.Fatalf("Alarm label/cue peak = %d/%d, want 10000/8000", label, played)
	}
	if peak := signalPeak(cue.PCM); peak != 10000 {
		t.Fatalf("shared cue changed after Alarm ring: %d", peak)
	}
}

// quietPipeline answers every request with one fixed reply.
type quietPipeline struct {
	reply pipeline.Reply
}

func (p *quietPipeline) HandleEvents(context.Context, tool.Request, string, pipeline.Events) pipeline.Reply {
	return p.reply
}

// quietCues serves one fixed error Cue.
type quietCues struct {
	clip sound.Clip
}

func (c *quietCues) Clip(sound.Role) (sound.Clip, bool) { return c.clip, true }

// TestReplyAndUnavailablePCMFollowQuietVolume runs the real reply session with
// the production Quiet volume and Cue scale seams, so the reply text, the
// Unavailable message and the error Cue are measured as actual PCM and a
// second turn after the window ends is not compounded or left scaled.
func TestReplyAndUnavailablePCMFollowQuietVolume(t *testing.T) {
	fixture := newFixture(t, quiet.Config{Enabled: true, Start: "00:00", End: "23:59", Volume: 25}, utc(2026, 11, 12, 12, 0), time.UTC)
	ttsPCM := constantPCM(10000, 8)
	addr := synthServer(t, ttsPCM)
	cue := sound.Clip{PCM: constantPCM(10000, 8), Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}}

	ask := func(p pipeline.Reply) (int16, int16) {
		t.Helper()
		speakerSink := &signalSink{}
		cueSink := &signalSink{}
		speaker := speech.New(wyoming.NewClient(addr), speakerSink, wyoming.Voice{}, nil)
		cues := &quietCues{clip: cue}
		session := reply.New(&quietPipeline{reply: p}, speaker, cueSink, nil,
			reply.WithCues(cues),
			reply.WithCueScale(fixture.service.Clip),
			reply.WithVolumeSource(fixture.service.Gain),
		)
		if _, err := session.Ask(context.Background(), tool.Request{ID: "test", Device: tool.DevicePi}, "hello"); err != nil {
			t.Fatalf("Ask: %v", err)
		}
		return signalPeak(speakerSink.signal()), signalPeak(cueSink.signal())
	}

	// A normal reply is quiet; the error Cue is quiet for an Unavailable reply.
	speechPeak, cuePeak := ask(pipeline.Reply{Text: "Here is your answer.", Path: "agent", Tool: "test"})
	if speechPeak != 2500 || cuePeak != 0 {
		t.Fatalf("normal reply speech/cue peak = %d/%d, want 2500 and no cue", speechPeak, cuePeak)
	}
	speechPeak, cuePeak = ask(pipeline.Reply{Text: "Sorry, I can't answer that right now.", Path: "agent", Unavailable: true})
	if speechPeak != 2500 || cuePeak != 2500 {
		t.Fatalf("unavailable speech/cue peak = %d/%d, want 2500/2500", speechPeak, cuePeak)
	}
	if peak := signalPeak(cue.PCM); peak != 10000 {
		t.Fatalf("shared error cue was mutated to %d, want 10000", peak)
	}
	// Turning the window off applies on the next turn, with no compounded gain.
	if err := fixture.service.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	speechPeak, cuePeak = ask(pipeline.Reply{Text: "Sorry, I can't answer that right now.", Path: "agent", Unavailable: true})
	if speechPeak != 10000 || cuePeak != 10000 {
		t.Fatalf("normal-volume speech/cue peak = %d/%d, want 10000/10000", speechPeak, cuePeak)
	}

	// The Filler shares the same speech gain wrapper and cached PCM stays whole.
	filler := &speech.Filler{
		Text:   "One moment.",
		Format: audio.Format{Rate: 16000, Width: 2, Channels: 1},
		PCM:    constantPCM(10000, 8),
	}
	fillerSink := &signalSink{}
	if err := filler.Play(speech.WithGain(context.Background(), 25), fillerSink); err != nil {
		t.Fatalf("Filler.Play: %v", err)
	}
	if peak := signalPeak(fillerSink.signal()); peak != 2500 {
		t.Fatalf("Filler peak = %d, want 2500", peak)
	}
	if peak := signalPeak(filler.PCM); peak != 10000 {
		t.Fatalf("cached Filler PCM was mutated to %d, want 10000", peak)
	}
}
