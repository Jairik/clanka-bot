package pipeline

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type benchIn struct{}

type benchOut struct {
	Value string `json:"value"`
}

type benchEmbedder struct{}

// Embed returns basis vectors for bench examples and queries.
func (e *benchEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if text == "turn off the lights" {
			out[i] = []float32{1.0, 0.0}
		} else {
			out[i] = []float32{0.0, 1.0}
		}
	}
	return out, nil
}

// BenchmarkDeferredRegex measures the matching cost of the hoisted package-level deferred triggers.
func BenchmarkDeferredRegex(b *testing.B) {
	b.ReportAllocs()
	text := "turn off the lights"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		trimmed := strings.TrimSpace(text)
		for _, tr := range deferredTriggers {
			_ = tr.re.FindStringSubmatch(trimmed)
		}
	}
}

// BenchmarkHasJoiner measures clause joiner detection using the shared joiner set.
func BenchmarkHasJoiner(b *testing.B) {
	b.ReportAllocs()
	joiners := map[string]bool{"and": true, "then": true, "also": true}
	text := "turn off the lights and play some music"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hasJoiner(text, joiners)
	}
}

// BenchmarkSecondCommandClause measures compound command detection after joiners using the router's joiner set.
func BenchmarkSecondCommandClause(b *testing.B) {
	b.ReportAllocs()
	reg := tool.NewRegistry(time.Second)
	router := NewSemanticRouter(reg, &benchEmbedder{}, SemanticConfig{
		Threshold: 0.75,
		Margin:    0.1,
		Joiners:   []string{"and", "then", "also"},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(reg, nil, logger, WithSemanticRouter(router))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	phrases := map[string]string{"turn off the lights": "lights.off"}
	intents := map[string]string{}
	text := "set a timer for five minutes and turn off the lights"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.secondCommandClause(text, phrases, intents)
	}
}

// BenchmarkHandle measures pipeline routing overhead for a control phrase hit and a semantic miss.
func BenchmarkHandle(b *testing.B) {
	reg := tool.NewRegistry(time.Second)
	_ = tool.Add(reg, tool.Tool[benchIn, benchOut]{
		Name:        "lights.off",
		Description: "Turn off lights.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, benchIn) (benchOut, error) {
			return benchOut{Value: "ok"}, nil
		},
		Speak:    func(out benchOut) string { return out.Value },
		Examples: []string{"turn off the lights"},
	})

	emb := &benchEmbedder{}
	router := NewSemanticRouter(reg, emb, SemanticConfig{
		Threshold: 0.75,
		Margin:    0.1,
		Joiners:   []string{"and", "then", "also"},
	})
	if err := router.Build(context.Background()); err != nil {
		b.Fatalf("router.Build: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(reg, map[string]string{"turn off the lights": "lights.off"}, logger, WithSemanticRouter(router))
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	req := tool.Request{ID: "bench-req"}
	ctx := context.Background()

	b.Run("ControlPhrase", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = p.Handle(ctx, req, "turn off the lights")
		}
	})

	b.Run("SemanticMiss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = p.Handle(ctx, req, "what is the meaning of life")
		}
	})
}
