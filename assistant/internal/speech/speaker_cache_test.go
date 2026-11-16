package speech_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// testSink is the Sink surface the tests need; Sink itself is unexported.
type testSink interface {
	Open(ctx context.Context, format audio.Format) (io.WriteCloser, error)
}

// serveTTS starts a Wyoming-style fake TTS endpoint that handles every
// connection with handle, and reports how many synthesis connections it
// received. Each Say miss opens exactly one connection, so the count is the
// number of TTS requests made.
func serveTTS(t *testing.T, handle func(net.Conn)) (string, func() int32) {
	t.Helper()
	var count int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&count, 1)
			go func(c net.Conn) {
				defer c.Close()
				handle(c)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() int32 { return atomic.LoadInt32(&count) }
}

// okHandler streams one fixed PCM buffer for every synthesis.
func okHandler(t *testing.T, pcm []byte) func(net.Conn) {
	return func(c net.Conn) {
		readLine(t, c)
		writeEvent(t, c, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, c, pcm)
		writeEvent(t, c, "audio-stop", nil)
	}
}

// swapSink lets one Speaker write to a different underlying testSink per Say, so
// a test can capture audio from both a miss and a later hit (which share a
// cache) without constructing two Speakers.
type swapSink struct {
	inner testSink
}

func (s *swapSink) Swap(inner testSink) { s.inner = inner }

func (s *swapSink) Open(ctx context.Context, format audio.Format) (io.WriteCloser, error) {
	return s.inner.Open(ctx, format)
}

// cancelSink cancels ctx on its first Write, simulating a Barge-in that aborts a
// reply mid-playback.
type cancelSink struct {
	mu     sync.Mutex
	writes int
	cancel context.CancelFunc
}

func (s *cancelSink) Open(context.Context, audio.Format) (io.WriteCloser, error) { return s, nil }
func (s *cancelSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	return len(p), nil
}
func (s *cancelSink) Close() error { return nil }
func (s *cancelSink) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func TestTTSCacheHitServesWithoutTTSAndRunsHookAndMarks(t *testing.T) {
	audioData := bytes.Repeat([]byte{1, 2}, 480)
	addr, requests := serveTTS(t, okHandler(t, audioData))

	// The hit must still run the sound-slot hook and the latency marks.
	order := &orderSink{}
	hookCtx := speech.WithFirstAudio(context.Background(), func() {
		order.mu.Lock()
		order.order = append(order.order, "hook")
		order.mu.Unlock()
	})
	timeline := latency.New("req-hit", latency.SourceVoice)
	hookCtx = latency.WithContext(hookCtx, timeline)

	swap := &swapSink{}
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	// First Say: miss (synthesises and warms the cache).
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("first Say: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("first Say requests = %d, want 1", got)
	}

	// Second identical Say: hit, zero new TTS requests.
	swap.Swap(order)
	if _, err := sp.Say(hookCtx, "Set."); err != nil {
		t.Fatalf("hit Say: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("hit Say made %d TTS requests, want 0", got-1)
	}
	got := order.events()
	if len(got) < 2 || got[0] != "hook" || got[1] != "write" {
		t.Fatalf("hit sound-slot order = %v, want [hook write ...]", got)
	}
	if _, ok := timeline.Duration(latency.StageTTSFirstAudio); !ok {
		t.Fatal("tts_first_audio stage not recorded on hit")
	}
	if !timeline.HasReplyAudio() {
		t.Fatal("reply audio not marked on hit")
	}
}

func TestTTSCacheBypassesLongText(t *testing.T) {
	chunk := bytes.Repeat([]byte{1, 2}, 20000) // 40 KB chunk
	addr, requests := serveTTS(t, func(c net.Conn) {
		readLine(t, c)
		writeEvent(t, c, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		// A long reply split into several chunks.
		for i := 0; i < 5; i++ {
			writeAudio(t, c, chunk)
		}
		writeEvent(t, c, "audio-stop", nil)
	})
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	long := strings.Repeat("a", 200) // 200 runes, over the 80-rune cap.
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), long); err != nil {
		t.Fatalf("first long Say: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("first long Say requests = %d, want 1", got)
	}
	// A long reply never enters the cache, so an identical second Say still
	// misses and makes another TTS request (no PCM was buffered).
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), long); err != nil {
		t.Fatalf("second long Say: %v", err)
	}
	if got := requests(); got != 2 {
		t.Fatalf("second long Say requests = %d, want 2 (cache bypass)", got)
	}
}

