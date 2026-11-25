package voice

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// DefaultVADThreshold is the Silero VAD speech probability threshold.
// Release hysteresis sits 0.15 below it so trailing consonants and breaths do
// not prematurely start the silence countdown.
const (
	DefaultVADThreshold = 0.45
	vadHysteresisGap    = 0.15
	vadMinNegThreshold  = 0.05
)

// FrameDetector classifies one PCM frame as speech or non-speech (for example
// Silero VAD). Returning an error causes Endpointer to fall back to RMS.
type FrameDetector interface {
	IsSpeech(pcm []byte) (bool, error)
}

// VADSession scores one PCM frame as a speech probability in [0, 1].
type VADSession interface {
	Score(pcm []byte) (float64, error)
	Close() error
}

// VADDetector turns a streaming speech-probability scorer into a FrameDetector
// with Schmitt-trigger hysteresis.
type VADDetector struct {
	session      VADSession
	threshold    float64
	negThreshold float64
	speaking     bool
}

// NewVADDetector builds a hysteresis speech detector around session.
// A non-positive threshold selects DefaultVADThreshold.
func NewVADDetector(session VADSession, threshold float64) *VADDetector {
	if threshold <= 0 || threshold >= 1 {
		threshold = DefaultVADThreshold
	}
	neg := threshold - vadHysteresisGap
	if neg < vadMinNegThreshold {
		neg = vadMinNegThreshold
	}
	return &VADDetector{
		session:      session,
		threshold:    threshold,
		negThreshold: neg,
	}
}

// IsSpeech scores pcm and updates hysteresis state.
func (d *VADDetector) IsSpeech(pcm []byte) (bool, error) {
	prob, err := d.session.Score(pcm)
	if err != nil {
		return false, err
	}
	if !d.speaking {
		if prob >= d.threshold {
			d.speaking = true
		}
	} else if prob < d.negThreshold {
		d.speaking = false
	}
	return d.speaking, nil
}

// EndpointConfig is the end-of-speech rule driven by config.
type EndpointConfig struct {
	// Rate is the sample rate of the PCM handed to Add.
	Rate int
	// VAD enables neural voice activity detection (Silero VAD hosted by stt).
	// When false or unavailable, endpointing falls back to ThresholdDBFS RMS.
	VAD bool
	// VADThreshold is the Silero speech probability onset threshold (0..1).
	// Zero selects DefaultVADThreshold when VAD is active.
	VADThreshold float64
	// Silence is how long the input must stay non-speech after speech before
	// the utterance is over.
	Silence time.Duration
	// ThresholdDBFS is the fallback RMS level below which a frame counts as
	// silence when VAD is disabled or unreachable.
	ThresholdDBFS float64
	// MinSpeech is how much speech audio must accumulate before the endpointer
	// treats it as the question. It keeps a chime tail or a door click from
	// opening an utterance and then ending it on silence.
	MinSpeech time.Duration
	// MaxUtterance bounds a session so a false wake cannot listen forever.
	MaxUtterance time.Duration
}

// Endpointer turns a stream of PCM frames into one end-of-speech decision.
type Endpointer struct {
	cfg        EndpointConfig
	threshold  float64
	detector   FrameDetector
	onFallback func(error)
	vadFailed  bool
	speech     time.Duration
	heard      bool
	silent     time.Duration
	total      time.Duration
	now        func() time.Time
	lastVoice  time.Time
}

// NewEndpointer validates the config and returns an RMS-backed endpointer.
func NewEndpointer(cfg EndpointConfig) (*Endpointer, error) {
	return NewEndpointerWithDetector(cfg, nil, nil)
}

// NewEndpointerWithDetector returns an endpointer that classifies frames with
// detector when non-nil, falling back to RMS if detector returns an error.
func NewEndpointerWithDetector(cfg EndpointConfig, detector FrameDetector, onFallback func(error)) (*Endpointer, error) {
	if cfg.Rate <= 0 {
		return nil, fmt.Errorf("voice: endpoint rate must be positive")
	}
	if cfg.Silence <= 0 {
		return nil, fmt.Errorf("voice: endpoint silence must be positive")
	}
	if cfg.MaxUtterance > 0 && cfg.MaxUtterance < cfg.Silence {
		return nil, fmt.Errorf("voice: endpoint max_utterance must be at least the silence window")
	}
	return &Endpointer{
		cfg:        cfg,
		threshold:  math.Pow(10, cfg.ThresholdDBFS/20),
		detector:   detector,
		onFallback: onFallback,
		now:        time.Now,
	}, nil
}

// UsingVAD reports whether the endpointer is currently classifying frames via
// its neural detector rather than the RMS fallback.
func (e *Endpointer) UsingVAD() bool {
	return e.detector != nil && !e.vadFailed
}

// Add consumes one mono PCM16 frame and reports whether the utterance ended.
func (e *Endpointer) Add(pcm []byte) bool {
	if len(pcm) < 2 {
		return false
	}
	duration := time.Duration(float64(len(pcm)/2) / float64(e.cfg.Rate) * float64(time.Second))
	e.total += duration

	if e.isSpeech(pcm) {
		e.speech += duration
		// Last voiced frame is where Response latency starts (#12/#27).
		e.lastVoice = e.now()
		if !e.heard && e.speech >= e.cfg.MinSpeech {
			e.heard = true
		}
		if e.heard {
			e.silent = 0
		}
	} else if e.heard {
		e.silent += duration
	}

	if e.cfg.MaxUtterance > 0 && e.total >= e.cfg.MaxUtterance {
		return true
	}
	return e.heard && e.silent >= e.cfg.Silence
}

func (e *Endpointer) isSpeech(pcm []byte) bool {
	if e.detector != nil && !e.vadFailed {
		speech, err := e.detector.IsSpeech(pcm)
		if err == nil {
			return speech
		}
		e.vadFailed = true
		if e.onFallback != nil {
			e.onFallback(err)
		}
	}
	return rmsLevel(pcm) >= e.threshold
}

// LastSpeech is the arrival time of the most recent voiced frame: the user's
// last voiced sound. It is zero before any speech.
func (e *Endpointer) LastSpeech() time.Time { return e.lastVoice }

// SpeechHeard reports whether enough voiced frames have accumulated.
func (e *Endpointer) SpeechHeard() bool { return e.heard }

// Silence reports how much quiet audio has accumulated since speech.
func (e *Endpointer) Silence() time.Duration { return e.silent }

// rmsLevel returns the frame's RMS as a fraction of full scale (0..1).
func rmsLevel(pcm []byte) float64 {
	samples := len(pcm) / 2
	if samples == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < samples; i++ {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) / math.MaxInt16
		sum += value * value
	}
	return math.Sqrt(sum / float64(samples))
}
