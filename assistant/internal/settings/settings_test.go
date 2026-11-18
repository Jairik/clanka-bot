package settings_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/connections"
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

// openSeeded opens a fresh store, writes raw rows before New reads them, and
// returns the service plus New's error so a fatal case can assert the policy.
func openSeeded(t *testing.T, rows map[string]string, defs ...settings.Definition) (*settings.Service, *store.Store, error) {
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
	seedRows(t, st.DB(), rows)
	svc, err := settings.New(context.Background(), st, defs...)
	return svc, st, err
}

// seedRows inserts stored overrides exactly as an earlier build wrote them,
// bypassing the current validator.
func seedRows(t *testing.T, db *sql.DB, rows map[string]string) {
	t.Helper()
	for key, raw := range rows {
		if _, err := db.Exec(
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			key, raw, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
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

func TestSetManyResetClearsAndWritesInOneBatch(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := openSettings(t,
		settings.String("a", "default", settings.WithConfig("config")),
		settings.String("b", "default", settings.WithConfig("config")),
	)
	if err := svc.SetMany(ctx, map[string]any{"a": "old", "b": "old"}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}
	var observed []string
	svc.SetObserver(func(keys []string) { observed = append(observed, keys...) })

	if err := svc.SetManyReset(ctx, map[string]any{"a": "new"}, []string{"b"}); err != nil {
		t.Fatalf("SetManyReset: %v", err)
	}
	if got := svc.String("a"); got != "new" {
		t.Errorf("a = %q, want new", got)
	}
	if got := svc.String("b"); got != "config" {
		t.Errorf("b = %q, want the config layer", got)
	}
	if len(observed) != 2 || observed[0] != "a" || observed[1] != "b" {
		t.Errorf("observed keys = %v, want one batch with a and b", observed)
	}

	// A key in both lists is a wiring error, and nothing is written.
	before := svc.String("a")
	if err := svc.SetManyReset(ctx, map[string]any{"a": "other"}, []string{"a"}); err == nil {
		t.Fatal("SetManyReset accepted a key in both values and reset")
	}
	if got := svc.String("a"); got != before {
		t.Errorf("a = %q after a rejected batch, want %q", got, before)
	}
	// An unknown reset key is rejected before the transaction.
	if err := svc.SetManyReset(ctx, nil, []string{"missing"}); !errors.Is(err, settings.ErrUnknownSetting) {
		t.Errorf("unknown reset error = %v, want ErrUnknownSetting", err)
	}
}

func TestNewRejectsAnInvalidConfigLayer(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	nonBlank := settings.WithValidator(func(value any) error {
		if value == "" {
			return errTestInvalid
		}
		return nil
	})
	tests := []struct {
		name string
		def  settings.Definition
	}{
		{"wrong type", settings.String("x", "default", settings.WithConfig(42))},
		{"validator", settings.String("y", "default", settings.WithConfig(""), nonBlank)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := settings.New(context.Background(), st, tt.def); err == nil {
				t.Fatal("New accepted an invalid config value; the config layer must stay fatal")
			}
		})
	}
}

// TestNewSkipsAStoredOverrideThatNoLongerValidates covers the reachable path
// in #170: a fresh install stored an empty dashboard.addr while the Dashboard
// was off, and the owner later enabled it.
func TestNewSkipsAStoredOverrideThatNoLongerValidates(t *testing.T) {
	cfg := config.Default()
	cfg.Dashboard.Owner = "owner@example.com"
	cfg.Dashboard.Addr = "127.0.0.1:8082"
	svc, _, err := openSeeded(t, map[string]string{connections.DashboardAddrKey: `""`},
		connections.Definitions(config.Default(), cfg)...)
	if err != nil {
		t.Fatalf("New failed on a stale stored override: %v", err)
	}
	value, ok := svc.Get(connections.DashboardAddrKey)
	if !ok {
		t.Fatal("dashboard.addr is not defined")
	}
	if value.Value != cfg.Dashboard.Addr || value.Source != settings.SourceConfig {
		t.Fatalf("dashboard.addr = %+v, want the config layer", value)
	}
	ignored := svc.Ignored()
	if len(ignored) != 1 || ignored[0].Key != connections.DashboardAddrKey {
		t.Fatalf("Ignored() = %+v, want the skipped dashboard.addr", ignored)
	}
	if !strings.Contains(ignored[0].Reason, "required") {
		t.Errorf("reason = %q, want the validator's explanation", ignored[0].Reason)
	}

	// A valid write replaces the ignored row; it no longer needs a warning.
	if _, err := svc.Set(context.Background(), connections.DashboardAddrKey, "127.0.0.1:9090"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := svc.Ignored(); len(got) != 0 {
		t.Fatalf("Ignored() after a valid write = %+v, want none", got)
	}
	// Reset drops back to the config layer and keeps the warning clear.
	if _, err := svc.Reset(context.Background(), connections.DashboardAddrKey); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	value, _ = svc.Get(connections.DashboardAddrKey)
	if value.Value != cfg.Dashboard.Addr || value.Source != settings.SourceConfig {
		t.Fatalf("after Reset = %+v, want the config layer", value)
	}
	if got := svc.Ignored(); len(got) != 0 {
		t.Fatalf("Ignored() after Reset = %+v, want none", got)
	}
}

// TestIgnoredOverrideWarningOmitsValues keeps interpolated validator data out of logs.
func TestIgnoredOverrideWarningOmitsValues(t *testing.T) {
	const marker = "private-validator-marker"
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	def := settings.String("test.setting", "safe", settings.WithValidator(func(value any) error {
		if value == marker {
			return fmt.Errorf("invalid setting value %q", value)
		}
		return nil
	}))
	svc, _, err := openSeeded(t, map[string]string{"test.setting": `"` + marker + `"`}, def)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("warning exposed the stored value: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "test.setting") {
		t.Fatalf("warning omitted the setting key: %s", logs.String())
	}
	// The authenticated recovery view still explains why the row was ignored.
	ignored := svc.Ignored()
	if len(ignored) != 1 || !strings.Contains(ignored[0].Reason, marker) {
		t.Fatalf("Ignored = %+v, want the owner-facing validator detail", ignored)
	}
}

// TestNewKeepsADecodeFailureFatal pins the documented policy: a row that cannot
// be decoded is corrupt data, and skipping tools.<name>.enabled would fall back
// to the code default true and silently re-enable a Tool. The error names the
// SQLite row because the Settings page cannot start to reset it.
func TestNewKeepsADecodeFailureFatal(t *testing.T) {
	key := "tools.clock.enabled"
	_, _, err := openSeeded(t, map[string]string{key: `"yes"`}, settings.Bool(key, true))
	if err == nil {
		t.Fatal("New accepted a stored override of the wrong type")
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("error does not name the row: %v", err)
	}
	if !strings.Contains(err.Error(), "DELETE FROM settings") {
		t.Errorf("error does not name the recovery step: %v", err)
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

// TestObserverFanoutAndAddObserver tests that AddObserver fans out notifications
// alongside SetObserver, and clearing SetObserver leaves AddObserver intact.
func TestObserverFanoutAndAddObserver(t *testing.T) {
	svc, _, _ := openSettings(t,
		settings.String("key.a", "val_a"),
		settings.String("key.b", "val_b"),
	)
	ctx := context.Background()

	var mu sync.Mutex
	var singleEvents []string
	var fanoutEvents1 []string
	var fanoutEvents2 []string

	svc.SetObserver(func(keys []string) {
		mu.Lock()
		singleEvents = append(singleEvents, keys...)
		mu.Unlock()
	})
	svc.AddObserver(func(keys []string) {
		mu.Lock()
		fanoutEvents1 = append(fanoutEvents1, keys...)
		mu.Unlock()
	})
	svc.AddObserver(func(keys []string) {
		mu.Lock()
		fanoutEvents2 = append(fanoutEvents2, keys...)
		mu.Unlock()
	})

	if _, err := svc.Set(ctx, "key.a", "new_a"); err != nil {
		t.Fatalf("Set key.a: %v", err)
	}

	mu.Lock()
	if !reflect.DeepEqual(singleEvents, []string{"key.a"}) {
		t.Fatalf("singleEvents = %v, want [key.a]", singleEvents)
	}
	if !reflect.DeepEqual(fanoutEvents1, []string{"key.a"}) {
		t.Fatalf("fanoutEvents1 = %v, want [key.a]", fanoutEvents1)
	}
	if !reflect.DeepEqual(fanoutEvents2, []string{"key.a"}) {
		t.Fatalf("fanoutEvents2 = %v, want [key.a]", fanoutEvents2)
	}
	mu.Unlock()

	// Clearing SetObserver does not affect AddObserver registrations.
	svc.SetObserver(nil)

	if _, err := svc.Set(ctx, "key.b", "new_b"); err != nil {
		t.Fatalf("Set key.b: %v", err)
	}

	mu.Lock()
	if !reflect.DeepEqual(singleEvents, []string{"key.a"}) {
		t.Fatalf("singleEvents after clear = %v, want unchanged [key.a]", singleEvents)
	}
	if !reflect.DeepEqual(fanoutEvents1, []string{"key.a", "key.b"}) {
		t.Fatalf("fanoutEvents1 = %v, want [key.a key.b]", fanoutEvents1)
	}
	if !reflect.DeepEqual(fanoutEvents2, []string{"key.a", "key.b"}) {
		t.Fatalf("fanoutEvents2 = %v, want [key.a key.b]", fanoutEvents2)
	}
	mu.Unlock()

	// Fanout helper calls each observer in order.
	var combined []string
	fn := settings.Fanout(
		func(keys []string) { combined = append(combined, "first:"+strings.Join(keys, ",")) },
		func(keys []string) { combined = append(combined, "second:"+strings.Join(keys, ",")) },
	)
	fn([]string{"x", "y"})
	wantCombined := []string{"first:x,y", "second:x,y"}
	if !reflect.DeepEqual(combined, wantCombined) {
		t.Fatalf("Fanout = %v, want %v", combined, wantCombined)
	}
}
