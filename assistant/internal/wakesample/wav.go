// Package wakesample stores the owner's wake-phrase takes recorded in the
// Dashboard and exports them as the same bundle layout the desktop recorder
// writes: real/positive/real_p_00001.wav and real/negative/real_n_00001.wav.
//
// Everything on disk is canonical 16 kHz mono 16-bit PCM. A browser take is
// checked and trimmed here rather than trusted, so a phone that resamples
// differently still cannot put a malformed clip into the training set.
package wakesample

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	// SampleRate, Channels and BitsPerSample are the one format the training
	// pipeline reads. The browser converts before upload; the server checks
	// rather than resamples, so a client bug is visible.
	SampleRate     = 16000
	Channels       = 1
	BitsPerSample  = 16
	bytesPerSample = BitsPerSample / 8

	// MaxTakeSeconds is the longest upload accepted. Capture stops around two
	// seconds; the extra second is for a browser that keeps a little tail.
	MaxTakeSeconds = 3.0
	// MinTakeSeconds rejects a click or an empty file.
	MinTakeSeconds = 0.2
	// MaxClipSeconds matches record.py: the clip is capped so openWakeWord's
	// two-second augmentation buffer always fits.
	MaxClipSeconds = 1.9
	// TrimBefore and TrimAfter keep a short pre-roll and post-roll around the
	// detected speech, the same margins record.py uses.
	trimBefore = 0.15
	trimAfter  = 0.30
	// trimFloorDBFS and trimMarginDB mirror record.py's relative gate: a
	// webcam or phone mic idles around -41 dBFS, so an absolute floor would
	// call room noise a phrase.
	trimFloorDBFS = -45.0
	trimMarginDB  = 8.0
	frameSeconds  = 0.020
)

// ErrNotWAV is returned for a file that is not a canonical take.
var ErrNotWAV = errors.New("not a 16 kHz mono 16-bit WAV")

// Audio is one parsed take.
type Audio struct {
	Samples []int16
	// FoundSpeech reports whether an energy gate found speech. A take with
	// nothing above the room noise is kept whole and reported, so the
	// Dashboard can ask for a redo instead of silently training on silence.
	FoundSpeech bool
}

// Duration is the clip length in seconds.
func (a Audio) Duration() float64 {
	return float64(len(a.Samples)) / SampleRate
}

// DurationSeconds of a sample count.
func DurationSeconds(samples int) float64 { return float64(samples) / SampleRate }

// ParseWAV reads a RIFF/WAVE file and requires the one format training reads.
// Unknown chunks are skipped; a truncated or non-PCM file is rejected.
func ParseWAV(data []byte) ([]int16, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, ErrNotWAV
	}
	var (
		format    uint16
		channels  uint16
		rate      uint32
		bits      uint16
		pcm       []byte
		sawFormat bool
	)
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		body := offset + 8
		if size < 0 || body+size > len(data) {
			return nil, fmt.Errorf("%w: truncated %q chunk", ErrNotWAV, id)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("%w: short fmt chunk", ErrNotWAV)
			}
			format = binary.LittleEndian.Uint16(data[body : body+2])
			channels = binary.LittleEndian.Uint16(data[body+2 : body+4])
			rate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			bits = binary.LittleEndian.Uint16(data[body+14 : body+16])
			sawFormat = true
		case "data":
			pcm = data[body : body+size]
		}
		offset = body + size
		if size%2 == 1 && offset < len(data) {
			offset++ // RIFF chunks are word-aligned.
		}
	}
	if !sawFormat || pcm == nil {
		return nil, fmt.Errorf("%w: missing fmt or data chunk", ErrNotWAV)
	}
	if format != 1 {
		return nil, fmt.Errorf("%w: audio format %d is not PCM", ErrNotWAV, format)
	}
	if channels != Channels {
		return nil, fmt.Errorf("%w: %d channels", ErrNotWAV, channels)
	}
	if rate != SampleRate {
		return nil, fmt.Errorf("%w: %d Hz", ErrNotWAV, rate)
	}
	if bits != BitsPerSample {
		return nil, fmt.Errorf("%w: %d-bit", ErrNotWAV, bits)
	}
	if len(pcm)%bytesPerSample != 0 {
		return nil, fmt.Errorf("%w: odd data length", ErrNotWAV)
	}
	samples := make([]int16, len(pcm)/bytesPerSample)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*bytesPerSample:]))
	}
	return samples, nil
}

