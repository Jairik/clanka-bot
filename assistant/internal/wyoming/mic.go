package wyoming

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
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
	// closed is atomic so Close can unblock a concurrent Write without racing it.
	closed atomic.Bool
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
	if s.closed.Load() {
		return errors.New("wyoming: detect stream is closed")
	}
	// Wyoming audio chunks repeat the stream format; the services decode each
	// chunk on its own and error on a missing rate.
	return writeEvent(s.conn, s.timeout, "audio-chunk", AudioStartData(s.format.Rate, s.format.Width, s.format.Channels), pcm)
}

func (s *DetectStream) write(typ string, data map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return errors.New("wyoming: detect stream is closed")
	}
	return writeEvent(s.conn, s.timeout, typ, data, nil)
}

// Next waits for a detection or for the service to say the stream ended without
// one. The bool is true for a detection. There is deliberately no read
// deadline: a wake stream is legitimately silent for minutes, so a timeout here
// would reopen a healthy stream (see #201).
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

// Close ends the detection stream and releases the connection. The atomic swap
// makes it idempotent without holding writeMu, so Close can still unblock a
// concurrent Write or a pending Next.
func (s *DetectStream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
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
	readTimeout time.Duration
	format      Format

	writeMu sync.Mutex
	// closed is atomic so Close can unblock a concurrent Write or Finish
	// without racing it.
	closed atomic.Bool
	// partials is only touched by the background reader; Finish reads it once
	// the reader has delivered a transcript-stop, so the access is ordered by
	// recvMu.
	partials []string

	// refusalDeadline bounds the early read that looks for an immediate busy
	// refusal right after the stream opens (#368). Zero disables the probe.
	refusalDeadline time.Duration
	// finalisationBudget is the total time Finish may keep reading after
	// audio-stop before giving up on a babbling or stuck service (#340).
	finalisationBudget time.Duration

	// errCh hands the first server refusal (for example a busy error event) to
	// the caller while audio is still streaming, so the voice loop can stop
	// early instead of writing the whole utterance into a refused session
	// (#368). It is buffered and delivers at most one refusal.
	errCh chan error
	// reader owns every read from the connection so the caller can learn about
	// a refusal mid-utterance. It buffers transcript events for Finish and
	// surfaces a refusal on errCh; because it is the only reader, Finish never
	// reads the same bytes and needs no read deadline of its own.
	recvMu     sync.Mutex
	recv       []event
	finalErr   error
	recvNotify chan struct{}
	readerDone chan struct{}
}

// maxTranscriptChunks bounds the partial buffer. Only the last partial is ever
// used as a fallback, so keeping the most recent ones bounds memory without
// losing the fallback when a babbling service never sends a final transcript
// (#340).
const maxTranscriptChunks = 64

