// Command dashsmoke serves the Dashboard handler with the same embedded
// bundle the Pi serves, but answers every request from memory. The browser
// smoke test drives this binary, so a page is exercised against production
// routing, CSP and assets without a Pi, a database or a real microphone.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// fixtures holds canned page views served by the populated (-fixtures) mode so
// the e2e suite exercises the real page markup without wiring every production
// service. Each file is named "<slug>.json" for the /api/ui/<slug> page.
//
//go:embed all:fixtures
var fixturesFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	owner := flag.String("owner", "smoke@example.com", "the Tailscale login the stub stands in for")
	uiURL := flag.String("ui-url", "", "optional standalone Dashboard artifact base URL")
	uiVersion := flag.String("ui-version", "smoke", "artifact pin reported in X-Dashboard-UI-Version")
	// fixtures turns on the populated mode: the stub serves a minimal but
	// complete view per page (one row of each kind) instead of the first-run
	// not-configured state, so the route and axe suites see the real markup.
	fixtures := flag.Bool("fixtures", false, "serve populated page fixtures instead of the first-run not-configured state")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The Dashboard proxies /api/* to the assistant API. Mount the deferred
	// queue from a stub so the queued-actions surfaces exercise the real route
	// (and never 404) instead of only the Settings page tolerating it (#297).
	api := http.NewServeMux()
	api.Handle("/deferred", &pipeline.DeferredQueue{})

	opts := dashboard.Options{
		Owner:       *owner,
		Logger:      logger,
		Activity:    activity.New(50),
		Typed:       typedStub{},
		Transcriber: transcriberStub{},
		Volume:      &volumeStub{percent: 40},
		UIURL:       *uiURL,
		UIVersion:   *uiVersion,
		API:         api,
	}
	// The populated mode only swaps the page reads for fixtures; the shell
	// (session, activity, volume, transcriber, typed) stays exactly as the
	// first-run mode, so both modes share one stub implementation.
	if *fixtures {
		sub, err := fs.Sub(fixturesFS, "fixtures")
		if err != nil {
			log.Fatalf("dashsmoke fixtures: %v", err)
		}
		opts.Fixtures = sub
	}
	handler, err := dashboard.New(opts)
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}

	// tailscale serve adds the identity header in production; the stub stands
	// in for it on loopback so the real auth guard still runs.
	serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				r.Header.Set("Tailscale-User-Login", *owner)
			}
		}
		handler.ServeHTTP(w, r)
	})

	log.Printf("dashsmoke listening on http://%s", *addr)
	server := &http.Server{Addr: *addr, Handler: serve, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// typedStub answers a typed request and records its reply for the Activity ring.
type typedStub struct{}

func (typedStub) Ask(ctx context.Context, _ tool.Request, transcript string) (pipeline.Reply, error) {
	reply := fmt.Sprintf("You said: %s", transcript)
	// The real reply.Session records the route, transcript and display summary;
	// the stub does it directly so Activity shows the exchange.
	latency.SetRoute(ctx, string(tool.PathFallthrough), "", transcript)
	latency.SetSummary(ctx, reply, false, false)
	return pipeline.Reply{Text: reply}, nil
}

// transcriberStub accepts a browser recording and returns a fixed transcript.
type transcriberStub struct{}

func (transcriberStub) Transcribe(context.Context, []byte) (string, error) {
	return "hello from the browser", nil
}

// volumeStub reads a fixed level and refuses every write, which is the case the
// UI must show rather than silently swallow.
type volumeStub struct {
	mu      sync.Mutex
	percent int
}

func (v *volumeStub) state() audio.VolumeState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return audio.VolumeState{Percent: v.percent, Muted: false}
}

func (v *volumeStub) Volume(context.Context) (audio.VolumeState, error) { return v.state(), nil }

func (v *volumeStub) SetVolume(context.Context, int) error {
	return fmt.Errorf("The speaker did not accept that change.")
}

func (v *volumeStub) SetMute(context.Context, bool) error {
	return fmt.Errorf("The speaker did not accept that change.")
}
