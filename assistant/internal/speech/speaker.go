// Package speech turns reply text into audio: it asks the Wyoming TTS service
// for PCM and streams each chunk to the speaker as it arrives.
package speech

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// tts cache bounds. Short replies dominate Fast path turns, so caching their
// fully synthesized PCM avoids re-paying TTS latency and CPU on the Pi.
const (
	// ttsCacheMaxRunes is the longest spoken text eligible for the PCM cache.
	// Phrasebook acknowledgements stay well under this; longer replies are not
	// worth the memory on the Pi, so they bypass the cache entirely.
	ttsCacheMaxRunes = 80
	// ttsCacheMaxEntries bounds the number of cached phrases.
	ttsCacheMaxEntries = 64
	// ttsCacheMaxBytes bounds total cached PCM (about 32 KB per second of
	// audio). The 1 MiB cap is deliberate on the Pi.
	ttsCacheMaxBytes = 1 << 20
	// ttsCacheChunk is the write granularity when replaying cached PCM.
	ttsCacheChunk = 4096
)

// Sink plays a PCM stream. Production uses audio.PulseSink; tests use a fake.
// Reply playback, Fillers and alerts should share a SerialSink so two sounds
// never play over each other.
type Sink interface {
	Open(ctx context.Context, format audio.Format) (io.WriteCloser, error)
}

// Result reports what was played, for the request log.
type Result struct {
	// FirstAudio is when the first PCM chunk was accepted by the sink, not when
	// the DAC emitted it. pacat and the host audio stack add buffer latency
	// that this interface cannot observe.
	FirstAudio time.Duration
	AudioBytes int
	Chunks     int
	AudioTime  time.Duration
}

// VoiceSettings is one synthesis choice read from the shared settings service:
// the library voice and the speech-rate multiplier.
type VoiceSettings struct {
	Voice wyoming.Voice
	Rate  float64
}

// Speaker speaks one reply at a time.
type Speaker struct {
	client *wyoming.Client
	sink   Sink
	logger *slog.Logger

	voiceMu sync.RWMutex
	voice   wyoming.Voice
	rate    float64
	live    func() VoiceSettings

	// ttsCache is a bounded LRU of fully synthesized PCM for short replies.
	// cacheMu guards it and ttsFormat. A hit copies the PCM out from under the
	// lock and never holds the slice (or the lock) while writing to the sink.
	cacheMu   sync.Mutex
	ttsCache  *ttsCache
	ttsFormat audio.Format
}

// New builds a Speaker. A nil logger uses the default; an empty voice uses the
// service's default voice. The rate defaults to 1.0 (normal speed).
func New(client *wyoming.Client, sink Sink, voice wyoming.Voice, logger *slog.Logger) *Speaker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Speaker{client: client, sink: sink, voice: voice, rate: 1.0, logger: logger}
}

// BindVoice installs the live voice/rate source. It is late-bound in main
// because the settings service is created after the Tool registry the Speaker
// is passed into. Every reply, Filler and alert reads it per synthesis, so a
// Dashboard or voice write applies without recreating the Speaker.
func (s *Speaker) BindVoice(source func() VoiceSettings) {
	if s == nil {
		return
	}
	s.voiceMu.Lock()
	s.live = source
	s.voiceMu.Unlock()
}

// synthesis reads the current voice and rate in one snapshot. A nil or empty
// live source keeps the constructor's static voice, which is what tests use.
func (s *Speaker) synthesis() VoiceSettings {
	s.voiceMu.RLock()
	live := s.live
	current := VoiceSettings{Voice: s.voice, Rate: s.rate}
	s.voiceMu.RUnlock()
	if live != nil {
		if settings := live(); strings.TrimSpace(settings.Voice.Name) != "" {
			if settings.Rate <= 0 {
				settings.Rate = 1.0
			}
			return settings
		}
	}
	if current.Rate <= 0 {
		current.Rate = 1.0
	}
	return current
}