// Transcribe opens a transcription stream and announces the audio format. A
// background reader then owns every read from the connection: it buffers the
// final transcript for Finish and surfaces a busy refusal on Refused while audio
// is still streaming, so the voice loop can stop early instead of writing the
// whole utterance into a dead session (#368).
//
// As a fast path, Transcribe also probes once for an immediate busy refusal: a
// serialising service that answers a second session before any audio arrives is
// caught here and returned at once. A healthy session answers nothing before
// audio-stop, so the probe simply times out and the stream is returned as usual;
// a refusal that only arrives after the model is released is still caught by the
// reader during streaming.
func (c *Client) Transcribe(ctx context.Context, opts TranscribeOptions, format Format) (*TranscriptStream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	budget := c.finalisationBudget
	if budget <= 0 {
		budget = 3 * c.readTimeout
		if budget < 60*time.Second {
			budget = 60 * time.Second
		}
	}
	s := &TranscriptStream{
		conn:               conn,
		reader:             bufio.NewReaderSize(conn, 64<<10),
		ctx:                ctx,
		timeout:            c.timeout,
		readTimeout:        c.readTimeout,
		format:             format,
		refusalDeadline:    c.refusalDeadline,
		finalisationBudget: budget,
		errCh:              make(chan error, 1),
		recvNotify:         make(chan struct{}, 1),
		readerDone:         make(chan struct{}),
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

	// The reader owns every read from here on. It delivers a busy refusal while
	// audio is still streaming and buffers the final transcript for Finish.
	go s.readLoop()

	// Fast path: some serialising services refuse a second session before any
	// audio arrives. A short probe catches that immediately; the reader catches
	// the refusal that only arrives after the model is released.
	if s.refusalDeadline > 0 {
		select {
		case err := <-s.errCh:
			s.Close()
			return nil, err
		case <-time.After(s.refusalDeadline):
			// Healthy so far; keep streaming, the reader stays live.
		}
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
	if s.closed.Load() {
		return errors.New("wyoming: transcript stream is closed")
	}
	return writeEvent(s.conn, s.timeout, "audio-chunk", AudioStartData(s.format.Rate, s.format.Width, s.format.Channels), pcm)
}

// Finish marks the end of speech and returns the final transcript. It writes
// audio-stop and then drains the events the background reader has buffered,
// applying the partial cap and the total finalisation budget from #340. A busy
// refusal the reader already surfaced on Refused is still returned here if the
// caller reaches Finish first.
func (s *TranscriptStream) Finish() (string, error) {
	s.writeMu.Lock()
	if s.closed.Load() {
		s.writeMu.Unlock()
		return "", errors.New("wyoming: transcript stream is closed")
	}
	err := writeEvent(s.conn, s.timeout, "audio-stop", nil, nil)
	s.writeMu.Unlock()
	if err != nil {
		return "", err
	}

	deadline := time.Now().Add(s.finalisationBudget)
	text := ""
	idx := 0
	for {
		// Drain every event the reader has buffered so far, in order.
		s.recvMu.Lock()
		for ; idx < len(s.recv); idx++ {
			ev := s.recv[idx]
			switch ev.Type {
			case "transcript":
				if value, ok := ev.Data["text"].(string); ok && value != "" {
					text = value
				}
			case "transcript-stop":
				if text == "" && len(s.partials) > 0 {
					text = s.partials[len(s.partials)-1]
				}
				s.recvMu.Unlock()
				return text, nil
			}
		}
		if s.finalErr != nil {
			err := s.finalErr
			s.recvMu.Unlock()
			return "", err
		}
		s.recvMu.Unlock()

		// No decision yet: wait for the reader to land an event, the stream to
		// end, or the total budget to run out.
		if err := s.ctx.Err(); err != nil {
			return "", err
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			return "", fmt.Errorf("wyoming: transcribe: no final transcript within %s of audio-stop: %w", s.finalisationBudget, os.ErrDeadlineExceeded)
		}
		// The reader notify()s once more as it exits, so a session that ends
		// without a transcript still wakes Finish; otherwise we wait out the
		// total budget as a hard cap.
		select {
		case <-s.recvNotify:
		case <-s.ctx.Done():
		case <-time.After(time.Until(deadline)):
		}
	}
}

// readLoop owns every read from the connection for the lifetime of the stream. It
// buffers transcript events for Finish and hands the first server refusal to
// Refused so the caller can abort streaming early (#368). Because it is the only
// reader, Finish never contends with it for bytes and needs no read deadline.
func (s *TranscriptStream) readLoop() {
	defer func() {
		s.notify()
		close(s.readerDone)
	}()
	for {
		if s.closed.Load() {
			return
		}
		ev, err := readEvent(s.reader, 0)
		if err != nil {
			s.recvMu.Lock()
			if s.finalErr == nil {
				s.finalErr = s.mapError(err)
			}
			s.recvMu.Unlock()
			return
		}
		switch ev.Type {
		case "transcript":
			s.recvMu.Lock()
			s.recv = append(s.recv, ev)
			s.recvMu.Unlock()
			s.notify()
		case "transcript-chunk":
			if value, ok := ev.Data["text"].(string); ok && value != "" {
				s.recvMu.Lock()
				s.partials = append(s.partials, value)
				if len(s.partials) > maxTranscriptChunks {
					s.partials = s.partials[len(s.partials)-maxTranscriptChunks:]
				}
				s.recvMu.Unlock()
			}
		case "transcript-stop":
			s.recvMu.Lock()
			s.recv = append(s.recv, ev)
			s.recvMu.Unlock()
			s.notify()
		case "error":
			serverErr := errorEvent(ev)
			s.recvMu.Lock()
			if s.finalErr == nil {
				s.finalErr = serverErr
			}
			s.recvMu.Unlock()
			// Hand the refusal to the caller for early abort. A buffered channel
			// delivers at most one refusal without blocking the reader.
			select {
			case s.errCh <- serverErr:
			default:
			}
			return
		default:
			// Ignore other event types (for example an unexpected echo).
		}
	}
}

// notify wakes a blocked Finish. recvNotify is buffered so the send never blocks
// the reader, and Finish drains every buffered event on each wake, so a dropped
// notify cannot lose data.
func (s *TranscriptStream) notify() {
	select {
	case s.recvNotify <- struct{}{}:
	default:
	}
}

// Refused reports a server refusal (for example a busy error event) seen while
// the stream is open, so the caller can stop streaming and answer the user
// instead of writing the whole utterance into a refused session (#368). It is
// buffered and delivers at most one refusal; a nil read means none arrived.
func (s *TranscriptStream) Refused() <-chan error { return s.errCh }

// Close ends the transcription stream and releases the connection. The atomic
// swap makes it idempotent without holding writeMu, so Close can still unblock
// a concurrent Write or a pending Finish.
func (s *TranscriptStream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.stopContext != nil {
		s.stopContext()
	}
	return s.conn.Close()
}

func (s *TranscriptStream) write(typ string, data map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return errors.New("wyoming: transcript stream is closed")
	}
	return writeEvent(s.conn, s.timeout, typ, data, nil)
}

func (s *TranscriptStream) mapError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if isTimeout(err) && s.readTimeout > 0 {
		return fmt.Errorf("wyoming: transcribe: no final transcript within %s: %w", s.readTimeout, err)
	}
	if isClosed(err) {
		return fmt.Errorf("wyoming: transcript stream closed: %w", err)
	}
	return fmt.Errorf("wyoming: read transcript: %w", err)
}
