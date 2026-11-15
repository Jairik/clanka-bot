// Package wyoming is a small client for the Wyoming voice protocol over TCP.
// It speaks JSONL headers plus raw PCM payloads, and only the parts the
// assistant needs: describe, one-shot synthesis and streaming synthesis.
package wyoming

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	defaultTimeout     = 30 * time.Second
	defaultReadTimeout = 60 * time.Second
)

// Format is the PCM layout announced by an audio-start event.
type Format struct {
	Rate     int
	Width    int
	Channels int
}

// Voice selects a voice for synthesis. Empty fields are omitted on the wire.
type Voice struct {
	Name     string
	Language string
	Speaker  string
}

// VoiceInfo is one voice advertised in an info response.
type VoiceInfo struct {
	Name      string   `json:"name"`
	Languages []string `json:"languages"`
}

// Program is one TTS or wake-word program advertised in an info response.
type Program struct {
	Name                        string      `json:"name"`
	Description                 string      `json:"description"`
	Version                     string      `json:"version"`
	Installed                   bool        `json:"installed"`
	Languages                   []string    `json:"languages"`
	Voices                      []VoiceInfo `json:"voices"`
	Models                      []WakeModel `json:"models"`
	SupportsSynthesizeStreaming bool        `json:"supports_synthesize_streaming"`
}

// WakeModel is one wake-word model an info response advertises. The upstream
// openWakeWord service lists every model it can serve, not only the detectors a
// connection has loaded; loading happens on the first detect for a name.
type WakeModel struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Phrase      string   `json:"phrase"`
	Installed   bool     `json:"installed"`
	Languages   []string `json:"languages"`
	Version     string   `json:"version"`
}

// Info is the parsed describe response.
type Info struct {
	TTS  []Program `json:"tts"`
	Wake []Program `json:"wake"`
}

// Client talks to one Wyoming service address.
type Client struct {
	addr        string
	timeout     time.Duration
	readTimeout time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets the dial, write and read-idle timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
			c.readTimeout = d
		}
	}
}

// NewClient builds a client for a host:port Wyoming service.
func NewClient(addr string, opts ...Option) *Client {
	c := &Client{addr: addr, timeout: defaultTimeout, readTimeout: defaultReadTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ServerError is an error event sent by the service.
type ServerError struct {
	Text string
	Code string
}

// ErrBusy is the refusal a serialising speech-to-text service sends when
// another connection already holds its model: a first-class answer instead of
// a read timeout. stt_server.py words it as code "busy" (#196).
var ErrBusy = errors.New("wyoming: speech recognition is busy")

func (e *ServerError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("wyoming: %s (%s)", e.Text, e.Code)
	}
	return "wyoming: " + e.Text
}

// Is reports the busy refusal to errors.Is, so a caller can tell "try again in
// a moment" from a model failure without matching the service's wording.
func (e *ServerError) Is(target error) bool {
	return target == ErrBusy && e.Code == "busy"
}

// Describe asks the service what it can do. It is the readiness check.
func (c *Client) Describe(ctx context.Context) (Info, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Info{}, err
	}
	defer conn.Close()

	if err := writeEvent(conn, c.timeout, "describe", nil, nil); err != nil {
		return Info{}, err
	}
	ev, err := readEvent(conn, c.readTimeout)
	if err != nil {
		return Info{}, err
	}
	switch ev.Type {
	case "info":
		var info Info
		data, err := json.Marshal(ev.Data)
		if err != nil {
			return Info{}, fmt.Errorf("wyoming: encode info: %w", err)
		}
		if err := json.Unmarshal(data, &info); err != nil {
			return Info{}, fmt.Errorf("wyoming: decode info: %w", err)
		}
		return info, nil
	case "error":
		return Info{}, errorEvent(ev)
	default:
		return Info{}, fmt.Errorf("wyoming: describe got %q, want info", ev.Type)
	}
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	dialer := net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("wyoming: connect %s: %w", c.addr, err)
	}
	return conn, nil
}

// errorEvent converts an error event into a ServerError.
func errorEvent(ev event) error {
	text, _ := ev.Data["text"].(string)
	if text == "" {
		text = "service error"
	}
	code, _ := ev.Data["code"].(string)
	return &ServerError{Text: text, Code: code}
}

// isClosed reports whether an error is just a closed or cancelled connection.
func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isTimeout reports whether an error is an expired read or write deadline.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}
