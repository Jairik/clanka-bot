package wyoming

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// VADStream is an open frame-by-frame voice activity detection stream against
// the STT service's Silero VAD endpoint. Each Score call sends one audio-chunk
// and returns the model's speech probability in [0, 1].
type VADStream struct {
	conn        net.Conn
	reader      *bufio.Reader
	ctx         context.Context
	stopContext func() bool
	timeout     time.Duration
	readTimeout time.Duration
	format      Format

	mu     sync.Mutex
	closed atomic.Bool
}

// OpenVAD starts a Silero VAD session on the Wyoming STT service and waits for
// the vad-ready handshake. A service without VAD answers with a structured
// ServerError (code "vad-unavailable"), letting the caller fall back to RMS.
func (c *Client) OpenVAD(ctx context.Context, format Format) (*VADStream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &VADStream{
		conn:        conn,
		reader:      bufio.NewReaderSize(conn, 16<<10),
		ctx:         ctx,
		timeout:     c.timeout,
		readTimeout: c.readTimeout,
		format:      format,
	}
	s.stopContext = context.AfterFunc(ctx, func() { _ = conn.Close() })

	if err := writeEvent(s.conn, s.timeout, "vad-start", AudioStartData(format.Rate, format.Width, format.Channels), nil); err != nil {
		s.Close()
		return nil, err
	}
	ev, err := readEvent(s.reader, s.readTimeout)
	if err != nil {
		s.Close()
		return nil, s.mapError(err)
	}
	switch ev.Type {
	case "vad-ready":
		return s, nil
	case "error":
		s.Close()
		return nil, errorEvent(ev)
	default:
		s.Close()
		return nil, fmt.Errorf("wyoming: vad-start got %q, want vad-ready", ev.Type)
	}
}

// Score sends one PCM frame and returns the Silero VAD speech probability.
func (s *VADStream) Score(pcm []byte) (float64, error) {
	if len(pcm) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return 0, errors.New("wyoming: vad stream is closed")
	}
	if err := writeEvent(s.conn, s.timeout, "audio-chunk", AudioStartData(s.format.Rate, s.format.Width, s.format.Channels), pcm); err != nil {
		return 0, s.mapError(err)
	}
	ev, err := readEvent(s.reader, s.readTimeout)
	if err != nil {
		return 0, s.mapError(err)
	}
	switch ev.Type {
	case "vad-score":
		prob, ok := ev.Data["probability"].(float64)
		if !ok {
			return 0, errors.New("wyoming: vad-score missing numeric probability")
		}
		return prob, nil
	case "error":
		return 0, errorEvent(ev)
	default:
		return 0, fmt.Errorf("wyoming: vad score got %q, want vad-score", ev.Type)
	}
}

// Close ends the VAD session and releases the TCP connection.
func (s *VADStream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.stopContext != nil {
		s.stopContext()
	}
	return s.conn.Close()
}

func (s *VADStream) mapError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if isClosed(err) {
		return fmt.Errorf("wyoming: vad stream closed: %w", err)
	}
	return fmt.Errorf("wyoming: vad: %w", err)
}
