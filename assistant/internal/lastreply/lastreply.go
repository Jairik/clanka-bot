// Package lastreply keeps the most recent Assistant reply in memory so a
// follow-up request such as "say that again" or "send that to my phone" can
// refer to it. The record carries the reply's provenance: which Tool produced
// it and whether that Tool is Local-only. A consumer that leaves the Pi
// (Notifications) must refuse a reply whose provenance says it is Local-only,
// so the privacy declaration is never bypassed by a follow-up.
package lastreply

import (
	"strings"
	"sync"
	"time"
)

// Reply is one Assistant reply and where it came from.
type Reply struct {
	// Text is the full reply text the Assistant spoke or would have spoken.
	Text string
	// Tool is the canonical Tool name that produced the reply, if any. An
	// Agent-path answer with no Tool call is empty.
	Tool string
	// Path is the routing path that produced the reply, for debugging and
	// provenance. It is the string form of tool.Path.
	Path string
	// LocalOnly is true when the reply text came from a Local-only Tool. Such
	// text must never be sent off the Pi, including by a follow-up Tool.
	LocalOnly bool
	// At is when the reply was recorded.
	At time.Time
}

// Store is a concurrency-safe in-memory record of the last reply. It is
// deliberately not persisted: a restart has no "that" to refer to.
type Store struct {
	mu   sync.RWMutex
	last Reply
	ok   bool
}

// New builds an empty store.
func New() *Store { return &Store{} }

// Record stores one reply. An empty or blank reply is ignored so a silent turn
// does not erase the last thing the user actually heard. The newest non-blank
// reply wins; concurrent turns are serialised by the mutex, so the last writer
// is the one a later follow-up sees.
func (s *Store) Record(reply Reply) {
	if s == nil || strings.TrimSpace(reply.Text) == "" {
		return
	}
	if reply.At.IsZero() {
		reply.At = time.Now()
	}
	s.mu.Lock()
	s.last = reply
	s.ok = true
	s.mu.Unlock()
}

// Last returns the most recent reply and whether one exists. A caller gets a
// copy, so a later turn cannot change the text underneath it.
func (s *Store) Last() (Reply, bool) {
	if s == nil {
		return Reply{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last, s.ok
}
