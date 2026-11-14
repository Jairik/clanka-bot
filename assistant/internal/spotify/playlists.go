package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Playlist cache bounds. 20 pages of 50 is 1000 playlists, far beyond one
// person's library; a runaway "next" chain cannot loop forever.
const (
	// DefaultPlaylistRefresh is how often the cache is refreshed after startup.
	DefaultPlaylistRefresh = 6 * time.Hour
	defaultPlaylistPages   = 20
	defaultPlaylistPage    = 50
)

// Playlist is one entry of the user's library.
type Playlist struct {
	Name string
	URI  string
}

// PlaylistCacheOptions configure a PlaylistCache.
type PlaylistCacheOptions struct {
	// API is the Web API seam; *Client satisfies it.
	API API
	// Logger receives refresh failures; nil uses slog.Default().
	Logger *slog.Logger
	// Interval is the refresh period; zero or negative uses six hours.
	Interval time.Duration
	// MaxPages bounds how many pages one refresh reads; zero uses 20.
	MaxPages int
	// PageSize is the page limit requested from Spotify (1-50); zero uses 50.
	PageSize int
	// NewTicker builds the refresh ticker and its stop function. Tests inject
	// it to drive refreshes without waiting; nil uses time.NewTicker.
	NewTicker func(d time.Duration) (<-chan time.Time, func())
	// Now is the clock for Updated; nil uses time.Now.
	Now func() time.Time
}

// PlaylistCache is a local copy of the user's playlists so a spoken name
// resolves without a Web API round trip. Reads never block on a refresh, and a
// failed refresh keeps the previous copy.
type PlaylistCache struct {
	api       API
	logger    *slog.Logger
	interval  time.Duration
	maxPages  int
	pageSize  int
	newTicker func(time.Duration) (<-chan time.Time, func())
	now       func() time.Time

	mu      sync.RWMutex
	lists   []Playlist
	loaded  bool
	updated time.Time

	runMu  sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewPlaylistCache builds an empty cache; call Start to fill and refresh it.
func NewPlaylistCache(opts PlaylistCacheOptions) *PlaylistCache {
	c := &PlaylistCache{
		api:       opts.API,
		logger:    opts.Logger,
		interval:  opts.Interval,
		maxPages:  opts.MaxPages,
		pageSize:  opts.PageSize,
		newTicker: opts.NewTicker,
		now:       opts.Now,
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.interval <= 0 {
		c.interval = DefaultPlaylistRefresh
	}
	if c.maxPages <= 0 {
		c.maxPages = defaultPlaylistPages
	}
	if c.pageSize <= 0 || c.pageSize > defaultPlaylistPage {
		c.pageSize = defaultPlaylistPage
	}
	if c.newTicker == nil {
		c.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Start begins the background refresh loop and returns at once: one refresh
// now, then one per interval, until Close or ctx ends. Calling it twice does
// nothing.
func (c *PlaylistCache) Start(ctx context.Context) {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.cancel != nil {
		return
	}
	ctx, c.cancel = context.WithCancel(ctx)
	ticks, stop := c.newTicker(c.interval)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer stop()
		c.refreshLogged(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				c.refreshLogged(ctx)
			}
		}
	}()
}

// Close cancels any refresh in flight and waits for the loop to end. It is
// safe to call more than once.
func (c *PlaylistCache) Close() {
	c.runMu.Lock()
	cancel := c.cancel
	c.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// refreshLogged refreshes and logs a failure. The error carries the HTTP
// status and Spotify's machine reason only, never a token or response body.
func (c *PlaylistCache) refreshLogged(ctx context.Context) {
	if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
		c.logger.Warn("spotify playlist refresh failed; keeping the previous list", "error", safeError(err))
	}
}

// safeError reduces an error to what is safe to log: the API status and reason
// when there are any, otherwise a fixed phrase.
func safeError(err error) string {
	if status, reason := APIStatus(err); status != 0 {
		if reason != "" {
			return fmt.Sprintf("spotify api status %d (%s)", status, reason)
		}
		return fmt.Sprintf("spotify api status %d", status)
	}
	switch {
	case err == ErrNotConfigured:
		return "spotify is not configured"
	case err == ErrNeedsReconnect:
		return "spotify needs to be reconnected"
	}
	return "request failed"
}

// Refresh reads every page of /me/playlists and replaces the cache. On any
// error the previous cache stays in place.
func (c *PlaylistCache) Refresh(ctx context.Context) error {
	var all []Playlist
	offset := 0
	for page := 0; page < c.maxPages; page++ {
		raw, err := c.api.Get(ctx, "/me/playlists", url.Values{
			"limit":  {strconv.Itoa(c.pageSize)},
			"offset": {strconv.Itoa(offset)},
		})
		if err != nil {
			return err
		}
		var body struct {
			Items []*struct {
				Name string `json:"name"`
				URI  string `json:"uri"`
			} `json:"items"`
			Next *string `json:"next"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("spotify api: playlists: %w", err)
		}
		for _, item := range body.Items {
			// A deleted or unavailable playlist can arrive as null.
			if item != nil && strings.TrimSpace(item.Name) != "" && item.URI != "" {
				all = append(all, Playlist{Name: strings.TrimSpace(item.Name), URI: item.URI})
			}
		}
		if body.Next == nil || *body.Next == "" || len(body.Items) == 0 {
			break
		}
		// Spotify's offset counts items, including the null ones.
		offset += len(body.Items)
	}
	c.mu.Lock()
	c.lists, c.loaded, c.updated = all, true, c.now()
	c.mu.Unlock()
	return nil
}

// Loaded reports whether a refresh has ever succeeded.
func (c *PlaylistCache) Loaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loaded
}

// Updated is when the cache was last refreshed; zero before the first success.
func (c *PlaylistCache) Updated() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.updated
}

// Names returns the playlist names, sorted, for the name matcher.
func (c *PlaylistCache) Names() []string {
	c.mu.RLock()
	names := make([]string, 0, len(c.lists))
	for _, pl := range c.lists {
		names = append(names, pl.Name)
	}
	c.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Lookup returns the playlist with exactly this name. When two playlists share
// a name, the first one Spotify listed wins; speech cannot tell them apart.
func (c *PlaylistCache) Lookup(name string) (Playlist, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, pl := range c.lists {
		if pl.Name == name {
			return pl, true
		}
	}
	return Playlist{}, false
}
