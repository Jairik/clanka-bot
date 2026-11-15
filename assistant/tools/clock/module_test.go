package clock_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// liveZone opens a real temp database and builds the timezone seam with the
// shipped setting definition, as main does, so the setting layer is the real
// one and not a fake reader.
func liveZone(t *testing.T, env, configValue string) (*tz.Service, *settings.Service) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sett, err := settings.New(context.Background(), st, tz.Definition(configValue))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New(env, nil)
	zone.Bind(sett)
	return zone, sett
}

// TestClockToolsUseTimezoneSetting is the issue regression: with the setting
// at Asia/Tokyo, time.now and date.today report the Tokyo instant and date
// even though the injected clock and the process zone are UTC.
func TestClockToolsUseTimezoneSetting(t *testing.T) {
	// 22:30 UTC is 07:30 the next day in Tokyo.
	at := time.Date(2026, 11, 11, 22, 30, 0, 0, time.UTC)
	zone, sett := liveZone(t, "UTC", "")
	if _, err := sett.Set(context.Background(), tz.Key, "Asia/Tokyo"); err != nil {
		t.Fatalf("Set timezone: %v", err)
	}
	registry := clockRegistry(t, func() time.Time { return at }, zone)

	nowOut, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
	if err != nil {
		t.Fatalf("time.now: %v", err)
	}
	if !nowOut.Time.Equal(at) {
		t.Errorf("instant = %s, want the same instant %s", nowOut.Time, at)
	}
	if got := nowOut.Time.Location().String(); got != "Asia/Tokyo" {
		t.Fatalf("zone = %q, want Asia/Tokyo", got)
	}
	if h, m := nowOut.Time.Hour(), nowOut.Time.Minute(); h != 7 || m != 30 {
		t.Fatalf("wall time = %02d:%02d, want 07:30 Tokyo", h, m)
	}
	if _, offset := nowOut.Time.Zone(); offset != 9*60*60 {
		t.Fatalf("offset = %d, want +9h", offset)
	}
	if text, _ := registry.Speak(clock.Name, nowOut); text != "It's 7:30 AM." {
		t.Fatalf("time.now Speak = %q", text)
	}

	dateOut, err := executeClock[clock.DateOut](t, registry, clock.DateName, clock.In{})
	if err != nil {
		t.Fatalf("date.today: %v", err)
	}
	if got := dateOut.Date.Format("2006-01-02"); got != "2026-11-12" {
		t.Fatalf("date = %s, want 2026-11-12 (Tokyo has rolled over)", got)
	}
	if text, _ := registry.Speak(clock.DateName, dateOut); text != "Today is Thursday, 12 November 2026." {
		t.Fatalf("date.today Speak = %q", text)
	}
}

// TestClockToolsFollowTheLiveSetting proves the zone is read per call: a
// Dashboard write after registration changes the next answer with no restart
// (a registration-time snapshot would keep the old zone).
func TestClockToolsFollowTheLiveSetting(t *testing.T) {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	zone, sett := liveZone(t, "UTC", "")
	registry := clockRegistry(t, func() time.Time { return at }, zone)

	before, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
	if err != nil {
		t.Fatalf("time.now before: %v", err)
	}
	if got := before.Time.Location().String(); got != "UTC" {
		t.Fatalf("unset zone = %q, want UTC", got)
	}

	if _, err := sett.Set(context.Background(), tz.Key, "Asia/Tokyo"); err != nil {
		t.Fatalf("Set timezone: %v", err)
	}
	after, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
	if err != nil {
		t.Fatalf("time.now after: %v", err)
	}
	if got := after.Time.Location().String(); got != "Asia/Tokyo" {
		t.Fatalf("after Set zone = %q, want Asia/Tokyo", got)
	}
	dateOut, err := executeClock[clock.DateOut](t, registry, clock.DateName, clock.In{})
	if err != nil {
		t.Fatalf("date.today after: %v", err)
	}
	if got := dateOut.Date.Format("2006-01-02"); got != "2026-11-12" {
		t.Fatalf("after Set date = %s, want 2026-11-12", got)
	}

	if _, err := sett.Reset(context.Background(), tz.Key); err != nil {
		t.Fatalf("Reset timezone: %v", err)
	}
	reset, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
	if err != nil {
		t.Fatalf("time.now after reset: %v", err)
	}
	if got := reset.Time.Location().String(); got != "UTC" {
		t.Fatalf("after reset zone = %q, want UTC", got)
	}
}

