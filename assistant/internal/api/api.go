// Package api exposes the assistant's health and localhost debug endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const maxTranscriptBytes = 8 << 10

// Sayer speaks reply text. speech.Speaker satisfies it through SayText.
type Sayer interface {
	SayText(ctx context.Context, text string) error
}

// Option configures the HTTP surface.
type Option func(*server)

// WithSpeech plays the reply through sayer. Without it the debug entry is
// text-only, which is how tests and speechless dev runs behave.
func WithSpeech(sayer Sayer) Option {
	return func(s *server) { s.sayer = sayer }
}

// NewHandler builds the HTTP surface: GET /healthz, plus POST
// /debug/transcript when the debug entry is enabled.
func NewHandler(p *pipeline.Pipeline, logger *slog.Logger, debugEnabled bool, opts ...Option) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{pipeline: p, logger: logger, debugEnabled: debugEnabled}
	for _, opt := range opts {
		opt(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/debug/transcript", s.debugTranscript)
	return mux
}

type server struct {
	pipeline     *pipeline.Pipeline
	logger       *slog.Logger
	debugEnabled bool
	sayer        Sayer
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

	requestID := tool.NewRequestID()
	w.Header().Set("X-Request-ID", requestID)

	var body transcriptRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTranscriptBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		s.reject(w, requestID, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.reject(w, requestID, "invalid JSON body", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		s.reject(w, requestID, "text is required", http.StatusBadRequest)
		return
	}

	reply := s.pipeline.Handle(r.Context(), tool.Request{
		ID:     requestID,
		Path:   tool.PathFallthrough,
		Device: tool.DevicePi,
	}, text)

	// Speaking is part of the reply, so the request waits for playback to
	// finish. A TTS failure still returns the text reply.
	spoken := false
	if s.sayer != nil {
		if err := s.sayer.SayText(r.Context(), reply.Text); err != nil {
			s.logger.Error("speech",
				"request_id", requestID,
				"path", "debug_entry",
				"error", err.Error(),
			)
		} else {
			spoken = true
		}
	}

	writeJSON(w, http.StatusOK, transcriptResponse{
		Reply:     reply.Text,
		Tool:      reply.Tool,
		Path:      string(reply.Path),
		RequestID: reply.RequestID,
		Spoken:    spoken,
	})
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