func TestTTSCacheMissesOnVoiceRateOrTextChange(t *testing.T) {
	audioData := bytes.Repeat([]byte{1, 2}, 480)
	addr, requests := serveTTS(t, okHandler(t, audioData))

	var voice wyoming.Voice
	voice.Name = "alba"
	sp := speech.New(wyoming.NewClient(addr), newRecorder(), wyoming.Voice{Name: "alba"}, nil)
	sp.BindVoice(func() speech.VoiceSettings { return speech.VoiceSettings{Voice: voice, Rate: 1.0} })

	// Baseline: miss then hit.
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("baseline miss: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("baseline miss requests = %d, want 1", got)
	}
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("baseline hit: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("baseline hit made a request: %d", got)
	}

	// A voice change misses.
	voice.Name = "other"
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("voice change Say: %v", err)
	}
	if got := requests(); got != 2 {
		t.Fatalf("voice change should miss: requests = %d, want 2", got)
	}
	// Reverting the voice hits again (no new request).
	voice.Name = "alba"
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("revert voice Say: %v", err)
	}
	if got := requests(); got != 2 {
		t.Fatalf("revert voice should hit: requests = %d, want 2", got)
	}

	// A per-utterance rate override change misses.
	if _, err := sp.Say(speech.WithRate(context.Background(), 1.5), "Set."); err != nil {
		t.Fatalf("rate change Say: %v", err)
	}
	if got := requests(); got != 3 {
		t.Fatalf("rate change should miss: requests = %d, want 3", got)
	}
	if _, err := sp.Say(speech.WithRate(context.Background(), 1.5), "Set."); err != nil {
		t.Fatalf("same rate Say: %v", err)
	}
	if got := requests(); got != 3 {
		t.Fatalf("same rate override should hit: requests = %d, want 3", got)
	}

	// A pronunciation / spoken-text change misses. The cache key is the
	// post-pronunciation text, so a pronunciation map (#5) rewriting the text
	// misses naturally instead of replaying old audio.
	if _, err := sp.Say(context.Background(), "Set!"); err != nil {
		t.Fatalf("text change Say: %v", err)
	}
	if got := requests(); got != 4 {
		t.Fatalf("text change should miss: requests = %d, want 4", got)
	}
}

func TestTTSCacheEvictsByEntryCap(t *testing.T) {
	audioData := bytes.Repeat([]byte{1, 2}, 480)
	addr, requests := serveTTS(t, okHandler(t, audioData))
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	// 70 distinct phrases; the 64-entry cap evicts the oldest.
	const total = 70
	for i := 0; i < total; i++ {
		swap.Swap(newRecorder())
		if _, err := sp.Say(context.Background(), fmt.Sprintf("ack %d", i)); err != nil {
			t.Fatalf("Say %d: %v", i, err)
		}
	}

	// The oldest phrase is evicted -> a fresh miss.
	swap.Swap(newRecorder())
	before := requests()
	if _, err := sp.Say(context.Background(), "ack 0"); err != nil {
		t.Fatalf("oldest re-Say: %v", err)
	}
	if got := requests(); got != before+1 {
		t.Fatalf("oldest should be evicted (miss): requests %d want %d", got, before+1)
	}

	// The most recent phrase is present -> a hit.
	swap.Swap(newRecorder())
	before = requests()
	if _, err := sp.Say(context.Background(), fmt.Sprintf("ack %d", total-1)); err != nil {
		t.Fatalf("newest re-Say: %v", err)
	}
	if got := requests(); got != before {
		t.Fatalf("newest should be a hit: requests %d want %d", got, before)
	}
}

func TestTTSCacheEvictsByByteCap(t *testing.T) {
	// ~20 KB per phrase; 60 phrases = 1.2 MiB, over the 1 MiB cap, so the byte
	// cap (not the entry cap) evicts the oldest.
	audioData := bytes.Repeat([]byte{1, 2}, 10240)
	addr, requests := serveTTS(t, okHandler(t, audioData))
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	const total = 60
	for i := 0; i < total; i++ {
		swap.Swap(newRecorder())
		if _, err := sp.Say(context.Background(), fmt.Sprintf("line %d", i)); err != nil {
			t.Fatalf("Say %d: %v", i, err)
		}
	}

	// The oldest phrase is evicted by the byte cap -> a miss.
	swap.Swap(newRecorder())
	before := requests()
	if _, err := sp.Say(context.Background(), "line 0"); err != nil {
		t.Fatalf("oldest re-Say: %v", err)
	}
	if got := requests(); got != before+1 {
		t.Fatalf("oldest should be evicted (miss): requests %d want %d", got, before+1)
	}

	// The most recent phrase is present -> a hit.
	swap.Swap(newRecorder())
	before = requests()
	if _, err := sp.Say(context.Background(), fmt.Sprintf("line %d", total-1)); err != nil {
		t.Fatalf("newest re-Say: %v", err)
	}
	if got := requests(); got != before {
		t.Fatalf("newest should be a hit: requests %d want %d", got, before)
	}
}

