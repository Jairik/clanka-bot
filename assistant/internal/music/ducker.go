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
	// pactlTimeout bounds each Duck or restore, so a hung pactl cannot stall
	// the wake path or a Ring.
	pactlTimeout = time.Second
)

// Streams sets the Pulse volume of every music stream. The real implementation
// is PulseStreams; tests supply a fake.
type Streams interface {
	SetPercent(ctx context.Context, percent int) error
}

// Ducker is a refcounted local duck. It satisfies ring.Music and the voice
// loop's Ducker seam, so one instance serves replies, Rings and the wake word.
// A nil Ducker is a no-op, so a setup without music spawns no pactl at all.
type Ducker struct {
	streams Streams
	percent int
	logger  *slog.Logger

	mu    sync.Mutex // guards depth
	depth int

	// opMu serializes the pactl work. Each run applies the level the depth
	// implies when it gets the lock, so a restore and a new duck can never
	// land out of order. applied is the last level set; -1 means unknown.
	opMu    sync.Mutex
	applied int
}

// NewDucker builds a Ducker. A percent outside 0..99 uses DefaultDuckPercent.
func NewDucker(streams Streams, duckPercent int, logger *slog.Logger) *Ducker {
	if duckPercent < 0 || duckPercent >= normalPercent {
		duckPercent = DefaultDuckPercent
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Ducker{streams: streams, percent: duckPercent, logger: logger, applied: -1}
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
	d.mu.Unlock()

	err := d.apply(ctx)
	var once sync.Once
	return func() { once.Do(d.release) }, err
}

// Reset sets the stream to full volume. The Assistant calls it once at startup
// so a duck left behind by a crash (or remembered by Pulse's stream-restore)
// does not persist. It is nil-safe.
func (d *Ducker) Reset(ctx context.Context) error {
	if d == nil || d.streams == nil {
		return nil
	}
	d.opMu.Lock()
	defer d.opMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, pactlTimeout)
	defer cancel()
	if err := d.streams.SetPercent(ctx, normalPercent); err != nil {
		return err
	}
	d.applied = normalPercent
	return nil
}

// release drops one hold and restores when none remain.
func (d *Ducker) release() {
	d.mu.Lock()
	d.depth--
	d.mu.Unlock()
	// The holder's context may already be cancelled (a stopped Ring), and a
	// stuck duck would leave the music quiet, so the restore has its own clock.
	if err := d.apply(context.Background()); err != nil {
		d.logger.Warn("music restore", "error", err.Error())
	}
}

// apply sets the level implied by the current depth, once it holds the pactl
// lock. A level already applied is skipped; a failure forgets it, so the next
// call retries.
func (d *Ducker) apply(ctx context.Context) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.mu.Lock()
	want := normalPercent
	if d.depth > 0 {
		want = d.percent
	}
	d.mu.Unlock()
	if want == d.applied {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pactlTimeout)
	defer cancel()
	if err := d.streams.SetPercent(ctx, want); err != nil {
		d.applied = -1 // a partial failure leaves the real level unknown
		return err
	}
	d.applied = want
	return nil
}
