package wyoming

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	maxHeaderBytes  = 1 << 20
	maxPayloadBytes = 32 << 20
)

// event is one decoded Wyoming frame.
type event struct {
	Type string
	Data map[string]any
}

type wireEvent struct {
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data,omitempty"`
	Version       string          `json:"version,omitempty"`
	DataLength    int             `json:"data_length,omitempty"`
	PayloadLength int             `json:"payload_length,omitempty"`
}

// writeEvent writes one header line and optional payload. Data is written
// inline, which the protocol allows alongside the extra-data form.
func writeEvent(w io.Writer, timeout time.Duration, typ string, data map[string]any, payload []byte) error {
	if conn, ok := w.(net.Conn); ok && timeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	header := map[string]any{"type": typ}
	if len(data) > 0 {
		header["data"] = data
	}
	if len(payload) > 0 {
		header["payload_length"] = len(payload)
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("wyoming: encode %s: %w", typ, err)
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("wyoming: write %s: %w", typ, err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("wyoming: write %s payload: %w", typ, err)
		}
	}
	return nil
}

// readEvent reads one header, merges any extra data block, and reads the payload.
func readEvent(r io.Reader, timeout time.Duration) (event, error) {
	if conn, ok := r.(net.Conn); ok && timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 64<<10)
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		return event{}, err
	}
	if len(line) > maxHeaderBytes {
		return event{}, fmt.Errorf("wyoming: header too large (%d bytes)", len(line))
	}
	var we wireEvent
	if err := json.Unmarshal(line, &we); err != nil {
		return event{}, fmt.Errorf("wyoming: decode header: %w", err)
	}
	if we.Type == "" {
		return event{}, errors.New("wyoming: event has no type")
	}

	data := map[string]any{}
	if len(we.Data) > 0 {
		if err := json.Unmarshal(we.Data, &data); err != nil {
			return event{}, fmt.Errorf("wyoming: decode data: %w", err)
		}
	}
	if we.DataLength > 0 {
		if we.DataLength > maxHeaderBytes {
			return event{}, fmt.Errorf("wyoming: extra data too large (%d bytes)", we.DataLength)
		}
		extra := make([]byte, we.DataLength)
		if _, err := io.ReadFull(br, extra); err != nil {
			return event{}, fmt.Errorf("wyoming: read extra data: %w", err)
		}
		var merged map[string]any
		if err := json.Unmarshal(extra, &merged); err != nil {
			return event{}, fmt.Errorf("wyoming: decode extra data: %w", err)
		}
		for k, v := range merged {
			data[k] = v
		}
	}
	if we.PayloadLength > 0 {
		if we.PayloadLength > maxPayloadBytes {
			return event{}, fmt.Errorf("wyoming: payload too large (%d bytes)", we.PayloadLength)
		}
		payload := make([]byte, we.PayloadLength)
		if _, err := io.ReadFull(br, payload); err != nil {
			return event{}, fmt.Errorf("wyoming: read payload: %w", err)
		}
		return event{Type: we.Type, Data: withPayload(data, payload)}, nil
	}
	return event{Type: we.Type, Data: data}, nil
}

// payloadKey keeps the binary payload alongside decoded event data.
const payloadKey = "\x00payload"

func withPayload(data map[string]any, payload []byte) map[string]any {
	data[payloadKey] = payload
	return data
}

func payloadOf(ev event) []byte {
	payload, _ := ev.Data[payloadKey].([]byte)
	return payload
}

// streamMode says whether the stream carries one synthesis or streamed text.
type streamMode int

const (
	modeOnce streamMode = iota
	modeText
)

// Stream is a synthesis in progress. Audio is available chunk by chunk. It is
// safe for one goroutine to call Next while another calls SendText.
type Stream struct {
	conn        net.Conn
	reader      *bufio.Reader
	ctx         context.Context
	stopContext func() bool
	timeout     time.Duration
	readTimeout time.Duration
	mode        streamMode

	// Format is the latest audio format announced by the service.
	Format Format

	writeMu sync.Mutex
	queue   [][]byte
	closed  bool
	closeMu sync.Mutex
}

