// Package activity keeps the recent-request list behind the authenticated
// Dashboard Activity page. It is fed by the one latency completion event that
// also writes the structured request log, so there is no second tracing
// protocol and no unbounded log file to read back.
//
// The bounded ring is the fast first page. When a History is attached it is
// also the durable source of truth: a request from last week is still
// searchable after a restart (#96).
//
// Privacy: entries include the reply text and Tool results, including
// Local-only results. That data stays in this process's memory and database and
// is shown only by the authenticated Dashboard. Nothing here is logged, sent to
// a model, placed in Agent history, or passed to a Notification.
package activity

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

const (
	// DefaultLimit is the number of recent requests kept in memory. The page
	// shows fewer than this; the cap keeps one long-running process bounded.
	DefaultLimit = 100

	// maxTranscriptRunes and maxReplyRunes bound one entry's text. A model
	// answer or transcription can be long, but the Dashboard only needs a
	// readable recent summary.
	maxTranscriptRunes = 4000
	maxReplyRunes      = 4000
	maxResultRunes     = 2000
)

// stageOrder is the display order for measured stages. Unknown stages follow
// in name order, so a future stage still appears without a page change.
var stageOrder = []latency.Stage{
	latency.StageEndpointing,
	latency.StageSTTFinalize,
	latency.StageRouting,
	latency.StageToolExecute,
	latency.StageAgentFirstToken,
	latency.StageTTSFirstAudio,
}

// checkpointOrder is the display order for turn-relative checkpoints.
var checkpointOrder = []string{latency.CheckpointFirstChunk}

// Timing is one measured stage or checkpoint in milliseconds.
type Timing struct {
	Name string
	MS   int64
}

// Call is one Tool call and its human-facing result. LocalOnly marks a result
// that may be shown only on the authenticated Dashboard.
type Call struct {
	Name      string
	LocalOnly bool
	Result    string
}

// Entry is one completed request as the Activity page renders it.
type Entry struct {
	ID          string
	At          time.Time
	Source      string
	Path        string
	Tool        string
	Outcome     string
	Error       string
	Transcript  string
	Reply       string
	LocalOnly   bool
	Pending     bool
	ResponseMS  *int64
	Stages      []Timing
	Checkpoints []Timing
	Calls       []Call
	// Summary is set when retention has trimmed this row's long text.
	Summary bool
	// Cursor is the durable history position this row was read at. It is empty
	// for a row that only exists in the in-memory ring.
	Cursor string
}

// Store is the bounded recent-request ring. It is safe for concurrent use by
// the voice, debug and Dashboard paths.
type Store struct {
	mu        sync.Mutex
	limit     int
	entries   []Entry
	now       func() time.Time
	history   *History
	observers map[uint64]func(Entry)
	nextSub   uint64
}

// Subscribe registers a listener for each new ring entry; the returned
// function removes it. The Dashboard's live stream is the one subscriber, so a
// completed request can be pushed without polling. A slow listener cannot
// block Observe: the caller must not hold a lock in the callback.
func (s *Store) Subscribe(listener func(Entry)) (cancel func()) {
	if s == nil || listener == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.observers == nil {
		s.observers = make(map[uint64]func(Entry))
	}
	s.nextSub++
	id := s.nextSub
	s.observers[id] = listener
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.observers, id)
	}
}

// New builds a bounded store. A zero or negative limit uses DefaultLimit.
func New(limit int) *Store {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Store{limit: limit, now: time.Now}
}

// NewWithHistory builds the same ring and also writes each entry to the
// durable history, which then answers anything older than the ring.
func NewWithHistory(limit int, history *History) *Store {
	store := New(limit)
	store.history = history
	return store
}

// History returns the attached durable store, or nil for a ring-only test or
// debug setup.
func (s *Store) History() *History {
	if s == nil {
		return nil
	}
	return s.history
}

