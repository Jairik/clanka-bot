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

// Option configures the HTTP surface.
type Option func(*server)

// WithResponder routes the debug entry through the spoken reply session.
// Without it the debug entry is text-only, which is how tests and speechless
// dev runs behave.
func WithResponder(responder Responder) Option {
	return func(s *server) { s.responder = responder }
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
	responder    Responder
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
