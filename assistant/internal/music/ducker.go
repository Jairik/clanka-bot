// Package music lowers and restores the librespot stream locally. ADR-0004
// puts Spotify on the Assistant's own Pulse sink so a reply, a Ring or the
// wake word can duck it instantly without a Web API call. Ducking is a
// refcounted hold: overlapping holders (a Ring during a reply) restore only
// when the last one releases.
package music

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultDuckPercent is the stream level while ducked. It is a starting
	// value that has not been tuned on the Pi's speaker; #70's spike or live
	// listening should confirm or replace it.
	DefaultDuckPercent = 25
	// normalPercent is the stream level when not ducked. Spotify's own volume
	// (Web API / Connect) is separate from the Pulse stream volume, so the
	// stream always returns to full and the user's Spotify level stays theirs.
	normalPercent = 100
	// restoreTimeout bounds the restore when the caller's context is gone.
	restoreTimeout = 3 * time.Second
)

// Streams sets the Pulse volume of every music stream. The real implementation
// is PulseStreams; tests supply a fake.
type Streams interface {
	SetPercent(ctx context.Context, percent int) error
}

// Ducker is a refcounted local duck. It satisfies ring.Music and the voice
// loop's Ducker seam, so one instance serves replies, Rings and the wake word.
type Ducker struct {
	streams Streams
	percent int
	logger  *slog.Logger

	mu    sync.Mutex
	depth int
}

// NewDucker builds a Ducker. A percent outside 0..99 uses DefaultDuckPercent.
func NewDucker(streams Streams, duckPercent int, logger *slog.Logger) *Ducker {
	if duckPercent < 0 || duckPercent >= normalPercent {
		duckPercent = DefaultDuckPercent
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Ducker{streams: streams, percent: duckPercent, logger: logger}
}

// Duck lowers the music and returns an idempotent restore. The restore is
// always non-nil, even on error, so callers can defer it unconditionally.
// Only the first holder lowers the stream and only the last restores it.
func (d *Ducker) Duck(ctx context.Context) (func(), error) {
	if d == nil || d.streams == nil {
		return func() {}, nil
	}
	d.mu.Lock()
	d.depth++
	first := d.depth == 1
	d.mu.Unlock()

	var err error
	if first {
		err = d.streams.SetPercent(ctx, d.percent)
	}
	var once sync.Once
	return func() { once.Do(d.release) }, err
}

// release drops one hold and restores when none remain.
func (d *Ducker) release() {
	d.mu.Lock()
	d.depth--
	last := d.depth == 0
	d.mu.Unlock()
	if !last {
		return
	}
	// The holder's context may already be cancelled (a stopped Ring), and a
	// stuck duck would leave the music quiet, so the restore has its own clock.
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	if err := d.streams.SetPercent(ctx, normalPercent); err != nil {
		d.logger.Warn("music restore", "error", err.Error())
	}
}
