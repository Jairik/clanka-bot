package audio_test

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// wavBytes builds a canonical little-endian RIFF/WAVE file for the decoder
// tests. Samples are interleaved by channel.
func wavBytes(rate, channels, bits int, samples []int32) []byte {
	width := bits / 8
	data := make([]byte, len(samples)*width)
	for i, sample := range samples {
		switch bits {
		case 8:
			data[i] = byte(sample)
		case 16:
			binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(sample)))
		case 24:
			value := uint32(sample)
			data[i*3] = byte(value)
			data[i*3+1] = byte(value >> 8)
			data[i*3+2] = byte(value >> 16)
		case 32:
			binary.LittleEndian.PutUint32(data[i*4:], uint32(sample))
		}
	}
	byteRate := rate * channels * width
	blockAlign := channels * width
	out := make([]byte, 44+len(data))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(data)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(out[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:36], uint16(bits))
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(data)))
	copy(out[44:], data)
	return out
}

func writeWAV(t *testing.T, name string, rate, channels, bits int, samples []int32) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, wavBytes(rate, channels, bits, samples), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	return path
}

func TestInspectWAVReportsDurationAndSourceFormat(t *testing.T) {
	// 8000 frames at 16 kHz is exactly half a second.
	path := writeWAV(t, "half.wav", 16000, 1, 16, make([]int32, 8000))
	info, err := audio.InspectWAV(path)
	if err != nil {
		t.Fatalf("InspectWAV: %v", err)
	}
	if info.Format.Rate != 16000 || info.Format.Width != 2 || info.Format.Channels != 1 {
		t.Fatalf("format = %+v", info.Format)
	}
	if info.Duration != 500*time.Millisecond {
		t.Fatalf("duration = %v, want 500ms", info.Duration)
	}
}

func TestReadWAVDownmixesStereoToMonoPCM16(t *testing.T) {
	path := writeWAV(t, "stereo.wav", 24000, 2, 16, []int32{1000, 3000, -1000, 1000})
	decoded, err := audio.ReadWAV(path)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if decoded.Format != (audio.Format{Rate: 24000, Width: 2, Channels: 1}) {
		t.Fatalf("format = %+v", decoded.Format)
	}
	got := []int16{
		int16(binary.LittleEndian.Uint16(decoded.PCM[0:])),
		int16(binary.LittleEndian.Uint16(decoded.PCM[2:])),
	}
	want := []int16{2000, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestReadWAVDecodesEightAndTwentyFourBitSamples(t *testing.T) {
	for _, test := range []struct {
		name  string
		bits  int
		input int32
		want  int16
	}{
		{name: "8-bit", bits: 8, input: 192, want: 16384},
		{name: "24-bit", bits: 24, input: 0x400000, want: 16384},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeWAV(t, "sample.wav", 16000, 1, test.bits, []int32{test.input})
			decoded, err := audio.ReadWAV(path)
			if err != nil {
				t.Fatalf("ReadWAV: %v", err)
			}
			got := int16(binary.LittleEndian.Uint16(decoded.PCM))
			if got != test.want {
				t.Fatalf("sample = %d, want %d", got, test.want)
			}
		})
	}
}

func TestReadWAVDecodesFloatSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "float.wav")
	raw := wavBytes(16000, 1, 32, []int32{0}) // reuse the RIFF builder, then patch fmt
	// float32 0.5 at offset 44; bits=32, format=3.
	binary.LittleEndian.PutUint16(raw[20:22], 3)
	binary.LittleEndian.PutUint16(raw[34:36], 32)
	raw[44] = byte(math.Float32bits(0.5))
	raw[45] = byte(math.Float32bits(0.5) >> 8)
	raw[46] = byte(math.Float32bits(0.5) >> 16)
	raw[47] = byte(math.Float32bits(0.5) >> 24)
	binary.LittleEndian.PutUint32(raw[40:44], 4)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write float wav: %v", err)
	}
	decoded, err := audio.ReadWAV(path)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if got := int16(binary.LittleEndian.Uint16(decoded.PCM)); got != 16384 {
		t.Fatalf("float sample = %d, want 16384", got)
	}
}

func TestInspectWAVRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "not riff", data: []byte("hello world, not a wav file")},
		{name: "no data chunk", data: append(wavBytes(16000, 1, 16, nil)[:36], 0, 0, 0, 0, 0, 0, 0, 0)},
		{name: "truncated data", data: wavBytes(16000, 1, 16, []int32{1, 2})[:40]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.wav")
			if err := os.WriteFile(path, test.data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := audio.InspectWAV(path); err == nil {
				t.Fatal("InspectWAV accepted an invalid file")
			}
			if _, err := audio.ReadWAV(path); err == nil {
				t.Fatal("ReadWAV accepted an invalid file")
			}
		})
	}
}

func TestResamplePCMChangesRateAndKeepsDuration(t *testing.T) {
	input := make([]byte, 8000*2) // 0.5 s at 8 kHz
	for i := 0; i < 8000; i++ {
		binary.LittleEndian.PutUint16(input[i*2:], uint16(int16(1000)))
	}
	output := audio.ResamplePCM(input, 8000, 16000)
	if len(output) != 16000*2 {
		t.Fatalf("resampled bytes = %d, want %d", len(output), 16000*2)
	}
	for i := 0; i < len(output); i += 2 {
		if got := int16(binary.LittleEndian.Uint16(output[i:])); got != 1000 {
			t.Fatalf("sample %d = %d, want a constant 1000", i/2, got)
		}
	}
	if audio.ResamplePCM(input, 0, 8000) != nil {
		t.Fatal("ResamplePCM accepted a zero source rate")
	}
}
