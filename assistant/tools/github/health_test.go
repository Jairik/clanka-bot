package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// healthClock is an injectable clock for the health memos, so the TTL and the
// stale path are exercised without waiting minutes.
type healthClock struct{ now time.Time }

func (c *healthClock) Now() time.Time { return c.now }

func (c *healthClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newHealthClock() *healthClock {
	return &healthClock{now: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
}

// shortBudget is a context with only a sliver of the shared refresh budget left.
func shortBudget(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// healthModule registers the module against server with a fixed token and clock.
func healthModule(t *testing.T, module *Module, secrets tool.SecretResolver, clock *healthClock) *Module {
	t.Helper()
	module.now = clock.Now
	registerModule(t, module, secrets)
	return module
}

// acceptedToken answers GET /user the way GitHub does.
func acceptedToken(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"login":"octocat"}`)
}

// rejectedToken answers the way GitHub does for a revoked or expired token.
func rejectedToken(w http.ResponseWriter, _ *http.Request, _ []byte) {
	http.Error(w, "bad credentials", http.StatusUnauthorized)
}

func TestTokenHealthIsMemoisedWithinItsTTL(t *testing.T) {
	fake := &fakeGitHub{handler: acceptedToken}
	server := fake.start(t)
	clock := newHealthClock()
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret(fineToken), clock)

	// The Dashboard polls every minute; one answer covers five of them.
	for i := 0; i < 5; i++ {
		if err := module.Health(context.Background()); err != nil {
			t.Fatalf("Health %d: %v", i, err)
		}
		clock.advance(time.Minute)
	}
	if got := fake.count(http.MethodGet, "/user"); got != 1 {
		t.Fatalf("/user requests = %d, want 1 while the answer is fresh", got)
	}

	clock.advance(time.Second) // just past healthOKTTL
	if err := module.Health(context.Background()); err != nil {
		t.Fatalf("Health after the TTL: %v", err)
	}
	if got := fake.count(http.MethodGet, "/user"); got != 2 {
		t.Fatalf("/user requests = %d, want a live check once the answer is stale", got)
	}
}

func TestTokenHealthKeepsAHealthyTokenWhenTheSnapshotIsShortOfTime(t *testing.T) {
	fake := &fakeGitHub{handler: acceptedToken}
	server := fake.start(t)
	clock := newHealthClock()
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret(fineToken), clock)

	if err := module.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	// The memo expired, but this refresh has only a sliver of its shared
	// budget left. It must reuse the known answer, not report the page's own
	// deadline as a GitHub failure.
	clock.advance(healthOKTTL + time.Minute)
	if err := module.Health(shortBudget(t)); err != nil {
		t.Fatalf("Health with a spent budget = %v, want the last known healthy answer", err)
	}
	if got := fake.count(http.MethodGet, "/user"); got != 1 {
		t.Fatalf("/user requests = %d, want the memo to be reused", got)
	}
}

func TestTokenHealthReportsAStaleRejectionWithItsAge(t *testing.T) {
	fake := &fakeGitHub{handler: rejectedToken}
	server := fake.start(t)
	clock := newHealthClock()
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret(fineToken), clock)

	err := module.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Health = %v, want the token rejection", err)
	}

	// A real 401 is never hidden: once the failure memo expires the next
	// refresh reports it again, labelled with its age, when it has no budget
	// for a live check.
	clock.advance(healthFailTTL + time.Second)
	err = module.Health(shortBudget(t))
	if err == nil || !strings.Contains(err.Error(), "rejected") || !strings.Contains(err.Error(), "last checked") {
		t.Fatalf("Health with a spent budget = %v, want the stale rejection and its age", err)
	}
	if got := fake.count(http.MethodGet, "/user"); got != 1 {
		t.Fatalf("/user requests = %d, want no live check with too little budget", got)
	}
}

func TestTokenHealthDoesNotRememberASpentDeadline(t *testing.T) {
	// The first request is deliberately slower than the snapshot budget; every
	// later one answers. The counter is guarded, so the handler never mutates.
	var mu sync.Mutex
	firstRequest := true
	slow := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		slow := firstRequest
		firstRequest = false
		mu.Unlock()
		if slow {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		acceptedToken(w, r, nil)
	}}
	server := slow.start(t)
	clock := newHealthClock()
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret(fineToken), clock)

	// Nothing is known yet and the snapshot is nearly spent: say what really
	// happened instead of blaming the token.
	err := module.Health(shortBudget(t))
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("Health with no answer and a spent budget = %v, want the budget message", err)
	}
	if strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Health = %v, want no claim that the token was rejected", err)
	}

	// The timeout must not be remembered: with real budget the check answers.
	if err := module.Health(context.Background()); err != nil {
		t.Fatalf("Health after a budget timeout = %v, want a fresh live check", err)
	}
}

func TestTokenHealthNeverHidesARemovedSecret(t *testing.T) {
	fake := &fakeGitHub{handler: acceptedToken}
	server := fake.start(t)
	clock := newHealthClock()
	values := map[string]string{SecretName: fineToken}
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}), namedSecrets(values), clock)

	if err := module.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	// Deleting the token in the Dashboard is local state; the memo must not
	// keep reporting the old success.
	delete(values, SecretName)
	if err := module.Health(context.Background()); err == nil || !strings.Contains(err.Error(), SecretName) {
		t.Fatalf("Health after the secret was removed = %v, want the missing-secret error", err)
	}
}

func TestTokenHealthIsSafeForConcurrentRefreshes(t *testing.T) {
	fake := &fakeGitHub{handler: acceptedToken}
	server := fake.start(t)
	clock := newHealthClock()
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret(fineToken), clock)

	// The Health page and the system.status Tool can refresh at once.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = module.Health(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Health %d = %v", i, err)
		}
	}
}

func TestNotificationsHealthIsMemoisedToo(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/notifications" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}}
	server := fake.start(t)
	clock := newHealthClock()
	module := healthModule(t, testConfig(t, map[string]any{"base_url": server.URL}),
		namedSecrets(map[string]string{SecretName: fineToken, NotificationsSecretName: classicToken}), clock)

	first := module.AuxHealth(context.Background())
	if len(first) != 1 || first[0].State != tool.StatusOK {
		t.Fatalf("AuxHealth = %+v, want one ok entry", first)
	}
	clock.advance(healthOKTTL + time.Minute)
	// A refresh with too little budget reuses the answer but says how old it is.
	second := module.AuxHealth(shortBudget(t))
	if len(second) != 1 || second[0].State != tool.StatusOK {
		t.Fatalf("AuxHealth with a spent budget = %+v, want the reused ok entry", second)
	}
	if !strings.Contains(second[0].Detail, "last checked") {
		t.Fatalf("AuxHealth detail = %q, want the stale age", second[0].Detail)
	}
	if got := fake.count(http.MethodGet, "/notifications"); got != 1 {
		t.Fatalf("/notifications requests = %d, want the memo to be reused", got)
	}
}
