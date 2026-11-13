package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// RecordSample taps three seconds of the configured 16 kHz mono PCM16 mic without routing speech.
func (l *Loop) RecordSample(ctx context.Context) ([]byte, error) {
	l.sampleMu.Lock()
	if l.sampling || l.inTurn {
		l.sampleMu.Unlock()
		return nil, errors.New("microphone busy; wait for the current turn or take to finish")
	}
	l.sampling = true
	l.sampleOverflow = false
	l.sampleMu.Unlock()
	defer func() {
		l.sampleMu.Lock()
		l.sampling, l.sampleFrames = false, nil
		l.sampleMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	// The quiet cue ends before capture so it cannot become a training example.
	if l.deps.Player != nil {
		cue := make([]byte, 1600)
		for i := 0; i < len(cue)/2; i++ {
			envelope := math.Sin(math.Pi * float64(i) / 800)
			binary.LittleEndian.PutUint16(cue[i*2:], uint16(int16(650*envelope*math.Sin(2*math.Pi*880*float64(i)/16000))))
		}
		if err := l.deps.Player.Play(ctx, cue, audio.Format{Rate: 16000, Width: 2, Channels: 1}); err != nil {
			return nil, err
		}
	}
	if !sleep(ctx, 150*time.Millisecond) {
		return nil, ctx.Err()
	}
	frames := make(chan []byte, 160)
	l.sampleMu.Lock()
	l.sampleFrames = frames
	l.sampleMu.Unlock()
	pcm := make([]byte, 0, 96000)
	for len(pcm) < 96000 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case frame := <-frames:
			pcm = append(pcm, frame...)
		}
	}
	l.sampleMu.Lock()
	overflow := l.sampleOverflow
	l.sampleMu.Unlock()
	if overflow {
		return nil, errors.New("sample capture overflow; redo the take")
	}
	return pcm[:96000], nil
}
