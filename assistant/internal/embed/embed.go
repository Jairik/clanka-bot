// Package embed is the HTTP client for the MiniLM embed service.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxResponseBytes = 8 << 20

// Client calls one embed service. Timeouts come from the caller's context.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// NewClient builds a client for baseURL. Model is only for logging.
func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{},
	}
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// Embed returns one vector per text. It makes no request for an empty batch.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string][]string{"texts": texts})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: call service: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: service returned %s: %s", resp.Status, truncate(raw))
	}

	var decoded struct {
		Vectors [][]float32 `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(decoded.Vectors) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d texts", len(decoded.Vectors), len(texts))
	}
	dim := 0
	for i, v := range decoded.Vectors {
		if len(v) == 0 {
			return nil, fmt.Errorf("embed: vector %d is empty", i)
		}
		if i == 0 {
			dim = len(v)
			continue
		}
		if len(v) != dim {
			return nil, fmt.Errorf("embed: vector %d has %d dimensions, want %d", i, len(v), dim)
		}
	}
	return decoded.Vectors, nil
}

func truncate(raw []byte) string {
	const limit = 200
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}
