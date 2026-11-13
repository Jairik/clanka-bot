package main

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// wyomingTranscriber answers the Dashboard mic with the same STT service the
// voice loop uses. The browser already resampled to 16 kHz mono 16-bit, which
// is what the Wyoming audio-start announces.
type wyomingTranscriber struct {
	client *wyoming.Client
	opts   wyoming.TranscribeOptions
}

// micChunkBytes matches a 100 ms frame, so STT sees a normal-sized stream.
const micChunkBytes = 3200

func (t wyomingTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	stream, err := t.client.Transcribe(ctx, t.opts, wyoming.Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	for len(pcm) > 0 {
		n := min(micChunkBytes, len(pcm))
		if err := stream.Write(pcm[:n]); err != nil {
			return "", err
		}
		pcm = pcm[n:]
	}
	return stream.Finish()
}
