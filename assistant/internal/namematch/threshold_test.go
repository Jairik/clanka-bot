package namematch_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openSettings opens a fresh database and the settings service with the same
// definition main builds.
func openSettings(t *testing.T, configValue float64) *settings.Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := settings.New(context.Background(), st, namematch.Definition(configValue))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc
}

func TestThresholdIsALiveSetting(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t, 0.62)

	value, ok := svc.Get(namematch.KeyThreshold)
	if !ok {
		t.Fatal("name_match.threshold is not defined")
	}
	if value.Value != 0.62 || value.Source != settings.SourceConfig || value.Kind != settings.Live {
		t.Fatalf("threshold = %+v, want the live config value 0.62", value)
	}
	if value.Default != namematch.DefaultThreshold {
		t.Fatalf("default = %v, want %v", value.Default, namematch.DefaultThreshold)
	}

	if _, err := svc.Set(ctx, namematch.KeyThreshold, 0.75); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := svc.Float(namematch.KeyThreshold); got != 0.75 {
		t.Fatalf("Float = %v, want the override 0.75", got)
	}
	if _, err := svc.Reset(ctx, namematch.KeyThreshold); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := svc.Float(namematch.KeyThreshold); got != 0.62 {
		t.Fatalf("Float after reset = %v, want the config 0.62", got)
	}
}

func TestThresholdRejectsOutOfRangeValues(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t, 0.60)
	for _, value := range []float64{0, -0.1, 1.01, 2} {
		if _, err := svc.Set(ctx, namematch.KeyThreshold, value); err == nil {
			t.Errorf("Set(%v) accepted an unusable threshold", value)
		}
	}
	if _, err := svc.Set(ctx, namematch.KeyThreshold, "0.7"); err == nil {
		t.Error("Set accepted a string for a numeric setting")
	}
	if got := svc.Float(namematch.KeyThreshold); got != 0.60 {
		t.Fatalf("threshold changed to %v after rejected writes", got)
	}
}

func TestThresholdHolderReadsTheSettingsService(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t, 0.60)
	threshold := namematch.NewThreshold(0.60)

	if got := threshold.Get(); got != 0.60 {
		t.Fatalf("before Bind Get = %v, want the fallback", got)
	}
	threshold.Bind(svc)
	if got := threshold.Get(); got != 0.60 {
		t.Fatalf("after Bind Get = %v, want 0.60", got)
	}

	if _, err := svc.Set(ctx, namematch.KeyThreshold, 0.81); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := threshold.Get(); got != 0.81 {
		t.Fatalf("after override Get = %v, want 0.81 without a second observer", got)
	}
}

func TestThresholdHolderIsSafeWhileSettingsChange(t *testing.T) {
	ctx := context.Background()
	svc := openSettings(t, 0.60)
	threshold := namematch.NewThreshold(0.60)
	threshold.Bind(svc)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if got := threshold.Get(); got <= 0 || got > 1 {
					t.Errorf("Get = %v outside (0, 1]", got)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			value := 0.60 + float64(j%20)/100
			if _, err := svc.Set(ctx, namematch.KeyThreshold, value); err != nil {
				t.Errorf("Set: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
