// Package api exposes the assistant's health and localhost debug endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const maxTranscriptBytes = 8 << 10

// Responder answers a transcript and speaks the reply. reply.Session satisfies
// it; the debug entry never speaks on its own.
type Responder interface {
	Ask(ctx context.Context, req tool.Request, transcript string) (pipeline.Reply, error)
}

// Announcer queues unprompted speech for the debug entry. announce.Queue
// satisfies it.
type Announcer interface {
	Enqueue(text string) bool
}

// Option configures the HTTP surface.
type Option func(*server)

// WithResponder routes the debug entry through the spoken reply session.
// Without it the debug entry is text-only, which is how tests and speechless
// dev runs behave.
func WithResponder(responder Responder) Option {
	return func(s *server) { s.responder = responder }
}

// WithAnnouncements lets the debug entry queue an Announcement, so the
// between-turns behaviour can be demoed without waiting for a Reminder. Without
// it the endpoint reports that speech is not configured.
func WithAnnouncements(announcer Announcer) Option {
	return func(s *server) { s.announcer = announcer }
}

// WithDebugAllowlist replaces the source-address check. It exists for tests
// and for a deployment whose bridge gateway is known another way. Production
// uses the default: loopback plus this container's default gateway (the host
// that publishes the loopback port), nothing else.
func WithDebugAllowlist(allowed func(net.IP) bool) Option {
	return func(s *server) { s.debugAllowed = allowed }
}

// WithSettings mounts the settings and Tool-override API under /api/ on the
// debug handler. It is ignored when the debug entry is disabled, so the public
// health listener can never serve settings writes. The mounted handler has no
// authentication of its own; the debug source allowlist guards it here, and
// #47 will mount the same handler behind Tailscale identity.
func WithSettings(handler http.Handler) Option {
	return func(s *server) { s.settings = handler }
}