func TestTTSCacheDoesNotStoreOnMidStreamError(t *testing.T) {
	addr, requests := serveTTS(t, func(c net.Conn) {
		readLine(t, c)
		writeEvent(t, c, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeEvent(t, c, "error", map[string]any{"text": "boom"})
	})
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	// Mid-stream error returns the error and stores nothing.
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err == nil {
		t.Fatal("want error from mid-stream failure")
	}
	if got := requests(); got != 1 {
		t.Fatalf("first requests = %d, want 1", got)
	}
	// A second identical Say is still a miss: nothing was cached.
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err == nil {
		t.Fatal("want error from mid-stream failure (still not cached)")
	}
	if got := requests(); got != 2 {
		t.Fatalf("second requests = %d, want 2 (nothing cached)", got)
	}
}

func TestTTSCacheHitCancelledByBargeIn(t *testing.T) {
	audioData := bytes.Repeat([]byte{1, 2}, 480)
	addr, requests := serveTTS(t, okHandler(t, audioData))
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	// Warm the cache with a clean miss.
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("warmup Say: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("warmup requests = %d, want 1", got)
	}

	// A hit whose context is cancelled on the first write (Barge-in) must stop
	// promptly, write no further PCM, and return the interruption.
	sink := &cancelSink{}
	ctx, cancel := context.WithCancel(context.Background())
	sink.cancel = cancel
	swap.Swap(sink)
	if _, err := sp.Say(ctx, "Set."); err == nil {
		t.Fatal("cancelled hit should return an error")
	}
	if ctx.Err() == nil {
		t.Fatal("context should be cancelled")
	}
	if wc := sink.writeCount(); wc != 1 {
		t.Fatalf("hit wrote %d times, want 1 (no further PCM after cancel)", wc)
	}

	// The cache is intact: a subsequent clean hit makes no new request.
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("post-cancel hit: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("post-cancel hit made %d requests, want 0 new", got-1)
	}
}

func TestTTSCacheQuietHoursGainIdenticalOnHitAndMiss(t *testing.T) {
	audioData := bytes.Repeat([]byte{3, 4}, 480)
	addr, _ := serveTTS(t, okHandler(t, audioData))
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, nil)

	// Miss at Quiet-hours gain 50.
	miss := newRecorder()
	swap.Swap(miss)
	if _, err := sp.Say(speech.WithGain(context.Background(), 50), "Done."); err != nil {
		t.Fatalf("miss Say: %v", err)
	}
	audioMiss := miss.audio()

	// Hit at the same gain 50: identical audio (gain applied identically).
	hit := newRecorder()
	swap.Swap(hit)
	if _, err := sp.Say(speech.WithGain(context.Background(), 50), "Done."); err != nil {
		t.Fatalf("hit Say: %v", err)
	}
	audioHit := hit.audio()
	if !bytes.Equal(audioMiss, audioHit) {
		t.Fatalf("hit and miss audio differ at gain 50: %d vs %d bytes", len(audioMiss), len(audioHit))
	}

	// Hit at gain 100 (no software gain) must differ, proving gain still
	// applies on a hit from the pre-gain stored PCM.
	full := newRecorder()
	swap.Swap(full)
	if _, err := sp.Say(speech.WithGain(context.Background(), 100), "Done."); err != nil {
		t.Fatalf("full-gain hit Say: %v", err)
	}
	audioFull := full.audio()
	if bytes.Equal(audioFull, audioHit) {
		t.Fatal("gain 100 hit matched gain 50 hit: gain not applied on hit")
	}
}

func TestSpeechLogIncludesTTSCacheField(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	audioData := bytes.Repeat([]byte{1, 2}, 480)
	addr, _ := serveTTS(t, okHandler(t, audioData))
	swap := &swapSink{}
	swap.Swap(newRecorder())
	sp := speech.New(wyoming.NewClient(addr), swap, wyoming.Voice{Name: "alba"}, logger)

	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("miss Say: %v", err)
	}
	swap.Swap(newRecorder())
	if _, err := sp.Say(context.Background(), "Set."); err != nil {
		t.Fatalf("hit Say: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, `"tts_cache":"miss"`) {
		t.Fatalf("speech log missing tts_cache=miss:\n%s", out)
	}
	if !strings.Contains(out, `"tts_cache":"hit"`) {
		t.Fatalf("speech log missing tts_cache=hit:\n%s", out)
	}
}
