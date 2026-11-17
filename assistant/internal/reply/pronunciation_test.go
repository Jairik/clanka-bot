package reply_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

type discardSink struct{}

func (discardSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return discardWriter{}, nil
}

type fakeSearxIn struct{}
type fakeSearxOut struct{ Text string }

// TestPronunciationReplacesSynthesizedTextWhilePreservingReplyActivityAndLastReply
// proves that the live pronunciation substitution is speech-only:
// Wyoming TTS receives the substituted text, while Reply.Text, the Activity
// record, and the last-reply store retain the original wording.
func TestPronunciationReplacesSynthesizedTextWhilePreservingReplyActivityAndLastReply(t *testing.T) {
	requests := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		var req struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		if text, ok := req.Data["text"].(string); ok {
			requests <- text
		}
		conn.Write([]byte("{\"type\":\"audio-start\",\"data\":{\"rate\":24000,\"width\":2,\"channels\":1}}\n"))
		conn.Write([]byte("{\"type\":\"audio-chunk\",\"data\":{\"rate\":24000,\"width\":2,\"channels\":1},\"payload_length\":2}\n"))
		conn.Write([]byte{1, 2})
		conn.Write([]byte("{\"type\":\"audio-stop\"}\n"))
	}()

	reg := tool.NewRegistry(time.Second)
	searxTool := tool.Tool[fakeSearxIn, fakeSearxOut]{
		Name:        "test.searxng",
		Description: "Returns SearXNG status.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, fakeSearxIn) (fakeSearxOut, error) {
			return fakeSearxOut{Text: "SearXNG is running smoothly."}, nil
		},
		Speak: func(out fakeSearxOut) string { return out.Text },
	}
	if err := tool.Add(reg, searxTool); err != nil {
		t.Fatalf("Add test.searxng: %v", err)
	}

	lastStore := lastreply.New()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p, err := pipeline.New(reg, map[string]string{
		"check status": "test.searxng",
	}, logger, pipeline.WithLastReply(lastStore))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	sink := discardSink{}
	speaker := speech.New(wyoming.NewClient(ln.Addr().String()), sink, wyoming.Voice{Name: "alba"}, nil)
	pronouncer := speech.NewPronouncer([]string{"SearXNG=search engine"})
	speaker.BindPronunciation(func() *speech.Pronouncer {
		return pronouncer
	})

	session := reply.New(p, speaker, sink, nil, reply.WithLogger(logger))

	var mu sync.Mutex
	var completed []latency.Completed
	timeline := latency.New("req-tts-test", latency.SourceDashboard, latency.WithObserver(func(c latency.Completed) {
		mu.Lock()
		defer mu.Unlock()
		completed = append(completed, c)
	}))
	ctx := latency.WithContext(context.Background(), timeline)

	req := tool.Request{ID: "req-tts-test"}
	replyResult, err := session.Ask(ctx, req, "check status")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// 1. Synthesize request must carry the REPLACED text.
	select {
	case synthText := <-requests:
		if synthText != "search engine is running smoothly." {
			t.Fatalf("synthesize request text = %q, want %q", synthText, "search engine is running smoothly.")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for synthesize request")
	}

	// 2. Reply.Text must keep the ORIGINAL text.
	if replyResult.Text != "SearXNG is running smoothly." {
		t.Fatalf("reply.Text = %q, want original %q", replyResult.Text, "SearXNG is running smoothly.")
	}

	// 3. The last-reply text must keep the ORIGINAL text.
	last, ok := lastStore.Last()
	if !ok {
		t.Fatal("last-reply store has no entry")
	}
	if last.Text != "SearXNG is running smoothly." {
		t.Fatalf("last.Text = %q, want original %q", last.Text, "SearXNG is running smoothly.")
	}

	// 4. The Activity entry must keep the ORIGINAL text.
	mu.Lock()
	defer mu.Unlock()
	if len(completed) != 1 || completed[0].Detail == nil {
		t.Fatalf("completed records = %+v, want 1 with Detail", completed)
	}
	if got := completed[0].Detail.Reply; got != "SearXNG is running smoothly." {
		t.Fatalf("Activity detail.Reply = %q, want original %q", got, "SearXNG is running smoothly.")
	}
}