// NewHandler builds the HTTP surface: GET /healthz, plus POST
// /debug/transcript when the debug entry is enabled.
func NewHandler(p *pipeline.Pipeline, logger *slog.Logger, debugEnabled bool, opts ...Option) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{pipeline: p, logger: logger, debugEnabled: debugEnabled, debugAllowed: defaultDebugAllowlist}
	for _, opt := range opts {
		opt(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/debug/transcript", s.debugTranscript)
	mux.HandleFunc("/debug/announce", s.debugAnnounce)
	if debugEnabled && s.settings != nil {
		// Settings writes are local-only: the same source check as the debug
		// entry wraps the whole /api/ subtree.
		mux.Handle("/api/", s.localOnly(http.StripPrefix("/api", s.settings)))
	}
	return mux
}

type server struct {
	pipeline     *pipeline.Pipeline
	logger       *slog.Logger
	debugEnabled bool
	responder    Responder
	settings     http.Handler
	announcer    Announcer
	debugAllowed func(net.IP) bool
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type transcriptRequest struct {
	Text string `json:"text"`
}

type transcriptResponse struct {
	Reply     string `json:"reply"`
	Tool      string `json:"tool"`
	Path      string `json:"path"`
	RequestID string `json:"request_id"`
	Spoken    bool   `json:"spoken"`
}

func (s *server) debugTranscript(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The debug entry drives act Tools and provider spend. Reject sibling
	// containers by source address; do not trust X-Forwarded-For, which is
	// caller-controlled. Host loopback reaches the container through Docker's
	// published port from the bridge gateway, so that one address is allowed.
	if !s.debugSourceAllowed(r.RemoteAddr) {
		s.reject(w, tool.NewRequestID(), "debug entry is local only", http.StatusForbidden)
		return
	}

	requestID := tool.NewRequestID()
	w.Header().Set("X-Request-ID", requestID)

	var body transcriptRequest
	if !decodeJSON(w, r, &body) {
		s.reject(w, requestID, "invalid JSON body", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		s.reject(w, requestID, "text is required", http.StatusBadRequest)
		return
	}

	req := tool.Request{
		ID:     requestID,
		Path:   tool.PathFallthrough,
		Device: tool.DevicePi,
	}
	// The debug entry has no endpointing or STT, so its timeline never
	// reports Response latency; routing and playback stages still correlate.
	timeline := latency.New(requestID, latency.SourceDebug)
	requestCtx := latency.WithContext(r.Context(), timeline)

	// Speaking is part of the reply, so the request waits for playback to
	// finish. A TTS failure still returns the text reply.
	var reply pipeline.Reply
	spoken := false
	if s.responder != nil {
		var err error
		reply, err = s.responder.Ask(requestCtx, req, text)
		if err != nil {
			s.logger.Error("speech",
				"request_id", requestID,
				"path", "debug_entry",
				"error", err.Error(),
			)
		} else {
			spoken = true
		}
	} else {
		reply = s.pipeline.Handle(requestCtx, req, text)
		// Nothing was spoken, so the request line is completed here.
		latency.Complete(requestCtx, s.logger, latency.OutcomeNoAudio, nil)
	}

	writeJSON(w, http.StatusOK, transcriptResponse{
		Reply:     reply.Text,
		Tool:      reply.Tool,
		Path:      string(reply.Path),
		RequestID: reply.RequestID,
		Spoken:    spoken,
	})
}

// localOnly wraps the settings subtree in the same source check as the debug
// entry. The settings handler performs no authentication itself.
func (s *server) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.debugSourceAllowed(r.RemoteAddr) {
			s.reject(w, tool.NewRequestID(), "settings entry is local only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type announceRequest struct {
	Text string `json:"text"`
}

// debugAnnounce queues unprompted speech on the shared Announcement queue. It
// shares the transcript entry's debug gate: disabled means absent, and only the
// host loopback or its bridge gateway may post.
func (s *server) debugAnnounce(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.debugSourceAllowed(r.RemoteAddr) {
		s.reject(w, tool.NewRequestID(), "debug entry is local only", http.StatusForbidden)
		return
	}
	if s.announcer == nil {
		s.reject(w, tool.NewRequestID(), "speech is not configured", http.StatusServiceUnavailable)
		return
	}

	var body announceRequest
	if !decodeJSON(w, r, &body) {
		s.reject(w, tool.NewRequestID(), "invalid JSON body", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		s.reject(w, tool.NewRequestID(), "text is required", http.StatusBadRequest)
		return
	}
	if !s.announcer.Enqueue(text) {
		s.reject(w, tool.NewRequestID(), "announcements are unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// decodeJSON reads one bounded JSON object and rejects trailing data.
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTranscriptBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func (s *server) debugSourceAllowed(remoteAddr string) bool {
	host := remoteAddr
	if parsed, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = parsed
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	if s.debugAllowed == nil {
		return ip.IsLoopback()
	}
	return s.debugAllowed(ip)
}

// defaultDebugAllowlist accepts loopback and this container's default gateway.
// Compose publishes 127.0.0.1:8081 on the host; Docker's proxy reaches the
// container from the bridge gateway, while a sibling container arrives from
// its own address and is refused. The gateway is read from the route table
// once, and a missing gateway fails closed (loopback only).
func defaultDebugAllowlist(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	gateway := containerGateway()
	return gateway != nil && ip.Equal(gateway)
}

var (
	gatewayOnce sync.Once
	gatewayIP   net.IP
)

func containerGateway() net.IP {
	gatewayOnce.Do(func() {
		raw, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[1] != "00000000" {
				continue
			}
			// The Gateway column is little-endian hex.
			value, err := strconv.ParseUint(fields[2], 16, 32)
			if err != nil {
				continue
			}
			gatewayIP = net.IPv4(byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
			return
		}
	})
	return gatewayIP
}

func (s *server) reject(w http.ResponseWriter, requestID, message string, status int) {
	s.logger.Warn("request",
		"request_id", requestID,
		"path", "debug_entry",
		"status", status,
		"error", message,
	)
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
