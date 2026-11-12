package dashboard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
)

// wyomingHealthTimeout bounds a connected service that stops answering. The
// page context still bounds the dial, so the two limits together keep one
// Health refresh short even when a model container is wedged.
const wyomingHealthTimeout = 2 * time.Second

// ComposeServiceChecks returns the seven services in the current Compose
// stack. The checks are endpoint checks, not Docker's health field: the page
// answers whether the assistant can actually reach what it needs. librespot
// has no HTTP endpoint, so its check reads the heartbeat it writes to the
// status volume.
func ComposeServiceChecks(cfg config.Config) []ServiceCheck {
	webCfg := web.FromConfig(cfg)
	return []ServiceCheck{
		HTTPService("embed", strings.TrimRight(cfg.Embed.URL, "/")+"/healthz"),
		SelfService("assistant"),
		WyomingService("wakeword", cfg.Wakeword.Addr),
		WyomingService("stt", cfg.STT.Addr),
		WyomingService("tts", cfg.TTS.Addr),
		HTTPService("web-search", strings.TrimRight(webCfg.BaseURL, "/")+"/healthz"),
		PairingService("librespot", cfg.Spotify.PairingStatusFile, cfg.Spotify.StaleAfter.Duration()),
	}
}

// SelfService marks the process serving the page as healthy.
func SelfService(name string) ServiceCheck {
	return ServiceCheck{Name: name, Check: func(context.Context) error { return nil }}
}

// HTTPService checks a service's local HTTP health endpoint. The request
// context bounds it, so the page can abandon a dead service.
func HTTPService(name, healthURL string) ServiceCheck {
	return ServiceCheck{Name: name, Check: func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		// Drain a bounded amount so the connection can be reused, without
		// letting a chatty service fill memory.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("health endpoint returned %s", resp.Status)
		}
		return nil
	}}
}

// WyomingService checks a Wyoming service with describe, the same readiness
// call Compose uses. An empty address means the feature is not configured in
// this deployment.
func WyomingService(name, addr string) ServiceCheck {
	if strings.TrimSpace(addr) == "" {
		return ServiceCheck{Name: name}
	}
	return ServiceCheck{Name: name, Check: func(ctx context.Context) error {
		client := wyoming.NewClient(addr, wyoming.WithTimeout(wyomingHealthTimeout))
		_, err := client.Describe(ctx)
		return err
	}}
}

// PairingService checks librespot's heartbeat file. A missing or stale file is
// a real failure for the Compose row; the Spotify section separately reports
// whether the cached credentials say paired or unpaired.
func PairingService(name, path string, staleAfter time.Duration) ServiceCheck {
	if strings.TrimSpace(path) == "" {
		return ServiceCheck{Name: name}
	}
	reader := spotify.PairingReader{Path: path, StaleAfter: staleAfter}
	return ServiceCheck{Name: name, Check: func(context.Context) error {
		status := reader.Status()
		if !status.Available {
			return fmt.Errorf("%s", status.Detail)
		}
		return nil
	}}
}