// Observe implements latency.Observer. It copies the completion into a plain,
// bounded Entry and drops a wake detection that produced neither a transcript
// nor a route, so empty microphone turns do not fill the page.
func (s *Store) Observe(completed latency.Completed) {
	if s == nil {
		return
	}
	if strings.TrimSpace(completed.Transcript) == "" && strings.TrimSpace(completed.Path) == "" {
		return
	}
	entry := Entry{
		ID:         completed.ID,
		At:         s.now(),
		Source:     string(completed.Source),
		Path:       completed.Path,
		Tool:       completed.Tool,
		Outcome:    string(completed.Outcome),
		Error:      truncate(completed.Error, maxResultRunes),
		Transcript: truncate(completed.Transcript, maxTranscriptRunes),
	}
	if completed.HasResponse {
		ms := completed.Response.Milliseconds()
		entry.ResponseMS = &ms
	}
	entry.Stages = timings(completed.Stages, stageOrder)
	entry.Checkpoints = checkpoints(completed.Checkpoints)
	if completed.Detail != nil {
		entry.Reply = truncate(completed.Detail.Reply, maxReplyRunes)
		entry.LocalOnly = completed.Detail.LocalOnly
		entry.Pending = completed.Detail.Pending
		entry.Calls = calls(completed.Detail.Calls)
	}
	// Keep at least the Tool names when an older path reported only the
	// comma-separated list. The display can then always answer "which Tool".
	if len(entry.Calls) == 0 && entry.Tool != "" {
		for _, name := range strings.Split(entry.Tool, ",") {
			if name = strings.TrimSpace(name); name != "" {
				entry.Calls = append(entry.Calls, Call{Name: name})
			}
		}
	}

	s.mu.Lock()
	s.entries = append(s.entries, entry)
	if len(s.entries) > s.limit {
		// Copy into a fresh slice so the discarded entries can be reclaimed.
		s.entries = append([]Entry(nil), s.entries[len(s.entries)-s.limit:]...)
	}
	observers := make([]func(Entry), 0, len(s.observers))
	for _, observer := range s.observers {
		observers = append(observers, observer)
	}
	s.mu.Unlock()

	// Notify outside the lock, so a stream can never block the voice path.
	for _, observer := range observers {
		observer(entry)
	}
	// The durable write happens outside the ring lock: a slow disk must never
	// make a reader wait, and Add itself reports and swallows its error so the
	// completed request is unaffected.
	if s.history != nil {
		_ = s.history.Add(entry)
	}
}

// Recent returns up to limit entries, newest first. A zero or negative limit
// returns every retained entry.
func (s *Store) Recent(limit int) []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.entries) {
		limit = len(s.entries)
	}
	out := make([]Entry, 0, limit)
	for i := len(s.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.entries[i])
	}
	return out
}

// Get returns one retained entry by request ID.
func (s *Store) Get(id string) (Entry, bool) {
	if s == nil || id == "" {
		return Entry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].ID == id {
			return s.entries[i], true
		}
	}
	return Entry{}, false
}

// Len returns the number of retained entries; it is useful for bounded tests.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// timings orders measured stages by the fixed display order, then any unknown
// stage by name.
func timings(values map[latency.Stage]time.Duration, order []latency.Stage) []Timing {
	if len(values) == 0 {
		return nil
	}
	out := make([]Timing, 0, len(values))
	seen := make(map[latency.Stage]bool, len(values))
	for _, stage := range order {
		if value, ok := values[stage]; ok {
			out = append(out, Timing{Name: string(stage), MS: value.Milliseconds()})
			seen[stage] = true
		}
	}
	var unknown []latency.Stage
	for stage := range values {
		if !seen[stage] {
			unknown = append(unknown, stage)
		}
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i] < unknown[j] })
	for _, stage := range unknown {
		out = append(out, Timing{Name: string(stage), MS: values[stage].Milliseconds()})
	}
	return out
}

// checkpoints orders turn-relative checkpoints by the fixed display order.
func checkpoints(values map[string]time.Duration) []Timing {
	if len(values) == 0 {
		return nil
	}
	out := make([]Timing, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, name := range checkpointOrder {
		if value, ok := values[name]; ok {
			out = append(out, Timing{Name: name, MS: value.Milliseconds()})
			seen[name] = true
		}
	}
	var unknown []string
	for name := range values {
		if !seen[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		out = append(out, Timing{Name: name, MS: values[name].Milliseconds()})
	}
	return out
}

func calls(details []latency.ToolCallDetail) []Call {
	if len(details) == 0 {
		return nil
	}
	out := make([]Call, 0, len(details))
	for _, detail := range details {
		out = append(out, Call{
			Name:      detail.Name,
			LocalOnly: detail.LocalOnly,
			Result:    truncate(detail.Result, maxResultRunes),
		})
	}
	return out
}

// truncate bounds one display field on a rune boundary, so invalid UTF-8
// cannot reach the template. A clipped value says so instead of looking whole.
func truncate(text string, maxRunes int) string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxRunes]) + "…"
}
