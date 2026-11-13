// Live updates over Server-Sent Events (#94). One stream carries the deltas the
// pages used to poll for: a completed request, the speaker state, a changed
// alert list and a changed health report. The stream is read-only and sits
// behind the same identity, Host and source checks as every other route; a
// write still goes through its own Mutating route.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
)

const (
	// liveBuffer bounds one client. A reader that falls behind is closed so
	// EventSource reconnects and resyncs, rather than growing a queue on the
	// Pi.
	liveBuffer = 16
	// liveKeepalive is a comment every interval so an idle proxy does not drop
	// the connection.
	liveKeepalive = 15 * time.Second
)

// liveEvent is one already-encoded frame. data is JSON with escaped newlines,
// so it is always one SSE data line.
type liveEvent struct {
	name string
	data []byte
	id   uint64
}

// liveClient is one open stream. Its buffer is fixed; lag() closes the stream
// when the reader cannot keep up.
type liveClient struct {
	events chan liveEvent
	lagged chan struct{}
	once   sync.Once
}

func (c *liveClient) lag() { c.once.Do(func() { close(c.lagged) }) }

// liveHub fans out to every open stream and owns the samplers. It starts the
// samplers with the first stream and stops them with the last, so an idle
// Dashboard costs no health checks.
type liveHub struct {
	handler *Handler

	mu      sync.Mutex
	clients map[*liveClient]struct{}
	cancel  context.CancelFunc
	seq     uint64
	// last is the last key per event name, so a sampler only sends a change.
	// It is cleared when the last stream leaves.
	last map[string]string

	// The production cadence; an internal test shortens them.
	volumeEvery time.Duration
	alertsEvery time.Duration
	healthEvery time.Duration
}

func newLiveHub(handler *Handler) *liveHub {
	return &liveHub{
		handler:     handler,
		clients:     make(map[*liveClient]struct{}),
		last:        make(map[string]string),
		volumeEvery: 5 * time.Second,
		alertsEvery: 10 * time.Second,
		healthEvery: 30 * time.Second,
	}
}

// publish sends one delta to every stream. A full client buffer closes that
// stream; the client resyncs on reconnect.
func (hub *liveHub) publish(name string, data any) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.send(name, encoded)
}

// diffAndPublish sends data only when it differs from the last payload of the
// same name, so a sampler is silent when nothing changed.
func (hub *liveHub) diffAndPublish(name string, data any) {
	hub.diffAndPublishKey(name, data, data)
}

// diffAndPublishKey compares key while sending data. The health view carries a
// checked-at string that changes on every probe; the key drops it, so only a
// real check change is sent and the timestamp stays truthful in the payload.
func (hub *liveHub) diffAndPublishKey(name string, key, data any) {
	keyBytes, err := json.Marshal(key)
	if err != nil {
		return
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.last[name] == string(keyBytes) {
		return
	}
	hub.last[name] = string(keyBytes)
	hub.send(name, encoded)
}

// send must be called with hub.mu held.
func (hub *liveHub) send(name string, encoded []byte) {
	hub.seq++
	event := liveEvent{name: name, data: encoded, id: hub.seq}
	for client := range hub.clients {
		select {
		case client.events <- event:
		default:
			client.lag()
		}
	}
}

// subscribe adds one stream and starts the samplers on the first one.
func (hub *liveHub) subscribe() *liveClient {
	client := &liveClient{
		events: make(chan liveEvent, liveBuffer),
		lagged: make(chan struct{}),
	}
	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	if hub.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		hub.cancel = cancel
		go hub.sample(ctx)
	}
	hub.mu.Unlock()
	return client
}

// unsubscribe closes one stream and stops the samplers when it was the last.
func (hub *liveHub) unsubscribe(client *liveClient) {
	hub.mu.Lock()
	delete(hub.clients, client)
	if len(hub.clients) == 0 && hub.cancel != nil {
		hub.cancel()
		hub.cancel = nil
		hub.last = make(map[string]string)
	}
	hub.mu.Unlock()
}

// sample runs one goroutine for every open stream. Each source has its own
// ticker, no more frequent than the polling it replaces.
func (hub *liveHub) sample(ctx context.Context) {
	if hub.handler == nil {
		return
	}
	volume := time.NewTicker(hub.volumeEvery)
	defer volume.Stop()
	alerts := time.NewTicker(hub.alertsEvery)
	defer alerts.Stop()
	health := time.NewTicker(hub.healthEvery)
	defer health.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-volume.C:
			hub.sampleVolume(ctx)
		case <-alerts.C:
			hub.sampleAlerts(ctx)
		case <-health.C:
			hub.sampleHealth(ctx)
		}
	}
}

func (hub *liveHub) sampleVolume(ctx context.Context) {
	if hub.handler.volume == nil {
		return
	}
	hub.diffAndPublish("volume", hub.handler.readVolume(ctx, ""))
}

func (hub *liveHub) sampleAlerts(context.Context) {
	view := hub.handler.buildAlertsView("", "", nil)
	if !view.HasManagers {
		// Nothing is wired in this deployment, so the list cannot change.
		return
	}
	hub.diffAndPublish("alerts", view)
}

func (hub *liveHub) sampleHealth(ctx context.Context) {
	view := hub.handler.health(ctx)
	key := view
	key.CheckedAt = ""
	hub.diffAndPublishKey("health", key, view)
}

// events is the one live stream. It sends an event only for a delta; the
// client reloads the current view on every open, so nothing missed while the
// stream was down has to be replayed.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	client := h.live.subscribe()
	defer h.live.unsubscribe(client)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Connection", "keep-alive")
	// A buffering reverse proxy would hold the frames; tailscale serve does
	// not, and this tells any future one not to.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// The native reconnect delay is a fallback; the client runs its own
	// backoff and reloads the view on open.
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(liveKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			// The page went away (or the server is shutting down); the defer
			// unsubscribes and stops the samplers when this was the last one.
			return
		case event := <-client.events:
			_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.id, event.name, event.data)
			flusher.Flush()
		case <-client.lagged:
			// This reader fell behind; closing makes EventSource reconnect,
			// and the client resyncs instead of silently losing the frame.
			return
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// liveActivity forwards a completed request to every stream. It is registered
// once in New, so the ring's own observer seam is the only source.
func (h *Handler) liveActivity(entry activity.Entry) {
	if h.live == nil {
		return
	}
	h.live.publish("activity", h.requestView(entry))
}
