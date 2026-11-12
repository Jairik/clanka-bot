package speech

import (
	"bytes"
	"io"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// chunkWriter records each Write separately so a split-sample test can see the
// carry behaviour.
type chunkWriter struct {
	chunks [][]byte
	closed bool
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.chunks = append(w.chunks, append([]byte(nil), p...))
	return len(p), nil
}

func (w *chunkWriter) Close() error { w.closed = true; return nil }

func (w *chunkWriter) joined() []byte {
	var out []byte
	for _, chunk := range w.chunks {
		out = append(out, chunk...)
	}
	return out
}

func TestGainWriterCarriesASplitSampleAcrossWrites(t *testing.T) {
	inner := &chunkWriter{}
	writer := newGainWriter(inner, audio.Format{Rate: 16000, Width: 2, Channels: 1}, 50)
	source := []byte{0xe8, 0x03, 0x18, 0xfc} // 1000, -1000
	// Split the second sample across two Writes.
	if _, err := writer.Write(source[:3]); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := writer.Write(source[3:]); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := inner.joined(), []byte{0xf4, 0x01, 0x0c, 0xfe}; !bytes.Equal(got, want) {
		t.Fatalf("scaled bytes = %v, want %v", got, want)
	}
}

func TestGainWriterAtNormalVolumePassesThrough(t *testing.T) {
	inner := &chunkWriter{}
	writer := newGainWriter(inner, audio.Format{Rate: 16000, Width: 2, Channels: 1}, 100)
	if writer != io.WriteCloser(inner) {
		t.Fatal("normal volume did not return the original writer")
	}
}
