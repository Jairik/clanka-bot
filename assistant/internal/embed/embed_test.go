package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/embed"
)

func TestEmbedSendsTextsAndReturnsVectors(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody struct {
		Texts []string `json:"texts"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "test-model",
			"vectors": [][]float32{{0.6, 0.8}, {1, 0}},
		})
	}))
	defer srv.Close()

	client := embed.NewClient(srv.URL, "test-model")
	vectors, err := client.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/embed" {
		t.Fatalf("request = %s %s, want POST /embed", gotMethod, gotPath)
	}
	if len(gotBody.Texts) != 2 || gotBody.Texts[0] != "hello" {
		t.Fatalf("texts = %v", gotBody.Texts)
	}
	if len(vectors) != 2 || len(vectors[0]) != 2 || vectors[0][0] != 0.6 {
		t.Fatalf("vectors = %v", vectors)
	}
}

func TestEmbedRejectsBadResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`},
		{"not json", http.StatusOK, `oops`},
		{"count mismatch", http.StatusOK, `{"vectors":[[1,0],[0,1]]}`},
		{"empty vector", http.StatusOK, `{"vectors":[[]]}`},
		{"ragged dimensions", http.StatusOK, `{"vectors":[[1,0],[1]]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := embed.NewClient(srv.URL, "m").Embed(context.Background(), []string{"hi"})
			if err == nil {
				t.Fatal("Embed succeeded, want error")
			}
		})
	}
}

func TestEmbedHonoursContextTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := embed.NewClient(srv.URL, "m").Embed(ctx, []string{"hi"})
	if err == nil {
		t.Fatal("Embed succeeded, want timeout error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Embed took %s, want close to 20ms", elapsed)
	}
	if !strings.Contains(err.Error(), "embed") {
		t.Fatalf("error %q does not mention embed", err)
	}
}

func TestEmbedWithNoTextsSkipsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server was called for an empty batch")
	}))
	defer srv.Close()

	vectors, err := embed.NewClient(srv.URL, "m").Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 0 {
		t.Fatalf("vectors = %v, want none", vectors)
	}
}