// SayText speaks text and discards the timing details, satisfying the api seam.
func (s *Speaker) SayText(ctx context.Context, text string) error {
	_, err := s.Say(ctx, text)
	return err
}

// Say synthesises text and plays it. Playback starts on the first chunk, not
// after the whole reply is synthesised. Short replies (at most ttsCacheMaxRunes)
// are served from a PCM cache keyed by the spoken text, voice, rate and audio
// format: a hit replays the cached audio with no TTS request and the same gain,
// sound-slot hook and latency marks as a miss. Cancelling ctx stops the audio.
func (s *Speaker) Say(ctx context.Context, text string) (Result, error) {
	started := time.Now()
	choice := s.synthesis()
	// A per-utterance override (a replay at a different speed) wins for this
	// one call. The live voice.rate setting is untouched, so the next synthesis
	// reads it again. This is per Say, not per Speaker or per turn: a Filler,
	// Confirmation prefix and Ring keep the setting's rate.
	rate := choice.Rate
	if override := RateFrom(ctx); override > 0 {
		rate = override
	}

	// Short replies may be served from the cache. Longer replies bypass it
	// entirely (neither looked up nor stored) to keep memory bounded on the Pi.
	// A voice or rate change misses because both are part of the key; a
	// Quiet-hours gain change does not, because the stored PCM is pre-gain.
	if runes := len([]rune(text)); runes <= ttsCacheMaxRunes {
		s.cacheMu.Lock()
		keyFormat := s.ttsFormat
		s.cacheMu.Unlock()
		key := cacheKey(text, choice.Voice.Name, rate, keyFormat)
		if pcm, format, ok := s.cacheLookup(key); ok {
			return s.playCached(ctx, text, pcm, format, started)
		}
	}

	// Miss: synthesise and tee the chunks so a clean stream can be cached.
	stream, err := s.client.Synthesize(ctx, text, choice.Voice, wyoming.WithRate(rate))
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()

	format := audio.Format{Rate: stream.Format.Rate, Width: stream.Format.Width, Channels: stream.Format.Channels}
	player, err := s.openPlayer(ctx, format, started)
	if err != nil {
		return Result{}, err
	}
	var tee bytes.Buffer
	var playErr error
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			playErr = err
			break
		}
		if err := player.write(ctx, chunk); err != nil {
			playErr = err
			break
		}
		tee.Write(chunk)
	}
	if err := player.close(); playErr == nil {
		playErr = err
	}
	result := player.result

	// Store only a clean stream that produced audio and was not cancelled, so a
	// Barge-in or a mid-stream failure never leaves a half-rendered entry.
	if runes := len([]rune(text)); runes <= ttsCacheMaxRunes && playErr == nil && ctx.Err() == nil && tee.Len() > 0 {
		s.cacheStore(cacheKey(text, choice.Voice.Name, rate, format), append([]byte(nil), tee.Bytes()...), format)
	}

	return s.logAndReturn(ctx, "miss", text, result, format, started, playErr)
}

// openPlayer opens the sink and wraps it with the per-turn gain writer, so a
// hit and a miss apply an identical Quiet-hours volume. It also captures the
// sound-slot hook so the first reply sample claims the slot.
func (s *Speaker) openPlayer(ctx context.Context, format audio.Format, started time.Time) (*pcmPlayer, error) {
	writer, err := s.sink.Open(ctx, format)
	if err != nil {
		return nil, err
	}
	// Quiet hours applies its absolute volume in software here, so the media
	// sink volume is never touched and needs no restore. The wrapper buffers a
	// split PCM16 sample at a chunk boundary. Stored PCM is pre-gain, so this
	// applies identically on a hit and a miss.
	writer = newGainWriter(writer, format, GainFrom(ctx))
	return &pcmPlayer{writer: writer, hook: FirstAudioHook(ctx), started: started, first: true}, nil
}

// pcmPlayer streams one reply's PCM to the sink, claiming the sound slot on the
// first sample and recording the latency marks. Both the live miss path and the
// cached hit path feed it chunks.
type pcmPlayer struct {
	writer  io.WriteCloser
	hook    func()
	started time.Time
	first   bool
	result  Result
}

