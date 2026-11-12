package quiet

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// HoldQueue is the Announcement queue as Quiet hours sees it. announce.Queue
// satisfies it; a nil queue keeps the controller inert.
type HoldQueue interface {
	Hold()
	Release()
}

// Controller translates the live Quiet status into the Announcement queue's
// Hold/Release gate. It is deliberately small: one worker, bounded
// reevaluation at the next window boundary or after a short interval, and no
// settings observer. The wording manager therefore keeps the single settings
// observer it already owns.
type Controller struct {
	service *Service
	queue   HoldQueue
	logger  *slog.Logger
	// interval bounds how long a Dashboard/voice window change waits before
	// it is applied. It is also the fallback when no boundary is known.
	interval time.Duration

	mu     sync.Mutex
	held   bool
	closed bool
	cancel context.CancelFunc
	done   chan struct{}
	start  sync.Once
}

// ControllerInterval is the default reevaluation bound. It is short enough
// that a Dashboard edit applies "promptly" without a request, and cheap enough
// for one settings read per second.
const ControllerInterval = time.Second

// NewController builds the quiet gate around one Announcement queue.
func NewController(service *Service, queue HoldQueue, logger *slog.Logger) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		service:  service,
		queue:    queue,
		logger:   logger,
		interval: ControllerInterval,
		done:     make(chan struct{}),
	}
}

// WithInterval changes the bounded reevaluation period, mainly for tests.
func (c *Controller) WithInterval(interval time.Duration) *Controller {
	if c != nil && interval > 0 {
		c.interval = interval
	}
	return c
}

// Start applies the current status and runs the worker until ctx is cancelled
// or Close is called. It is safe to call once; a nil service or queue leaves it
// inert.
func (c *Controller) Start(ctx context.Context) {
	if c == nil || c.service == nil || c.queue == nil {
		return
	}
	c.start.Do(func() {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		runCtx, cancel := context.WithCancel(ctx)
		c.cancel = cancel
		c.mu.Unlock()
		c.reevaluate()
		go c.run(runCtx)
	})
}

// Reevaluate applies the current status immediately. The worker calls it on
// every boundary; tests and a post-write caller can use it without waiting for
// the interval.
func (c *Controller) Reevaluate() {
	if c == nil {
		return
	}
	c.reevaluate()
}

// reevaluate holds or releases the queue to match the live status.
func (c *Controller) reevaluate() {
	if c == nil || c.service == nil || c.queue == nil {
		return
	}
	status := c.service.Status()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	switch {
	case status.Quiet && !c.held:
		c.queue.Hold()
		c.held = true
		c.logger.Info("quiet hours on", "reason", string(status.Reason), "until", status.WindowEnd)
	case !status.Quiet && c.held:
		c.queue.Release()
		c.held = false
		c.logger.Info("quiet hours off", "reason", string(status.Reason))
	}
}

// run wakes at the next known boundary, bounded by interval so a Dashboard
// write is noticed without a settings observer.
func (c *Controller) run(ctx context.Context) {
	defer close(c.done)
	for {
		wait := c.interval
		status := c.service.Status()
		if !status.NextChange.IsZero() {
			until := time.Until(status.NextChange)
			if until < wait {
				wait = until
			}
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		c.reevaluate()
	}
}

// Close stops the worker and releases the queue if this controller still holds
// it, so a shutdown cannot leave announcements stuck behind a dead gate. It is
// idempotent.
func (c *Controller) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	cancel := c.cancel
	held := c.held
	c.held = false
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-c.done
	}
	if held && c.queue != nil {
		c.queue.Release()
	}
}
