package voice

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// EndpointConfig is the fixed end-of-speech rule. It is deliberately not
// adaptive: #12 set a 500 ms silence window, and the value comes from config.
type EndpointConfig struct {
	// Rate is the sample rate of the PCM handed to Add.
	Rate int
	// Silence is how long the input must stay below the threshold after speech
	// before the utterance is over.
	Silence time.Duration
	// ThresholdDBFS is the RMS level below which a frame counts as silence.
	// The dev box's webcam mic sits around -45 dBFS with nobody talking.
	ThresholdDBFS float64
	// MinSpeech is how much above-threshold audio must accumulate before the
	// endpointer treats it as the question. It keeps a chime tail or a door
	// click from opening an utterance and then ending it on silence.
	MinSpeech time.Duration
	// MaxUtterance bounds a session so a false wake cannot listen forever.
	MaxUtterance time.Duration
}

// Endpointer turns a stream of PCM frames into one end-of-speech decision.
type Endpointer struct {
	cfg       EndpointConfig
	threshold float64
	speech    time.Duration
	heard     bool
	silent    time.Duration
	total     time.Duration
}

// NewEndpointer validates the config and returns a fresh endpointer.
func NewEndpointer(cfg EndpointConfig) (*Endpointer, error) {
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
		cfg:       cfg,
		threshold: math.Pow(10, cfg.ThresholdDBFS/20),
	}, nil
}

// Add consumes one mono PCM16 frame and reports whether the utterance ended.
// A frame is speech when its RMS is at or above the configured threshold.
func (e *Endpointer) Add(pcm []byte) bool {
	if len(pcm) < 2 {
		return false
	}
	duration := time.Duration(float64(len(pcm)/2) / float64(e.cfg.Rate) * float64(time.Second))
	e.total += duration

	if rmsLevel(pcm) >= e.threshold {
		e.speech += duration
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

// SpeechHeard reports whether any frame has crossed the threshold.
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
