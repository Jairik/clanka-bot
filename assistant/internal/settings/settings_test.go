package settings_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openSettings opens a fresh database and settings service at the public seam.
func openSettings(t *testing.T, defs ...settings.Definition) (*settings.Service, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	svc, err := settings.New(context.Background(), st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc, st, path
}

func TestEffectiveValueResolvesOverrideThenConfigThenDefault(t *testing.T) {
	svc, _, _ := openSettings(t,
		settings.String("timezone", "UTC"),
		settings.String("agent.filler", "One moment.", settings.WithConfig("Just a second.")),
	)

	value, ok := svc.Get("timezone")
	if !ok {
		t.Fatal("timezone is not defined")
	}
	if value.Value != "UTC" || value.Source != settings.SourceDefault || value.Config != nil {
		t.Fatalf("timezone = %+v, want the code default", value)
	}

	value, ok = svc.Get("agent.filler")
	if !ok {
		t.Fatal("agent.filler is not defined")
	}
	if value.Value != "Just a second." || value.Source != settings.SourceConfig || value.Config != "Just a second." {
		t.Fatalf("agent.filler = %+v, want the config layer", value)
	}

	if _, err := svc.Set(context.Background(), "agent.filler", "Hold on."); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, _ = svc.Get("agent.filler")
	if value.Value != "Hold on." || value.Source != settings.SourceOverride {
		t.Fatalf("agent.filler = %+v, want the stored override", value)
	}
	if value.Override != "Hold on." || value.Config != "Just a second." || value.Default != "One moment." {
		t.Fatalf("agent.filler layers = %+v, want every layer reported", value)
	}
}

func TestTypedSettingsRoundTrip(t *testing.T) {
	svc, _, _ := openSettings(t,
		settings.Bool("quiet.enabled", true),
		settings.Int("ring.max", 5),
		settings.Duration("snooze", 9*time.Minute),
		settings.Float("name_match.threshold", 0.6),
		settings.Strings("semantic.joiners", []string{"and", "then"}),
	)
	ctx := context.Background()

	values := map[string]any{
		"quiet.enabled":        false,
		"ring.max":             7,
		"snooze":               30 * time.Second,
		"name_match.threshold": 0.75,
		"semantic.joiners":     []string{"and"},
	}
	for key, want := range values {
		value, err := svc.Set(ctx, key, want)
		if err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
		if !reflect.DeepEqual(value.Value, want) {
			t.Errorf("%s = %#v, want %#v", key, value.Value, want)
		}
	}
	if got := svc.Bool("quiet.enabled"); got {
		t.Error("Bool(quiet.enabled) = true, want the stored false")
	}
	if got := svc.Int("ring.max"); got != 7 {
		t.Errorf("Int(ring.max) = %d, want 7", got)
	}
	if got := svc.Duration("snooze"); got != 30*time.Second {
		t.Errorf("Duration(snooze) = %s, want 30s", got)
	}
	if got := svc.Float("name_match.threshold"); got != 0.75 {
		t.Errorf("Float(name_match.threshold) = %v, want 0.75", got)
	}
	if got := svc.Strings("semantic.joiners"); len(got) != 1 || got[0] != "and" {
		t.Errorf("Strings(semantic.joiners) = %v, want [and]", got)
	}
}

func TestOverrideSurvivesAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first store.Open: %v", err)
	}
	defs := []settings.Definition{settings.String("timezone", "UTC")}
	first, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("first settings.New: %v", err)
	}
	if _, err := first.Set(ctx, "timezone", "Europe/Berlin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// A restart means a new store handle and service over the same file.
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second store.Open: %v", err)
	}
	defer st.Close()
	second, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("second settings.New: %v", err)
	}
	value, ok := second.Get("timezone")
	if !ok || value.Value != "Europe/Berlin" || value.Source != settings.SourceOverride {
		t.Fatalf("timezone after reopen = %+v, want the stored override", value)
	}
}

func TestResetRestoresTheConfigOrDefaultLayer(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := openSettings(t,
		settings.String("agent.filler", "One moment.", settings.WithConfig("Just a second.")),
	)
	if _, err := svc.Set(ctx, "agent.filler", "Hold on."); err != nil {
		t.Fatalf("Set: %v", err)
	}

	value, err := svc.Reset(ctx, "agent.filler")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if value.Value != "Just a second." || value.Source != settings.SourceConfig || value.Override != nil {
		t.Fatalf("after Reset = %+v, want the config layer and no override", value)
	}
	// Resetting again is a no-op, not an error.
	if _, err := svc.Reset(ctx, "agent.filler"); err != nil {
		t.Fatalf("second Reset: %v", err)
	}
}

