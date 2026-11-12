package audio_test

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// writeTestWAV writes an integer PCM WAV with the given layout. It is a test
// helper only; production canonical files are written by audio.Canonicalize.
func writeTestWAV(t *testing.T, path string, rate, channels, bits int, pcm []byte) {
	t.Helper()
	header := make([]byte, 44)
	byteRate := rate * channels * bits / 8
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(pcm)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(channels*bits/8))
	binary.LittleEndian.PutUint16(header[34:36], uint16(bits))
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(pcm)))
	if err := os.WriteFile(path, append(header, pcm...), 0o600); err != nil {
		t.Fatalf("write test wav: %v", err)
	}
}

// sinePCM makes interleaved PCM16 for a quiet sine wave at the given layout.
func sinePCM(rate, channels int, seconds, amplitude float64) []byte {
	frames := int(float64(rate) * seconds)
	out := make([]byte, frames*channels*2)
	for frame := 0; frame < frames; frame++ {
		value := int16(math.Round(amplitude * math.MaxInt16 * math.Sin(2*math.Pi*440*float64(frame)/float64(rate))))
		for channel := 0; channel < channels; channel++ {
			binary.LittleEndian.PutUint16(out[(frame*channels+channel)*2:], uint16(value))
		}
	}
	return out
}

func TestResamplerMatchesAWholeBufferConversion(t *testing.T) {
	input := sinePCM(8000, 1, 0.1, 0.5)
	whole := audio.ResamplePCM(input, 8000, 16000)
	stream := audio.NewResampler(8000, 16000)
	var got []byte
	for offset := 0; offset < len(input); offset += 7 {
		end := offset + 7
		if end > len(input) {
			end = len(input)
		}
		got = append(got, stream.Write(input[offset:end])...)
	}
	got = append(got, stream.Flush()...)
	if len(got) != len(whole) {
		t.Fatalf("stream length = %d, whole-buffer length = %d", len(got), len(whole))
	}
	for index := 0; index+1 < len(got); index += 2 {
		a := int16(binary.LittleEndian.Uint16(got[index:]))
		b := int16(binary.LittleEndian.Uint16(whole[index:]))
		if a != b {
			t.Fatalf("sample %d = %d, whole-buffer = %d", index/2, a, b)
		}
	}
}

func TestResamplerKeepsExactRateForOneSecond(t *testing.T) {
	input := sinePCM(44100, 1, 1.0, 0.2)
	stream := audio.NewResampler(44100, 16000)
	var got []byte
	for offset := 0; offset < len(input); offset += 4096 {
		end := offset + 4096
		if end > len(input) {
			end = len(input)
		}
		got = append(got, stream.Write(input[offset:end])...)
	}
	got = append(got, stream.Flush()...)
	samples := len(got) / 2
	if samples < 15998 || samples > 16002 {
		t.Fatalf("resampled 1 s to %d samples, want about 16000", samples)
	}
}

func TestResamplerIdentityIsAPassThrough(t *testing.T) {
	input := sinePCM(16000, 1, 0.01, 0.5)
	stream := audio.NewResampler(16000, 16000)
	got := stream.Write(input)
	if len(got) != len(input) {
		t.Fatalf("identity output = %d bytes, want %d", len(got), len(input))
	}
	if len(stream.Flush()) != 0 {
		t.Fatal("identity resampler flushed extra samples")
	}
}

func TestCanonicalizeWAVDownmixesResamplesAndNormalises(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.wav")
	writeTestWAV(t, source, 8000, 2, 16, sinePCM(8000, 2, 0.5, 0.2))
	output := filepath.Join(dir, "out.wav")

	duration, err := audio.Canonicalize(context.Background(), source, output, audio.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if duration < 490*time.Millisecond || duration > 510*time.Millisecond {
		t.Fatalf("duration = %s, want about 500ms", duration)
	}
	info, err := audio.InspectWAV(output)
	if err != nil {
		t.Fatalf("InspectWAV: %v", err)
	}
	if info.Format != (audio.Format{Rate: 16000, Width: 2, Channels: 1}) {
		t.Fatalf("canonical format = %+v, want 16000/2/1", info.Format)
	}
	decoded, err := audio.ReadWAV(output)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	var sumSquares float64
	var samples int
	for index := 0; index+1 < len(decoded.PCM); index += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(decoded.PCM[index:]))) / math.MaxInt16
		sumSquares += sample * sample
		samples++
	}
	rms := math.Sqrt(sumSquares / float64(samples))
	if rms < 0.11 || rms > 0.14 {
		t.Fatalf("normalised RMS = %.3f, want about 0.125", rms)
	}
}

func TestCanonicalizeMP3ToCanonicalWAV(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "tone.wav")
	duration, err := audio.Canonicalize(context.Background(), "testdata/tone.mp3", output, audio.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if duration < 550*time.Millisecond || duration > 660*time.Millisecond {
		t.Fatalf("duration = %s, want about 600ms", duration)
	}
	info, err := audio.InspectWAV(output)
	if err != nil {
		t.Fatalf("InspectWAV: %v", err)
	}
	if info.Format != (audio.Format{Rate: 16000, Width: 2, Channels: 1}) {
		t.Fatalf("canonical format = %+v", info.Format)
	}
}

func TestCanonicalizeRejectsAnUndecodableFileWithoutLeavingOutput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(source, []byte("this is not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out.wav")
	if _, err := audio.Canonicalize(context.Background(), source, output, audio.Format{Rate: 16000, Width: 2, Channels: 1}); err == nil {
		t.Fatal("Canonicalize accepted a text file")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after failure: %v", err)
	}
}

func TestCanonicalizeRejectsAnEmptyWAV(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "empty.wav")
	writeTestWAV(t, source, 16000, 1, 16, nil)
	if _, err := audio.Canonicalize(context.Background(), source, filepath.Join(dir, "out.wav"), audio.Format{Rate: 16000, Width: 2, Channels: 1}); err == nil {
		t.Fatal("Canonicalize accepted a WAV with no frames")
	}
}

func TestEncodeWAVProducesAReadableCanonicalFile(t *testing.T) {
	pcm := sinePCM(16000, 1, 0.1, 0.5)
	path := filepath.Join(t.TempDir(), "encoded.wav")
	if err := os.WriteFile(path, audio.EncodeWAV(pcm, audio.Format{Rate: 16000, Width: 2, Channels: 1}), 0o600); err != nil {
		t.Fatal(err)
	}
	decoded, err := audio.ReadWAV(path)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if len(decoded.PCM) != len(pcm) {
		t.Fatalf("decoded %d bytes, want %d", len(decoded.PCM), len(pcm))
	}
}