// Prepare validates one upload and returns the training clip. A take longer
// than MaxTakeSeconds is refused rather than silently cut; a take with its
// speech well inside the window is trimmed to the speech plus the record.py
// margins and capped at MaxClipSeconds.
func Prepare(data []byte) (Audio, error) {
	samples, err := ParseWAV(data)
	if err != nil {
		return Audio{}, err
	}
	duration := DurationSeconds(len(samples))
	if duration < MinTakeSeconds {
		return Audio{}, fmt.Errorf("the take is %.2f s; the minimum is %.1f s", duration, MinTakeSeconds)
	}
	if duration > MaxTakeSeconds {
		return Audio{}, fmt.Errorf("the take is %.2f s; the limit is %.1f s", duration, MaxTakeSeconds)
	}
	clip, found := trim(samples)
	return Audio{Samples: clip, FoundSpeech: found}, nil
}

// trim mirrors record.py's energy gate: 20 ms RMS frames, a threshold relative
// to the quiet part of the clip, 150 ms before and 300 ms after the speech,
// capped so the augmentation buffer fits.
func trim(audio []int16) ([]int16, bool) {
	frame := int(frameSeconds * SampleRate)
	if len(audio) < frame {
		return capClip(audio), false
	}
	usable := (len(audio) / frame) * frame
	db := make([]float64, usable/frame)
	for i := range db {
		var sum float64
		for _, s := range audio[i*frame : (i+1)*frame] {
			v := float64(s) / 32768
			sum += v * v
		}
		db[i] = 20 * math.Log10(math.Sqrt(sum/float64(frame))+1e-12)
	}
	floor := percentile(db, 10)
	threshold := math.Max(trimFloorDBFS, floor+trimMarginDB)
	first, last := -1, -1
	for i, v := range db {
		if v > threshold {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return capClip(audio), false
	}
	start := first*frame - int(trimBefore*SampleRate)
	if start < 0 {
		start = 0
	}
	end := (last+1)*frame + int(trimAfter*SampleRate)
	if end > len(audio) {
		end = len(audio)
	}
	return capClip(audio[start:end]), true
}

func capClip(audio []int16) []int16 {
	limit := int(MaxClipSeconds * SampleRate)
	if len(audio) > limit {
		// Keep the onset: that is what the detector keys on, and the trailing
		// post-roll is the cheapest thing to lose.
		return audio[:limit]
	}
	return audio
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	index := int(p / 100 * float64(len(sorted)-1))
	return sorted[index]
}

// EncodeWAV writes the canonical 44-byte-header PCM file.
func EncodeWAV(samples []int16) []byte {
	var buf bytes.Buffer
	dataLen := len(samples) * bytesPerSample
	buf.Grow(44 + dataLen)
	write := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	write(uint32(36 + dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	write(uint32(16))
	write(uint16(1))
	write(uint16(Channels))
	write(uint32(SampleRate))
	write(uint32(SampleRate * Channels * bytesPerSample))
	write(uint16(Channels * bytesPerSample))
	write(uint16(BitsPerSample))
	buf.WriteString("data")
	write(uint32(dataLen))
	for _, s := range samples {
		write(s)
	}
	return buf.Bytes()
}

// Info is the cheap header read the store uses to list takes without loading
// every clip. It validates the same format as ParseWAV.
func Info(data []byte) (samples int, err error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, ErrNotWAV
	}
	var (
		format    uint16
		channels  uint16
		rate      uint32
		bits      uint16
		dataBytes = -1
		sawFormat bool
	)
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		body := offset + 8
		if size < 0 {
			return 0, ErrNotWAV
		}
		if id == "fmt " {
			if body+16 > len(data) {
				return 0, ErrNotWAV
			}
			format = binary.LittleEndian.Uint16(data[body : body+2])
			channels = binary.LittleEndian.Uint16(data[body+2 : body+4])
			rate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			bits = binary.LittleEndian.Uint16(data[body+14 : body+16])
			sawFormat = true
		}
		if id == "data" {
			dataBytes = size
			break
		}
		if body+size > len(data) {
			// A prefix read stops before a large data chunk; the fmt fields
			// above are all this function needs.
			break
		}
		offset = body + size
		if size%2 == 1 {
			offset++
		}
	}
	if !sawFormat || dataBytes < 0 {
		return 0, ErrNotWAV
	}
	if format != 1 || channels != Channels || rate != SampleRate || bits != BitsPerSample {
		return 0, ErrNotWAV
	}
	return dataBytes / bytesPerSample, nil
}
