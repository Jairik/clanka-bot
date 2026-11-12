package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openService opens the real store and returns a secrets service over it.
func openService(t *testing.T, names ...string) (*secrets.Service, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	svc, err := secrets.New(context.Background(), st.DB(), names)
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	return svc, st
}

func TestEnvironmentBootstrapsAndAStoredValueWins(t *testing.T) {
	const name = "SECRETS_TEST_OPENROUTER_KEY"
	t.Setenv(name, "env-key-123456")
	svc, _ := openService(t, name)
	ctx := context.Background()

	value, ok := svc.Get(name)
	if !ok || value != "env-key-123456" {
		t.Fatalf("Get before Set = %q, %v; want the environment value", value, ok)
	}
	meta, ok := svc.Metadata(name)
	if !ok {
		t.Fatal("Metadata: declared name missing")
	}
	if !meta.Set || meta.Source != secrets.SourceEnv {
		t.Fatalf("metadata before Set = %+v, want set from env", meta)
	}
	if meta.Last4 != "3456" {
		t.Errorf("last4 = %q, want 3456", meta.Last4)
	}
	if meta.UpdatedAt != nil {
		t.Errorf("updated_at = %v, want none for the environment layer", meta.UpdatedAt)
	}

	stored, err := svc.Set(ctx, name, "stored-key-abcdef")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if stored.Source != secrets.SourceStore || !stored.Set || stored.UpdatedAt == nil {
		t.Fatalf("metadata after Set = %+v, want the stored layer with a timestamp", stored)
	}
	if value, ok := svc.Get(name); !ok || value != "stored-key-abcdef" {
		t.Fatalf("Get after Set = %q, %v; want the stored value over the environment", value, ok)
	}

	cleared, err := svc.Clear(ctx, name)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if cleared.Source != secrets.SourceEnv || cleared.UpdatedAt != nil {
		t.Fatalf("metadata after Clear = %+v, want the environment layer again", cleared)
	}
	if value, ok := svc.Get(name); !ok || value != "env-key-123456" {
		t.Fatalf("Get after Clear = %q, %v; want the environment fallback", value, ok)
	}
}

func TestStoredSecretWorksWithNoEnvironmentValue(t *testing.T) {
	const name = "SECRETS_TEST_STORE_ONLY"
	t.Setenv(name, "")
	svc, _ := openService(t, name)
	ctx := context.Background()

	if _, ok := svc.Get(name); ok {
		t.Fatal("Get with no store row and an empty environment returned a value")
	}
	if _, err := svc.Set(ctx, name, "store-only-abcdef"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if value, ok := svc.Get(name); !ok || value != "store-only-abcdef" {
		t.Fatalf("Get = %q, %v; want the stored value with no environment", value, ok)
	}
}

func TestMetadataNeverExposesTheValueAndHidesShortLastFour(t *testing.T) {
	const name = "SECRETS_TEST_SHORT"
	t.Setenv(name, "")
	svc, _ := openService(t, name)
	ctx := context.Background()

	for _, value := range []string{"abcd", "abcdefgh"} {
		meta, err := svc.Set(ctx, name, value)
		if err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
		if meta.Last4 != "" {
			t.Errorf("Set(%q) last4 = %q, want hidden for a short secret", value, meta.Last4)
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		if strings.Contains(string(raw), value) {
			t.Fatalf("metadata JSON %s contains the secret value %q", raw, value)
		}
	}

	meta, err := svc.Set(ctx, name, "abcdefghijkl")
	if err != nil {
		t.Fatalf("Set long value: %v", err)
	}
	if meta.Last4 != "ijkl" {
		t.Errorf("last4 = %q, want ijkl for a 12-character secret", meta.Last4)
	}
}

func TestSetRejectsEmptyAndUndeclaredSecrets(t *testing.T) {
	const name = "SECRETS_TEST_DECLARED"
	t.Setenv(name, "")
	svc, _ := openService(t, name)
	ctx := context.Background()

	if _, err := svc.Set(ctx, name, "   "); !errors.Is(err, secrets.ErrEmptyValue) {
		t.Fatalf("Set empty error = %v, want ErrEmptyValue", err)
	}
	if _, err := svc.Set(ctx, "SECRETS_TEST_UNDECLARED", "value-12345678"); !errors.Is(err, secrets.ErrUnknownSecret) {
		t.Fatalf("Set undeclared error = %v, want ErrUnknownSecret", err)
	}
	if _, ok := svc.Metadata("SECRETS_TEST_UNDECLARED"); ok {
		t.Fatal("Metadata returned an undeclared secret")
	}
}

func TestListReportsEveryDeclaredSecretWithoutValues(t *testing.T) {
	const (
		setName   = "SECRETS_TEST_LIST_SET"
		unsetName = "SECRETS_TEST_LIST_UNSET"
	)
	t.Setenv(setName, "")
	t.Setenv(unsetName, "")
	svc, _ := openService(t, setName, unsetName, setName)
	ctx := context.Background()

	if _, err := svc.Set(ctx, setName, "list-value-abcdef"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	list := svc.List()
	if len(list) != 2 {
		t.Fatalf("List length = %d, want 2 (declarations deduplicated): %+v", len(list), list)
	}
	if list[0].Name != setName || !list[0].Set || list[0].Source != secrets.SourceStore {
		t.Errorf("first entry = %+v, want the stored secret", list[0])
	}
	if list[1].Name != unsetName || list[1].Set || list[1].Source != secrets.SourceNone {
		t.Errorf("second entry = %+v, want an unset secret", list[1])
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	if strings.Contains(string(raw), "list-value-abcdef") {
		t.Fatalf("list JSON contains the secret value: %s", raw)
	}
}

func TestSecretsSurviveReopenAndKeepTheDatabaseOwnerOnly(t *testing.T) {
	const name = "SECRETS_TEST_REOPEN"
	t.Setenv(name, "")
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	ctx := context.Background()

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first store.Open: %v", err)
	}
	svc, err := secrets.New(ctx, first.DB(), []string{name})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	if _, err := svc.Set(ctx, name, "reopen-value-abcdef"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second store.Open: %v", err)
	}
	defer second.Close()
	reopened, err := secrets.New(ctx, second.DB(), []string{name})
	if err != nil {
		t.Fatalf("secrets.New after reopen: %v", err)
	}
	if value, ok := reopened.Get(name); !ok || value != "reopen-value-abcdef" {
		t.Fatalf("Get after reopen = %q, %v; want the stored value", value, ok)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode = %o, want 600", got)
	}
	// WAL/SHM carry the same secret bytes and must keep the database's mode.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, entry := range entries {
		entryInfo, err := entry.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", entry.Name(), err)
		}
		if got := entryInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", entry.Name(), got)
		}
	}
}

func TestConcurrentReadsAndWritesAreSafe(t *testing.T) {
	const name = "SECRETS_TEST_CONCURRENT"
	t.Setenv(name, "")
	svc, _ := openService(t, name)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := svc.Set(ctx, name, "concurrent-value-abcdef"); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				if value, ok := svc.Get(name); !ok || value != "concurrent-value-abcdef" {
					t.Errorf("Get = %q, %v", value, ok)
					return
				}
				_ = svc.List()
			}
		}(i)
	}
	wg.Wait()
}