func (p *pcmPlayer) write(ctx context.Context, chunk []byte) error {
	// The request's one sound slot is claimed immediately before the first
	// PCM submission. A Filler racing this hook loses and stays silent; a
	// Filler that already claimed the slot makes this first Write wait in
	// SerialSink, so reply audio never overlaps it. A hit takes the same hook,
	// so a Filler can never overlap a replayed reply either.
	if p.first && len(chunk) > 0 && p.hook != nil {
		p.hook()
	}
	n, err := p.writer.Write(chunk)
	if err != nil {
		return err
	}
	if p.first && n > 0 {
		p.first = false
		p.result.FirstAudio = time.Since(p.started)
		// The first PCM sample of an actual reply is Response latency's end
		// point, measured after the sink accepted it. A Filler is cached PCM
		// played elsewhere and never reaches here. The mark is deliberately
		// after Write: measuring before submission understated Response
		// latency by the write and buffer time.
		latency.SetStage(ctx, latency.StageTTSFirstAudio, p.result.FirstAudio)
		latency.MarkReplyAudio(ctx)
	}
	p.result.Chunks++
	p.result.AudioBytes += n
	return nil
}

func (p *pcmPlayer) close() error {
	return p.writer.Close()
}

// playCached replays cached PCM with no TTS request. It opens the sink, applies
// the gain writer, runs the sound-slot hook and writes the PCM in chunks,
// checking ctx between writes so a Barge-in stops promptly and stores nothing.
func (s *Speaker) playCached(ctx context.Context, text string, pcm []byte, format audio.Format, started time.Time) (Result, error) {
	player, err := s.openPlayer(ctx, format, started)
	if err != nil {
		return Result{}, err
	}
	var playErr error
	for off := 0; off < len(pcm); off += ttsCacheChunk {
		if ctx.Err() != nil {
			// A Barge-in cancelled the replay; stop without writing further
			// PCM and leave the cache untouched (a hit never stores).
			break
		}
		end := off + ttsCacheChunk
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := player.write(ctx, pcm[off:end]); err != nil {
			playErr = err
			break
		}
	}
	// A barge-in that cancelled mid-replay is an expected interruption, not a
	// fault; surface it so the caller knows the reply did not finish.
	if ctx.Err() != nil && playErr == nil {
		playErr = ctx.Err()
	}
	if err := player.close(); playErr == nil {
		playErr = err
	}
	return s.logAndReturn(ctx, "hit", text, player.result, format, started, playErr)
}

// logAndReturn finishes a Say: it derives audio time, emits the speech log line
// (with the tts_cache hit/miss marker) and returns the result or a wrapped
// error for an interruption or failure.
func (s *Speaker) logAndReturn(ctx context.Context, cache, text string, result Result, format audio.Format, started time.Time, playErr error) (Result, error) {
	if bytesPerSecond := format.Rate * format.Width * format.Channels; bytesPerSecond > 0 {
		result.AudioTime = time.Duration(result.AudioBytes) * time.Second / time.Duration(bytesPerSecond)
	}
	attrs := []any{
		"text_chars", len(text),
		"chunks", result.Chunks,
		"audio_ms", result.AudioTime.Milliseconds(),
		"first_audio_ms", result.FirstAudio.Milliseconds(),
		"duration_ms", time.Since(started).Milliseconds(),
		"tts_cache", cache,
	}
	if id := latency.RequestID(ctx); id != "" {
		attrs = append([]any{"request_id", id}, attrs...)
	}
	if playErr != nil {
		// A Barge-in cancels the request context; that is expected, not a fault.
		if ctx.Err() != nil {
			s.logger.Info("speech interrupted", attrs...)
		} else {
			s.logger.Error("speech", append(attrs, "error", playErr.Error())...)
		}
		return result, fmt.Errorf("speech: %w", playErr)
	}
	s.logger.Info("speech", attrs...)
	return result, nil
}

