package wyoming

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// AudioStartData is the format every mic service expects: 16 kHz mono PCM16.
// Pulse resamples the source, so the services never see another layout.
func AudioStartData(rate, width, channels int) map[string]any {
	return map[string]any{"rate": rate, "width": width, "channels": channels}
}

// Detection is one wake word detection event.
type Detection struct {
	Name      string
	Timestamp int
}

// DetectStream is an open wake word detection stream. Write feeds mic audio and
// Next reports the result. Write and Next may run in different goroutines.
type DetectStream struct {
	conn        net.Conn
	reader      *bufio.Reader
	ctx         context.Context
	stopContext func() bool
	timeout     time.Duration
	format      Format

	writeMu sync.Mutex
	closed  bool
	closeMu sync.Mutex
}

// DetectOption adjusts one detect request.
type DetectOption func(map[string]any)

// WithDetectThresholds sends the per-model detection thresholds. The wakeword
// service applies them per model; a model without an entry keeps the service's
// global --threshold. The map is copied so a later config change cannot alter
// an in-flight stream.
func WithDetectThresholds(thresholds map[string]float64) DetectOption {
	return func(data map[string]any) {
		if len(thresholds) == 0 {
			return
		}
		copied := make(map[string]float64, len(thresholds))
		for name, threshold := range thresholds {
			copied[name] = threshold
		}
		data["thresholds"] = copied
	}
}

// Detect opens a detection stream and announces the audio format. names limits
// the models the service may fire; empty accepts any loaded model.
func (c *Client) Detect(ctx context.Context, names []string, format Format, opts ...DetectOption) (*DetectStream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &DetectStream{
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64<<10),
		ctx:     ctx,
		timeout: c.timeout,
		format:  format,
	}
	// Closing the connection on cancellation unblocks a pending read.
	s.stopContext = context.AfterFunc(ctx, func() { _ = conn.Close() })

	data := map[string]any{}
	if len(names) > 0 {
		data["names"] = names
	}
	for _, opt := range opts {
		if opt != nil {
			opt(data)
		}
	}
	if err := s.write("detect", data); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.write("audio-start", AudioStartData(format.Rate, format.Width, format.Channels)); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Write appends one chunk of mic audio to the detection stream.
func (s *DetectStream) Write(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return errors.New("wyoming: detect stream is closed")
	}
	// Wyoming audio chunks repeat the stream format; the services decode each
	// chunk on its own and error on a missing rate.
	return writeEvent(s.conn, s.timeout, "audio-chunk", AudioStartData(s.format.Rate, s.format.Width, s.format.Channels), pcm)
}

func (s *DetectStream) write(typ string, data map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return errors.New("wyoming: detect stream is closed")
	}
	return writeEvent(s.conn, s.timeout, typ, data, nil)
}

// Next waits for a detection or for the service to say the stream ended without
// one. The bool is true for a detection.
func (s *DetectStream) Next() (Detection, bool, error) {
	for {
		ev, err := readEvent(s.reader, 0)
		if err != nil {
			return Detection{}, false, s.mapError(err)
		}
		switch ev.Type {
		case "detection":
			name, _ := ev.Data["name"].(string)
			timestamp, _ := ev.Data["timestamp"].(float64)
			return Detection{Name: name, Timestamp: int(timestamp)}, true, nil
		case "not-detected":
			return Detection{}, false, nil
		case "error":
			return Detection{}, false, errorEvent(ev)
		}
	}
}

// Close ends the detection stream and releases the connection.
func (s *DetectStream) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stopContext != nil {
		s.stopContext()
	}
	return s.conn.Close()
}

func (s *DetectStream) mapError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if isClosed(err) {
		return fmt.Errorf("wyoming: detect stream closed: %w", err)
	}
	return fmt.Errorf("wyoming: read detection: %w", err)
}

// TranscribeOptions selects the language and model for one transcription.
type TranscribeOptions struct {
	Language string
	Name     string
}

// TranscriptStream is an open speech-to-text stream.
type TranscriptStream struct {
	conn        net.Conn
	reader      *bufio.Reader
	ctx         context.Context
	stopContext func() bool
	timeout     time.Duration
	format      Format

	writeMu  sync.Mutex
	closed   bool
	closeMu  sync.Mutex
	partials []string
}

// Transcribe opens a transcription stream and announces the audio format.
func (c *Client) Transcribe(ctx context.Context, opts TranscribeOptions, format Format) (*TranscriptStream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &TranscriptStream{
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64<<10),
		ctx:     ctx,
		timeout: c.timeout,
		format:  format,
	}
	s.stopContext = context.AfterFunc(ctx, func() { _ = conn.Close() })

	data := map[string]any{}
	if opts.Name != "" {
		data["name"] = opts.Name
	}
	if opts.Language != "" {
		data["language"] = opts.Language
	}
	if err := s.write("transcribe", data); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.write("audio-start", AudioStartData(format.Rate, format.Width, format.Channels)); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Write appends one chunk of mic audio to the transcription stream.
func (s *TranscriptStream) Write(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return errors.New("wyoming: transcript stream is closed")
	}
	return writeEvent(s.conn, s.timeout, "audio-chunk", AudioStartData(s.format.Rate, s.format.Width, s.format.Channels), pcm)
}

// Finish marks the end of speech and returns the final transcript. Partial
// transcript-chunk events are recorded but the service's final transcript wins.
func (s *TranscriptStream) Finish() (string, error) {
	s.writeMu.Lock()
	if s.closed {
		s.writeMu.Unlock()
		return "", errors.New("wyoming: transcript stream is closed")
	}
	err := writeEvent(s.conn, s.timeout, "audio-stop", nil, nil)
	s.writeMu.Unlock()
	if err != nil {
		return "", err
	}

	text := ""
	for {
		ev, err := readEvent(s.reader, 0)
		if err != nil {
			return "", s.mapError(err)
		}
		switch ev.Type {
		case "transcript":
			if value, ok := ev.Data["text"].(string); ok && value != "" {
				text = value
			}
		case "transcript-chunk":
			if value, ok := ev.Data["text"].(string); ok && value != "" {
				s.partials = append(s.partials, value)
			}
		case "transcript-stop":
			if text == "" && len(s.partials) > 0 {
				text = s.partials[len(s.partials)-1]
			}
			return text, nil
		case "error":
			return "", errorEvent(ev)
		}
	}
}

// Close ends the transcription stream and releases the connection.
func (s *TranscriptStream) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stopContext != nil {
		s.stopContext()
	}
	return s.conn.Close()
}

func (s *TranscriptStream) write(typ string, data map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return errors.New("wyoming: transcript stream is closed")
	}
	return writeEvent(s.conn, s.timeout, typ, data, nil)
}

func (s *TranscriptStream) mapError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if isClosed(err) {
		return fmt.Errorf("wyoming: transcript stream closed: %w", err)
	}
	return fmt.Errorf("wyoming: read transcript: %w", err)
}