func TestSetRejectsInvalidValuesAndKeepsTheOldOne(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := openSettings(t,
		settings.String("tool.permission", "read", settings.WithValidator(func(value any) error {
			level, _ := value.(string)
			switch level {
			case "read", "act", "confirm":
				return nil
			default:
				return errTestInvalid
			}
		})),
	)
	if _, err := svc.Set(ctx, "tool.permission", "admin"); err == nil {
		t.Fatal("Set accepted an invalid value")
	}
	if got := svc.String("tool.permission"); got != "read" {
		t.Errorf("tool.permission = %q after a rejected Set, want read", got)
	}
	if _, err := svc.Set(ctx, "missing", true); err == nil {
		t.Fatal("Set accepted an undefined key")
	}
	if _, err := svc.Set(ctx, "tool.permission", 42); err == nil {
		t.Fatal("Set accepted a value of the wrong JSON type")
	}
}

func TestSetManyIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := openSettings(t,
		settings.String("a", "default"),
		settings.String("b", "default", settings.WithValidator(func(value any) error {
			if value == "bad" {
				return errTestInvalid
			}
			return nil
		})),
	)
	err := svc.SetMany(ctx, map[string]any{"a": "changed", "b": "bad"})
	if err == nil {
		t.Fatal("SetMany accepted a batch with one invalid value")
	}
	if got := svc.String("a"); got != "default" {
		t.Errorf("a = %q after a rejected batch, want default", got)
	}
}

func TestNewRejectsAnInvalidConfigLayer(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if _, err := settings.New(context.Background(), st, settings.String("x", "default",
		settings.WithConfig(42))); err == nil {
		t.Fatal("New accepted a config value of the wrong type")
	}
}

func TestDurationValuesReportAsStrings(t *testing.T) {
	svc, _, _ := openSettings(t, settings.Duration("snooze", 9*time.Minute))
	if _, err := svc.Set(context.Background(), "snooze", 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, _ := svc.Get("snooze")
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["value"] != "30s" || decoded["default"] != "9m0s" || decoded["override"] != "30s" {
		t.Fatalf("duration JSON = %s", raw)
	}
}

func TestConcurrentReadsAndWritesAreSafe(t *testing.T) {
	svc, _, _ := openSettings(t, settings.String("timezone", "UTC"))
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if i%2 == 0 {
					if _, err := svc.Set(ctx, "timezone", "Europe/Berlin"); err != nil {
						t.Errorf("Set: %v", err)
						return
					}
					if _, err := svc.Reset(ctx, "timezone"); err != nil {
						t.Errorf("Reset: %v", err)
						return
					}
				} else {
					if got := svc.String("timezone"); got != "UTC" && got != "Europe/Berlin" {
						t.Errorf("String(timezone) = %q", got)
						return
					}
					_ = svc.List()
				}
			}
		}(i)
	}
	wg.Wait()
}

var errTestInvalid = &testError{}

type testError struct{}

func (*testError) Error() string { return "test: invalid value" }

func TestObserverReportsCommittedChangesOnly(t *testing.T) {
	svc, _, _ := openSettings(t,
		settings.String("agent.filler", "One moment."),
		settings.Strings("agent.request_prefixes", []string{"ask the agent"}),
	)
	ctx := context.Background()

	var mu sync.Mutex
	var batches [][]string
	svc.SetObserver(func(keys []string) {
		mu.Lock()
		batches = append(batches, append([]string(nil), keys...))
		mu.Unlock()
	})

	// Every successful write reports its keys; batching keeps the pair atomic.
	if _, err := svc.Set(ctx, "agent.filler", "Hold on."); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.SetMany(ctx, map[string]any{
		"agent.filler":           "Just a second.",
		"agent.request_prefixes": []string{"ask the agent", "agent"},
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	if _, err := svc.Reset(ctx, "agent.filler"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if err := svc.ResetMany(ctx, "agent.request_prefixes", "agent.filler"); err != nil {
		t.Fatalf("ResetMany: %v", err)
	}

	mu.Lock()
	got := batches
	mu.Unlock()
	want := [][]string{
		{"agent.filler"},
		{"agent.filler", "agent.request_prefixes"},
		{"agent.filler"},
		{"agent.filler", "agent.request_prefixes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observer batches = %v, want %v", got, want)
	}

	// A failed type check leaves the snapshot and the observer untouched.
	before := len(got)
	if _, err := svc.Set(ctx, "agent.filler", 42); err == nil {
		t.Fatal("Set accepted a non-string value")
	}
	mu.Lock()
	after := len(batches)
	mu.Unlock()
	if after != before {
		t.Fatalf("observer saw %d batches after a rejected write, want %d", after, before)
	}
}

func TestEmptyStringListStaysAnEmptyList(t *testing.T) {
	svc, _, _ := openSettings(t, settings.Strings("wording.list", []string{}))
	value, ok := svc.Get("wording.list")
	if !ok {
		t.Fatal("wording.list is not defined")
	}
	if list, ok := value.Value.([]string); !ok || list == nil || len(list) != 0 {
		t.Fatalf("default = %#v, want a non-nil empty list", value.Value)
	}

	if _, err := svc.Set(context.Background(), "wording.list", []string{}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, _ = svc.Get("wording.list")
	if list, ok := value.Value.([]string); !ok || list == nil || len(list) != 0 {
		t.Fatalf("after Set = %#v, want a non-nil empty list", value.Value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"value":[]`) {
		t.Fatalf("JSON = %s, want an empty array", raw)
	}
}