// firstAudioKey carries the per-request hook run immediately before the first
// reply PCM is submitted. It is per request, so one shared Speaker cannot mix
// a timer alert or a later turn into this request's sound slot.
type firstAudioKey struct{}

// WithFirstAudio attaches the sound-slot claim hook to ctx.
func WithFirstAudio(ctx context.Context, hook func()) context.Context {
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, firstAudioKey{}, hook)
}

// FirstAudioHook returns the hook attached by WithFirstAudio, or nil.
func FirstAudioHook(ctx context.Context) func() {
	hook, _ := ctx.Value(firstAudioKey{}).(func())
	return hook
}

// ttsCache is a bounded LRU of synthesized reply PCM, keyed by the
// post-pronunciation text, voice name, rate and audio format. It is not safe
// for concurrent use; the Speaker serialises access with cacheMu.
type ttsCache struct {
	ll    *list.List // front = most recently used, back = least recently used
	items map[string]*list.Element
	bytes int
}

type ttsEntry struct {
	key    string
	pcm    []byte
	format audio.Format
}

func newTTSCache() *ttsCache {
	return &ttsCache{ll: list.New(), items: make(map[string]*list.Element)}
}

func (c *ttsCache) get(key string) (ttsEntry, bool) {
	el, ok := c.items[key]
	if !ok {
		return ttsEntry{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(ttsEntry), true
}

// put inserts or replaces key and evicts least-recently-used entries while the
// entry or byte caps are exceeded. The just-inserted entry (at the front) is
// never evicted, so a single oversized phrase is still playable.
func (c *ttsCache) put(key string, pcm []byte, format audio.Format) {
	if el, ok := c.items[key]; ok {
		c.bytes -= len(el.Value.(ttsEntry).pcm)
		c.ll.Remove(el)
		delete(c.items, key)
	}
	el := c.ll.PushFront(ttsEntry{key: key, pcm: pcm, format: format})
	c.items[key] = el
	c.bytes += len(pcm)
	for (c.bytes > ttsCacheMaxBytes || c.ll.Len() > ttsCacheMaxEntries) && c.ll.Len() > 1 {
		back := c.ll.Back()
		be := back.Value.(ttsEntry)
		c.bytes -= len(be.pcm)
		c.ll.Remove(back)
		delete(c.items, be.key)
	}
}

// cacheLookup returns a copy of the cached PCM for key, or ok=false. The copy
// lets the caller write to the sink without holding cacheMu or risking a later
// eviction mutating the slice it is mid-write on.
func (s *Speaker) cacheLookup(key string) (pcm []byte, format audio.Format, ok bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.ttsCache == nil {
		return nil, audio.Format{}, false
	}
	entry, found := s.ttsCache.get(key)
	if !found {
		return nil, audio.Format{}, false
	}
	return append([]byte(nil), entry.pcm...), entry.format, true
}

// cacheStore records synthesized PCM for key. It also remembers the format so a
// later hit can rebuild the identical key without a fresh synthesis; the format
// is stable per voice/rate, so the last-seen value reproduces it.
func (s *Speaker) cacheStore(key string, pcm []byte, format audio.Format) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.ttsCache == nil {
		s.ttsCache = newTTSCache()
	}
	s.ttsFormat = format
	s.ttsCache.put(key, pcm, format)
}

// cacheKey identifies a cached phrase by its spoken text, voice name, rate and
// the audio format the service returned. Voice and rate are in the key so a
// change misses; the format is stable per voice/rate, so the Speaker's
// last-seen format rebuilds the same key on a hit without a new synthesis.
// Using the spoken (post-pronunciation) text means a pronunciation map change
// misses naturally instead of replaying audio for the old wording.
func cacheKey(text, voiceName string, rate float64, format audio.Format) string {
	return fmt.Sprintf("%s\x00%s\x00%.4f\x00%d/%d/%d", text, voiceName, rate, format.Rate, format.Width, format.Channels)
}
