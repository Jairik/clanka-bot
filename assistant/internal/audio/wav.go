package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// maxWAVBytes bounds one library file. The biggest Cue role cap is 60 s, which
// is about 11.5 MB as 48 kHz stereo PCM16; the extra room covers 32-bit files
// that are still rejected by the role cap after their duration is known.
const maxWAVBytes = 64 << 20

// WAVInfo is what the header says about a WAV file, without decoding samples.
type WAVInfo struct {
	// Format is the file's own layout. Cue playback downmixes and resamples to
	// the sink format, so this is not necessarily what reaches the speaker.
	Format   Format
	Duration time.Duration
}

// WAV is decoded mono signed 16-bit PCM at the file's sample rate. Channels
// are averaged; the caller resamples when the sink needs another rate.
type WAV struct {
	Format   Format
	PCM      []byte
	Duration time.Duration
}

// wavHeader is the parsed fmt and data layout of one RIFF/WAVE file.
type wavHeader struct {
	formatCode int
	channels   int
	rate       int
	bits       int
	dataOffset int64
	dataBytes  int64
}

// InspectWAV reads only the RIFF and fmt chunks, so scanning a folder does not
// decode every file. It rejects anything a length cap could not trust: a
// non-PCM format, a missing data chunk, a truncated file or an absurd size.
func InspectWAV(path string) (WAVInfo, error) {
	file, size, err := openWAV(path)
	if err != nil {
		return WAVInfo{}, err
	}
	defer file.Close()

	header, err := readWAVHeader(file, size)
	if err != nil {
		return WAVInfo{}, fmt.Errorf("audio: %s: %w", path, err)
	}
	format, err := header.sourceFormat()
	if err != nil {
		return WAVInfo{}, fmt.Errorf("audio: %s: %w", path, err)
	}
	return WAVInfo{Format: format, Duration: header.duration()}, nil
}

// ReadWAV decodes a complete file to mono PCM16 at its native sample rate.
func ReadWAV(path string) (WAV, error) {
	file, size, err := openWAV(path)
	if err != nil {
		return WAV{}, err
	}
	defer file.Close()

	header, err := readWAVHeader(file, size)
	if err != nil {
		return WAV{}, fmt.Errorf("audio: %s: %w", path, err)
	}
	if header.dataBytes > maxWAVBytes {
		return WAV{}, fmt.Errorf("audio: %s: data chunk is %d bytes, over the %d byte limit", path, header.dataBytes, maxWAVBytes)
	}
	raw := make([]byte, header.dataBytes)
	if _, err := file.ReadAt(raw, header.dataOffset); err != nil && !errors.Is(err, io.EOF) {
		return WAV{}, fmt.Errorf("audio: %s: read samples: %w", path, err)
	}

	pcm, err := decodeWAVSamples(raw, header)
	if err != nil {
		return WAV{}, fmt.Errorf("audio: %s: %w", path, err)
	}
	if len(pcm) == 0 {
		return WAV{}, fmt.Errorf("audio: %s: no audio frames", path)
	}
	return WAV{
		Format:   Format{Rate: header.rate, Width: 2, Channels: 1},
		PCM:      pcm,
		Duration: header.duration(),
	}, nil
}

// ResamplePCM converts mono PCM16 between sample rates with linear
// interpolation. Cue playback runs on the same pure-Go path as the wake chime;
// no external resampler is needed.
func ResamplePCM(pcm []byte, from, to int) []byte {
	if from <= 0 || to <= 0 || len(pcm) < 2 {
		return nil
	}
	if from == to {
		return pcm
	}
	in := len(pcm) / 2
	out := int(math.Round(float64(in) * float64(to) / float64(from)))
	if out <= 0 {
		return nil
	}
	result := make([]byte, out*2)
	sample := func(i int) float64 {
		if i < 0 {
			i = 0
		}
		if i >= in {
			i = in - 1
		}
		return float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}
	for i := 0; i < out; i++ {
		src := float64(i) * float64(from) / float64(to)
		index := int(src)
		frac := src - float64(index)
		value := sample(index) + (sample(index+1)-sample(index))*frac
		binary.LittleEndian.PutUint16(result[i*2:], uint16(int16(math.Round(value))))
	}
	return result
}

// openWAV validates the file size before any chunk walk.
func openWAV(path string) (*os.File, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("audio: open %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("audio: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, 0, fmt.Errorf("audio: %s is not a regular file", path)
	}
	if info.Size() > maxWAVBytes {
		file.Close()
		return nil, 0, fmt.Errorf("audio: %s is %d bytes, over the %d byte limit", path, info.Size(), maxWAVBytes)
	}
	return file, info.Size(), nil
}

