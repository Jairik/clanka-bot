package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

// Upload limit and canonicalisation targets. An upload may be any length up
// to the size limit; role length caps are checked when a sound is assigned,
// never here.
const (
	// MaxUploadBytes is the largest accepted WAV or MP3 upload.
	MaxUploadBytes int64 = 10 << 20
	// canonicalChunkBytes bounds one decode/encode block.
	canonicalChunkBytes = 32 << 10
	// normaliseRMS is the target RMS level, about -18 dBFS, so cues sound
	// similarly loud. Peaks stay below normalisePeak (about -1 dBFS).
	normaliseRMS  = 0.125
	normalisePeak = 0.891
	// silenceRMS leaves near-silent uploads unamplified instead of boosting
	// noise; maxGain bounds how far a quiet recording is lifted.
	silenceRMS = 0.001
	maxGain    = 8.0
)

// Canonicalize decodes source (RIFF/WAVE or MP3), downmixes it to mono,
// resamples it to target and writes a canonical PCM16 WAV at output. The source
// is streamed through a temporary PCM spool, so a long MP3 does not expand in
// memory. The output file is created only after decoding succeeds; a failure
// leaves no partial file.
func Canonicalize(ctx context.Context, source, output string, target Format) (time.Duration, error) {
	if target.Rate <= 0 || target.Width != 2 || target.Channels != 1 {
		return 0, fmt.Errorf("audio: canonical format must be mono PCM16, got %+v", target)
	}
	kind, err := uploadKind(source)
	if err != nil {
		return 0, err
	}

	spool, err := os.CreateTemp(filepath.Dir(output), ".pcm-*")
	if err != nil {
		return 0, fmt.Errorf("audio: create pcm spool: %w", err)
	}
	spoolPath := spool.Name()
	defer func() {
		spool.Close()
		os.Remove(spoolPath)
	}()

	var levels levelStats
	write := func(pcm []byte) error {
		if len(pcm) == 0 {
			return nil
		}
		levels.add(pcm)
		if _, err := spool.Write(pcm); err != nil {
			return err
		}
		return nil
	}

	switch kind {
	case "wav":
		err = decodeWAVStream(ctx, source, target, write)
	case "mp3":
		err = decodeMP3Stream(ctx, source, target, write)
	default:
		err = fmt.Errorf("audio: %s is neither a WAV nor an MP3 file", filepath.Base(source))
	}
	if err != nil {
		return 0, err
	}
	if levels.frames == 0 {
		return 0, fmt.Errorf("audio: %s contains no audio frames", filepath.Base(source))
	}
	duration := time.Duration(levels.frames) * time.Second / time.Duration(target.Rate)

	gain := levels.gain()
	if err := spool.Sync(); err != nil {
		return 0, fmt.Errorf("audio: sync pcm spool: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("audio: rewind pcm spool: %w", err)
	}
	if err := writeNormalisedWAV(spool, output, levels.frames*int64(target.Width*target.Channels), target, gain); err != nil {
		return 0, err
	}
	return duration, nil
}

// uploadKind reads only the header bytes needed to choose a decoder.
func uploadKind(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("audio: open upload: %w", err)
	}
	defer file.Close()
	var head [12]byte
	n, err := io.ReadFull(file, head[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("audio: read upload header: %w", err)
	}
	if n >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE" {
		return "wav", nil
	}
	if n >= 3 && string(head[0:3]) == "ID3" {
		return "mp3", nil
	}
	if n >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 {
		return "mp3", nil
	}
	return "", fmt.Errorf("audio: %s is not a WAV or MP3 file", filepath.Base(path))
}

// decodeWAVStream decodes a WAV into canonical-rate mono PCM. ReadWAV keeps one
// file in memory, which the upload limit bounds; the output still streams.
func decodeWAVStream(ctx context.Context, path string, target Format, write func([]byte) error) error {
	decoded, err := ReadWAV(path)
	if err != nil {
		return err
	}
	resampler := NewResampler(decoded.Format.Rate, target.Rate)
	for offset := 0; offset < len(decoded.PCM); offset += canonicalChunkBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + canonicalChunkBytes
		if end > len(decoded.PCM) {
			end = len(decoded.PCM)
		}
		if err := write(resampler.Write(decoded.PCM[offset:end])); err != nil {
			return err
		}
	}
	return write(resampler.Flush())
}

// decodeMP3Stream decodes the decoder's fixed stereo PCM16 output to mono and
// resamples it chunk by chunk. The MP3 decoder is pure Go, so no ffmpeg runs at
// runtime (ADR-0001).
func decodeMP3Stream(ctx context.Context, path string, target Format, write func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("audio: open mp3: %w", err)
	}
	defer file.Close()
	decoder, err := mp3.NewDecoder(file)
	if err != nil {
		return fmt.Errorf("audio: decode mp3: %w", err)
	}
	rate := decoder.SampleRate()
	if rate <= 0 {
		return errors.New("audio: mp3 has no sample rate")
	}
	resampler := NewResampler(rate, target.Rate)
	raw := make([]byte, canonicalChunkBytes)
	var carry []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := decoder.Read(raw)
		if n > 0 {
			mono := downmixStereo16(carry, raw[:n])
			carry = mono.carry
			if err := write(resampler.Write(mono.pcm)); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("audio: decode mp3: %w", readErr)
		}
	}
	if len(carry) > 0 {
		return errors.New("audio: mp3 ended mid-frame")
	}
	return write(resampler.Flush())
}

