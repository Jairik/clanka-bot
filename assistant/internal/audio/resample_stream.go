package audio

import (
	"encoding/binary"
	"math"
)

// Resampler converts mono PCM16 between sample rates incrementally, so an
// uploaded MP3 can be decoded and resampled without holding the whole file in
// memory. Output samples use linear interpolation, matching ResamplePCM; the
// interpolation state survives chunk boundaries.
type Resampler struct {
	from int
	to   int
	// pos is the source position of the next output sample, relative to buf.
	pos float64
	// buf holds unconsumed source samples. It stays at most a few samples long.
	buf []int16
	// carry holds an odd trailing input byte until the next Write.
	carry []byte
}

// NewResampler builds a stream resampler. An invalid or equal rate is a
// pass-through, which keeps callers free of a special case.
func NewResampler(from, to int) *Resampler {
	return &Resampler{from: from, to: to}
}

// Write feeds mono PCM16 and returns the output samples that are ready. The
// returned slice is owned by the caller.
func (r *Resampler) Write(pcm []byte) []byte {
	if r == nil || len(pcm) == 0 {
		return nil
	}
	if r.from <= 0 || r.to <= 0 || r.from == r.to {
		return pcm
	}
	if len(r.carry) > 0 {
		pcm = append(r.carry, pcm...)
		r.carry = nil
	}
	if len(pcm)%2 != 0 {
		r.carry = append([]byte(nil), pcm[len(pcm)-1:]...)
		pcm = pcm[:len(pcm)-1]
	}
	for index := 0; index+1 < len(pcm); index += 2 {
		r.buf = append(r.buf, int16(binary.LittleEndian.Uint16(pcm[index:])))
	}
	return r.emit(false)
}

// Flush returns the last samples, repeating the final source sample to fill
// the interpolation tail.
func (r *Resampler) Flush() []byte {
	if r == nil {
		return nil
	}
	if r.from <= 0 || r.to <= 0 || r.from == r.to {
		r.buf = nil
		return nil
	}
	return r.emit(true)
}

// emit interpolates as many complete output samples as the buffer allows. At
// the end of the stream the final source sample is reused for the tail.
func (r *Resampler) emit(final bool) []byte {
	if len(r.buf) == 0 {
		return nil
	}
	step := float64(r.from) / float64(r.to)
	capacity := int(math.Ceil(float64(len(r.buf)-1)/step)) + 1
	if capacity < 0 {
		capacity = 0
	}
	out := make([]int16, 0, capacity)
	last := len(r.buf) - 1
	for {
		index := int(math.Floor(r.pos))
		if index > last {
			break
		}
		if index == last {
			if !final {
				break
			}
			out = append(out, r.buf[last])
			r.pos += step
			continue
		}
		frac := r.pos - float64(index)
		value := float64(r.buf[index]) + (float64(r.buf[index+1])-float64(r.buf[index]))*frac
		out = append(out, clampPCM16(value))
		r.pos += step
	}
	// Drop consumed samples but keep the sample the next output interpolates
	// from, so a chunk boundary cannot change the waveform.
	drop := int(math.Floor(r.pos))
	if final {
		drop = len(r.buf)
	}
	if drop > 0 {
		if drop >= len(r.buf) {
			drop = len(r.buf) - 1
		}
		r.buf = append(r.buf[:0], r.buf[drop:]...)
		r.pos -= float64(drop)
	}
	if final {
		r.buf = nil
		r.pos = 0
	}
	if len(out) == 0 {
		return nil
	}
	bytes := make([]byte, len(out)*2)
	for index, sample := range out {
		binary.LittleEndian.PutUint16(bytes[index*2:], uint16(sample))
	}
	return bytes
}

// clampPCM16 rounds and clamps one interpolated sample to the PCM16 range.
func clampPCM16(value float64) int16 {
	rounded := math.Round(value)
	if rounded > math.MaxInt16 {
		return math.MaxInt16
	}
	if rounded < math.MinInt16 {
		return math.MinInt16
	}
	return int16(rounded)
}