// Synthesize sends one piece of text and returns the audio stream. It waits for
// the first audio-start so Format is known before playback opens. Options carry
// the live speech rate; the voice still names the library entry.
func (c *Client) Synthesize(ctx context.Context, text string, voice Voice, opts ...SynthesisOption) (*Stream, error) {
	if text == "" {
		return nil, errors.New("wyoming: text is required")
	}
	s, err := c.open(ctx, modeOnce)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"text": text}
	addVoice(data, voice)
	addRate(data, opts)
	if err := s.write("synthesize", data); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.awaitAudioStart(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// OpenStream starts a streaming synthesis. Call SendText as sentences are
// ready, EndText when the text is complete, and Next for audio as it arrives.
// The rate is fixed for the whole stream, like the voice.
func (c *Client) OpenStream(ctx context.Context, voice Voice, opts ...SynthesisOption) (*Stream, error) {
	s, err := c.open(ctx, modeText)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	addVoice(data, voice)
	addRate(data, opts)
	if err := s.write("synthesize-start", data); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func addVoice(data map[string]any, voice Voice) {
	fields := map[string]any{}
	if voice.Name != "" {
		fields["name"] = voice.Name
	}
	if voice.Language != "" {
		fields["language"] = voice.Language
	}
	if voice.Speaker != "" {
		fields["speaker"] = voice.Speaker
	}
	if len(fields) > 0 {
		data["voice"] = fields
	}
}

func (c *Client) open(ctx context.Context, mode streamMode) (*Stream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &Stream{
		conn:        conn,
		reader:      bufio.NewReaderSize(conn, 64<<10),
		ctx:         ctx,
		timeout:     c.timeout,
		readTimeout: c.readTimeout,
		mode:        mode,
	}
	// Closing the connection on cancellation unblocks a pending read.
	s.stopContext = context.AfterFunc(ctx, func() { _ = conn.Close() })
	return s, nil
}

// SendText appends a text chunk to a streaming synthesis.
func (s *Stream) SendText(text string) error {
	if text == "" {
		return nil
	}
	return s.write("synthesize-chunk", map[string]any{"text": text})
}

// EndText marks the end of a streaming synthesis's text.
func (s *Stream) EndText() error {
	return s.write("synthesize-stop", nil)
}

func (s *Stream) write(typ string, data map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return errors.New("wyoming: stream is closed")
	}
	return writeEvent(s.conn, s.timeout, typ, data, nil)
}

// awaitAudioStart reads until the first audio-start, buffering any chunks that
// arrive early.
func (s *Stream) awaitAudioStart() error {
	for {
		ev, err := s.readEvent()
		if err != nil {
			return s.mapError(err)
		}
		switch ev.Type {
		case "audio-start":
			s.setFormat(ev.Data)
			return nil
		case "audio-chunk":
			if payload := payloadOf(ev); len(payload) > 0 {
				s.queue = append(s.queue, payload)
			}
		case "error":
			return errorEvent(ev)
		}
	}
}

// Next returns the next audio chunk, blocking until it arrives. It returns
// io.EOF when the service signals the end of audio.
func (s *Stream) Next() ([]byte, error) {
	if len(s.queue) > 0 {
		chunk := s.queue[0]
		s.queue = s.queue[1:]
		return chunk, nil
	}
	for {
		ev, err := s.readEvent()
		if err != nil {
			return nil, s.mapError(err)
		}
		switch ev.Type {
		case "audio-start":
			s.setFormat(ev.Data)
		case "audio-chunk":
			if payload := payloadOf(ev); len(payload) > 0 {
				return payload, nil
			}
		case "audio-stop":
			// One-shot synthesis is done; a text stream may have more segments.
			if s.mode == modeOnce {
				return nil, io.EOF
			}
		case "synthesize-stopped":
			return nil, io.EOF
		case "error":
			return nil, errorEvent(ev)
		}
	}
}

func (s *Stream) setFormat(data map[string]any) {
	rate, _ := data["rate"].(float64)
	width, _ := data["width"].(float64)
	channels, _ := data["channels"].(float64)
	s.Format = Format{Rate: int(rate), Width: int(width), Channels: int(channels)}
}

// readEvent applies the read-idle deadline, then decodes one event.
func (s *Stream) readEvent() (event, error) {
	if s.readTimeout > 0 {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.readTimeout))
	}
	return readEvent(s.reader, 0)
}

// Close ends the stream and releases the connection.
func (s *Stream) Close() error {
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

// mapError turns a cancelled context into the context error callers expect.
func (s *Stream) mapError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if isClosed(err) {
		return fmt.Errorf("wyoming: stream closed: %w", err)
	}
	return fmt.Errorf("wyoming: read event: %w", err)
}