// TestClockToolsObserveDST uses Europe/London, a DST-observing zone: the same
// wall-clock hour carries offset 0 in January and +1h in June, and the June
// date rolls over at local midnight while UTC is still the previous day.
func TestClockToolsObserveDST(t *testing.T) {
	tests := []struct {
		name     string
		at       time.Time
		wantZone string
		wantHour int
		wantDate string
	}{
		{
			name:     "winter GMT",
			at:       time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
			wantZone: "Europe/London",
			wantHour: 12,
			wantDate: "2026-01-15",
		},
		{
			name:     "summer BST",
			at:       time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			wantZone: "Europe/London",
			wantHour: 13,
			wantDate: "2026-06-15",
		},
		{
			name:     "BST midnight rollover",
			at:       time.Date(2026, 6, 30, 23, 30, 0, 0, time.UTC),
			wantZone: "Europe/London",
			wantHour: 0,
			wantDate: "2026-07-01",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone, sett := liveZone(t, "", "")
			if _, err := sett.Set(context.Background(), tz.Key, "Europe/London"); err != nil {
				t.Fatalf("Set timezone: %v", err)
			}
			at := tt.at
			registry := clockRegistry(t, func() time.Time { return at }, zone)

			nowOut, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
			if err != nil {
				t.Fatalf("time.now: %v", err)
			}
			if got := nowOut.Time.Location().String(); got != tt.wantZone {
				t.Fatalf("zone = %q, want %s", got, tt.wantZone)
			}
			if got := nowOut.Time.Hour(); got != tt.wantHour {
				t.Fatalf("hour = %d, want %d in London", got, tt.wantHour)
			}
			dateOut, err := executeClock[clock.DateOut](t, registry, clock.DateName, clock.In{})
			if err != nil {
				t.Fatalf("date.today: %v", err)
			}
			if got := dateOut.Date.Format("2006-01-02"); got != tt.wantDate {
				t.Fatalf("date = %s, want %s", got, tt.wantDate)
			}
		})
	}
}

// TestClockToolsFallBackThroughTZThenUTC checks the unset chain the tz seam
// documents: an empty setting uses TZ, and no setting and no TZ use UTC.
func TestClockToolsFallBackThroughTZThenUTC(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	t.Run("empty setting uses TZ", func(t *testing.T) {
		zone, _ := liveZone(t, "Europe/Berlin", "")
		registry := clockRegistry(t, func() time.Time { return at }, zone)
		out, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
		if err != nil {
			t.Fatalf("time.now: %v", err)
		}
		if got := out.Time.Location().String(); got != "Europe/Berlin" {
			t.Fatalf("zone = %q, want Europe/Berlin", got)
		}
		if got := out.Time.Hour(); got != 13 {
			t.Fatalf("hour = %d, want 13 in Berlin", got)
		}
	})

	t.Run("config layer beats TZ", func(t *testing.T) {
		zone, _ := liveZone(t, "Europe/Berlin", "Asia/Tokyo")
		registry := clockRegistry(t, func() time.Time { return at }, zone)
		out, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
		if err != nil {
			t.Fatalf("time.now: %v", err)
		}
		if got := out.Time.Location().String(); got != "Asia/Tokyo" {
			t.Fatalf("zone = %q, want Asia/Tokyo", got)
		}
	})

	t.Run("no setting and no TZ use UTC", func(t *testing.T) {
		zone, _ := liveZone(t, "", "")
		registry := clockRegistry(t, func() time.Time { return at }, zone)
		out, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
		if err != nil {
			t.Fatalf("time.now: %v", err)
		}
		if got := out.Time.Location(); got != time.UTC {
			t.Fatalf("zone = %q, want UTC", got)
		}
		if got := out.Time.Hour(); got != 12 {
			t.Fatalf("hour = %d, want 12 UTC", got)
		}
		dateOut, err := executeClock[clock.DateOut](t, registry, clock.DateName, clock.In{})
		if err != nil {
			t.Fatalf("date.today: %v", err)
		}
		if got := dateOut.Date.Format("2006-01-02"); got != "2026-01-15" {
			t.Fatalf("date = %s, want 2026-01-15", got)
		}
	})

	t.Run("nil zone uses UTC", func(t *testing.T) {
		registry := clockRegistry(t, func() time.Time { return at }, nil)
		out, err := executeClock[clock.Out](t, registry, clock.Name, clock.In{})
		if err != nil {
			t.Fatalf("time.now: %v", err)
		}
		if got := out.Time.Location(); got != time.UTC {
			t.Fatalf("zone = %q, want UTC", got)
		}
	})
}