// stereoDownmix is one downmix result plus the trailing partial frame that the
// next decoder read must complete.
type stereoDownmix struct {
	pcm   []byte
	carry []byte
}

// downmixStereo16 averages stereo PCM16 into mono. The decoder emits whole
// frames, but carrying a partial frame keeps the function safe for any reader.
func downmixStereo16(carry, raw []byte) stereoDownmix {
	if len(carry) > 0 {
		raw = append(append([]byte(nil), carry...), raw...)
	}
	frames := len(raw) / 4
	out := make([]byte, frames*2)
	for frame := 0; frame < frames; frame++ {
		left := int32(int16(binary.LittleEndian.Uint16(raw[frame*4:])))
		right := int32(int16(binary.LittleEndian.Uint16(raw[frame*4+2:])))
		binary.LittleEndian.PutUint16(out[frame*2:], uint16(int16((left+right)/2)))
	}
	var rest []byte
	if len(raw)%4 != 0 {
		rest = append([]byte(nil), raw[frames*4:]...)
	}
	return stereoDownmix{pcm: out, carry: rest}
}

// levelStats accumulates the RMS and peak while canonical PCM is written.
type levelStats struct {
	frames int64
	sumSq  float64
	peak   float64
}

func (l *levelStats) add(pcm []byte) {
	for index := 0; index+1 < len(pcm); index += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[index:]))) / math.MaxInt16
		absolute := math.Abs(sample)
		if absolute > l.peak {
			l.peak = absolute
		}
		l.sumSq += sample * sample
		l.frames++
	}
}

// gain is the loudness-normalisation factor: lift the RMS towards the target
// but never let the peak exceed the ceiling, and never amplify noise.
func (l *levelStats) gain() float64 {
	if l.frames == 0 || l.peak == 0 {
		return 1
	}
	rms := math.Sqrt(l.sumSq / float64(l.frames))
	gain := 1.0
	if rms >= silenceRMS {
		gain = normaliseRMS / rms
	}
	if ceiling := normalisePeak / l.peak; ceiling < gain {
		gain = ceiling
	}
	if gain > maxGain {
		gain = maxGain
	}
	if gain <= 0 {
		return 1
	}
	return gain
}

// writeNormalisedWAV writes the canonical header and applies the gain while
// copying the spool. The file is synced before it becomes visible.
func writeNormalisedWAV(spool *os.File, output string, dataBytes int64, format Format, gain float64) error {
	if dataBytes <= 0 {
		return errors.New("audio: canonical audio is empty")
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("audio: create canonical wav: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			file.Close()
			os.Remove(output)
		}
	}()
	if err := writeWAVHeader(file, dataBytes, format); err != nil {
		return err
	}
	buf := make([]byte, canonicalChunkBytes)
	var written int64
	for {
		n, readErr := spool.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if gain != 1 {
				chunk = applyGain(chunk, gain)
			}
			if _, err := file.Write(chunk); err != nil {
				return fmt.Errorf("audio: write canonical wav: %w", err)
			}
			written += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("audio: read pcm spool: %w", readErr)
		}
	}
	if written != dataBytes {
		return fmt.Errorf("audio: canonical wav has %d bytes, want %d", written, dataBytes)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("audio: sync canonical wav: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("audio: close canonical wav: %w", err)
	}
	ok = true
	return nil
}

// applyGain scales PCM16 samples without changing alignment.
func applyGain(pcm []byte, gain float64) []byte {
	out := make([]byte, len(pcm))
	for index := 0; index+1 < len(pcm); index += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[index:]))) * gain
		binary.LittleEndian.PutUint16(out[index:], uint16(clampPCM16(value)))
	}
	if len(pcm)%2 != 0 {
		out[len(out)-1] = pcm[len(pcm)-1]
	}
	return out
}

// EncodeWAV returns a complete canonical RIFF/WAVE file for browser playback.
func EncodeWAV(pcm []byte, format Format) []byte {
	dataBytes := int64(len(pcm))
	out := make([]byte, 44+len(pcm))
	fillWAVHeader(out[:44], dataBytes, format)
	copy(out[44:], pcm)
	return out
}

// writeWAVHeader writes one canonical 44-byte PCM16 header.
func writeWAVHeader(w io.Writer, dataBytes int64, format Format) error {
	var header [44]byte
	fillWAVHeader(header[:], dataBytes, format)
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("audio: write wav header: %w", err)
	}
	return nil
}

// fillWAVHeader fills a canonical RIFF/WAVE PCM16 header. The caller must
// supply exactly 44 bytes.
func fillWAVHeader(header []byte, dataBytes int64, format Format) {
	byteRate := format.Rate * format.Width * format.Channels
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataBytes))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:24], uint16(format.Channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(format.Rate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(format.Channels*format.Width))
	binary.LittleEndian.PutUint16(header[34:36], uint16(format.Width*8))
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataBytes))
}