// readWAVHeader walks the chunk list until both fmt and data are known.
func readWAVHeader(file *os.File, size int64) (wavHeader, error) {
	if size < 12 {
		return wavHeader{}, errors.New("not a RIFF/WAVE file")
	}
	var riff [12]byte
	if _, err := file.ReadAt(riff[:], 0); err != nil {
		return wavHeader{}, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return wavHeader{}, errors.New("not a RIFF/WAVE file")
	}

	header := wavHeader{formatCode: -1}
	var foundFmt, foundData bool
	pos := int64(12)
	for pos+8 <= size {
		var chunk [8]byte
		if _, err := file.ReadAt(chunk[:], pos); err != nil {
			return wavHeader{}, fmt.Errorf("read chunk header: %w", err)
		}
		id := string(chunk[0:4])
		length := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		payload := pos + 8
		if length < 0 || payload+length > size {
			return wavHeader{}, fmt.Errorf("chunk %q is truncated", id)
		}
		switch id {
		case "fmt ":
			raw := make([]byte, length)
			if _, err := file.ReadAt(raw, payload); err != nil {
				return wavHeader{}, fmt.Errorf("read fmt chunk: %w", err)
			}
			if err := parseWAVFormat(raw, &header); err != nil {
				return wavHeader{}, err
			}
			foundFmt = true
		case "data":
			header.dataOffset = payload
			header.dataBytes = length
			foundData = true
		}
		if foundFmt && foundData {
			break
		}
		// RIFF chunks are word-aligned: an odd payload has one pad byte.
		pos = payload + length + length&1
	}
	if !foundFmt {
		return wavHeader{}, errors.New("no fmt chunk")
	}
	if !foundData {
		return wavHeader{}, errors.New("no data chunk")
	}
	if header.dataBytes == 0 {
		return wavHeader{}, errors.New("data chunk carries no audio")
	}
	if _, err := header.sourceFormat(); err != nil {
		return wavHeader{}, err
	}
	return header, nil
}

// parseWAVFormat reads the standard 16-byte fmt plus the WAVE_FORMAT_EXTENSIBLE
// sub-format when present. Only integer PCM and 32-bit float are accepted.
func parseWAVFormat(raw []byte, header *wavHeader) error {
	if len(raw) < 16 {
		return errors.New("fmt chunk is too short")
	}
	header.formatCode = int(binary.LittleEndian.Uint16(raw[0:2]))
	header.channels = int(binary.LittleEndian.Uint16(raw[2:4]))
	header.rate = int(binary.LittleEndian.Uint32(raw[4:8]))
	header.bits = int(binary.LittleEndian.Uint16(raw[14:16]))
	if header.formatCode == 0xFFFE && len(raw) >= 40 {
		// The first two bytes of the SubFormat GUID are the effective format.
		header.formatCode = int(binary.LittleEndian.Uint16(raw[24:26]))
	}
	return nil
}

// format validates the layout and returns the source audio.Format.
func (h wavHeader) sourceFormat() (Format, error) {
	if h.channels < 1 || h.channels > 8 {
		return Format{}, fmt.Errorf("unsupported channel count %d", h.channels)
	}
	if h.rate <= 0 || h.rate > 384000 {
		return Format{}, fmt.Errorf("unsupported sample rate %d", h.rate)
	}
	switch h.bits {
	case 8, 16, 24, 32:
	default:
		return Format{}, fmt.Errorf("unsupported sample width %d bits", h.bits)
	}
	switch h.formatCode {
	case 1, 3:
	default:
		return Format{}, fmt.Errorf("unsupported WAV format %d (only PCM and 32-bit float)", h.formatCode)
	}
	if h.formatCode == 3 && h.bits != 32 {
		return Format{}, fmt.Errorf("float WAV must be 32-bit, got %d", h.bits)
	}
	return Format{Rate: h.rate, Width: h.bits / 8, Channels: h.channels}, nil
}

// duration is the data chunk length divided by the byte rate.
func (h wavHeader) duration() time.Duration {
	byteRate := int64(h.rate * h.channels * h.bits / 8)
	if byteRate <= 0 {
		return 0
	}
	return time.Duration(h.dataBytes * int64(time.Second) / byteRate)
}

// decodeWAVSamples converts one data chunk to mono PCM16. Integer samples keep
// their most significant bits; float samples are clamped to full scale.
func decodeWAVSamples(raw []byte, header wavHeader) ([]byte, error) {
	width := header.bits / 8
	if width <= 0 || header.channels <= 0 {
		return nil, errors.New("invalid sample layout")
	}
	frameBytes := width * header.channels
	if frameBytes <= 0 || len(raw) < frameBytes {
		return nil, errors.New("data chunk is shorter than one frame")
	}
	frames := len(raw) / frameBytes
	out := make([]byte, frames*2)
	for frame := 0; frame < frames; frame++ {
		sum := int64(0)
		offset := frame * frameBytes
		for channel := 0; channel < header.channels; channel++ {
			sum += int64(readWAVSample(raw[offset+channel*width:], header))
		}
		sample := int16(sum / int64(header.channels))
		binary.LittleEndian.PutUint16(out[frame*2:], uint16(sample))
	}
	return out, nil
}

// readWAVSample returns one signed 16-bit sample from any accepted layout.
func readWAVSample(raw []byte, header wavHeader) int16 {
	switch header.formatCode {
	case 3:
		return floatToPCM16(math.Float32frombits(binary.LittleEndian.Uint32(raw)))
	default:
		switch header.bits {
		case 8:
			return int16((int(raw[0]) - 128) << 8)
		case 16:
			return int16(binary.LittleEndian.Uint16(raw))
		case 24:
			value := int32(raw[0]) | int32(raw[1])<<8 | int32(raw[2])<<16
			if value&0x800000 != 0 {
				value |= ^0xFFFFFF
			}
			return int16(value >> 8)
		case 32:
			return int16(int32(binary.LittleEndian.Uint32(raw)) >> 16)
		}
	}
	return 0
}

// floatToPCM16 maps [-1, 1] float samples to signed 16-bit PCM.
func floatToPCM16(value float32) int16 {
	if value > 1 {
		value = 1
	}
	if value < -1 {
		value = -1
	}
	return int16(math.Round(float64(value) * math.MaxInt16))
}
