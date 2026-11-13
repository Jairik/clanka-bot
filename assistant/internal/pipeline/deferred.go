package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// DeferredAction is an expiring, in-memory request waiting for one completion event.
type DeferredAction struct {
	ID      uint64    `json:"id"`
	Event   string    `json:"event"`
	Text    string    `json:"text"`
	Expires time.Time `json:"expires"`
	ready   bool
}

// DeferredQueue bounds pending requests and removes them before dispatch for at-most-once execution.
type DeferredQueue struct {
	mu      sync.Mutex
	next    uint64
	actions []DeferredAction
}

// List expires stale requests on every access.
func (q *DeferredQueue) List() []DeferredAction {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune()
	return append([]DeferredAction{}, q.actions...)
}

// prune requires the queue lock.
func (q *DeferredQueue) prune() {
	live := q.actions[:0]
	for _, a := range q.actions {
		if time.Now().Before(a.Expires) {
			live = append(live, a)
		}
	}
	q.actions = live
}

// Cancel removes one request, or all requests when id is zero.
func (q *DeferredQueue) Cancel(id uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	live := q.actions[:0]
	for _, a := range q.actions {
		if id != 0 && a.ID != id {
			live = append(live, a)
		}
	}
	q.actions = live
}

// Add refuses overflow rather than silently dropping an older request.
func (q *DeferredQueue) Add(event, text string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune()
	if len(q.actions) >= 8 {
		return false
	}
	q.next++
	q.actions = append(q.actions, DeferredAction{ID: q.next, Event: event, Text: text, Expires: time.Now().Add(time.Hour)})
	return true
}

// Take consumes matching actions atomically; callbacks execute outside the lock.
func (q *DeferredQueue) Take(event string) []DeferredAction {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune()
	var ready []DeferredAction
	live := q.actions[:0]
	for _, a := range q.actions {
		if a.Event == event {
			ready = append(ready, a)
		} else {
			live = append(live, a)
		}
	}
	q.actions = live
	return ready
}

// ServeHTTP is mounted behind the existing Dashboard identity and same-origin guards.
func (q *DeferredQueue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid cancellation", 400)
			return
		}
		id, err := strconv.ParseUint(r.FormValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", 400)
			return
		}
		q.Cancel(id)
	} else if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(q.List())
}

// BindDeferred shares orchestration state with the Dashboard and completion dispatcher.
func (p *Pipeline) BindDeferred(q *DeferredQueue) { p.deferred = q }

// deferredRequest recognises explicit orchestration syntax before semantic routing.
func (p *Pipeline) deferredRequest(ctx context.Context, req tool.Request, text string, phrases map[string]string) (Reply, bool) {
	if p.deferred == nil {
		return Reply{}, false
	}
	reply := Reply{Path: tool.PathControlPhrase, RequestID: req.ID}
	normalized := NormalizePhrase(text)
	if normalized == "cancel queued actions" || normalized == "cancel queued commands" {
		p.deferred.Cancel(0)
		reply.Text = "Cancelled all queued actions."
		return reply, true
	}
	// Commas are optional because STT punctuation varies.
	for prefix, event := range map[string]string{"when the music stops": "music", "after the timer": "timer", "when the reply ends": "reply", "after this reply": "reply"} {
		pattern := regexp.MustCompile(`(?i)^` + prefix + `(?:,\s*|\s+)(.+)$`)
		match := pattern.FindStringSubmatch(strings.TrimSpace(text))
		if len(match) != 2 {
			continue
		}
		action := strings.TrimSpace(match[1])
		// Validate a single safe leaf as well as chains; ambiguous deferred requests stay with the Agent.
		var plan []chainStep
		if strings.Contains(action, " and ") || strings.Contains(action, " then ") {
			plan = p.chainPlan(ctx, action, phrases)
		} else {
			name := phrases[NormalizePhrase(action)]
			if name == "" && p.semantic != nil {
				name = p.semantic.Match(ctx, action).Tool
			}
			if info, ok := p.registry.Info(name); ok && info.Permission != tool.PermissionConfirm {
				plan = []chainStep{{text: action, name: name}}
			}
		}
		if len(plan) == 0 {
			reply.Path = tool.PathAgent
			p.callAgent(ctx, req, text, Events{}, &reply)
			return reply, true
		}
		if p.deferred.Add(event, action) {
			reply.Text = "Queued for the end of " + event + ". Say cancel queued actions to cancel. It expires in one hour."
		} else {
			reply.Text = "The action queue is full. Cancel a queued action first."
		}
		return reply, true
	}
	return Reply{}, false
}

// Trigger marks matching requests ready while preserving their cancellation handles.
func (q *DeferredQueue) Trigger(event string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune()
	for i := range q.actions {
		if q.actions[i].Event == event {
			q.actions[i].ready = true
		}
	}
}

// NextReady consumes only the next request immediately before execution.
func (q *DeferredQueue) NextReady() (DeferredAction, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune()
	for i, action := range q.actions {
		if action.ready {
			q.actions = append(q.actions[:i], q.actions[i+1:]...)
			return action, true
		}
	}
	return DeferredAction{}, false
}
