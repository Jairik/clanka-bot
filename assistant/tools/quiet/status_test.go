package quiet_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	quiettools "github.com/Jairik/clanka-bot/assistant/tools/quiet"
)

// fakeQuietReader is a minimal settings.Reader that returns a fixed map, so the
// read path runs without a store or the Dashboard writer.
type fakeQuietReader struct{ values map[string]any }

func (r fakeQuietReader) Get(key string) (settings.Value, bool) {
	v, ok := r.values[key]
	if !ok {
		return settings.Value{}, false
	}
	return settings.Value{Key: key, Value: v}, true
}

const (
	statusEnabledKey = "quiet.enabled"
	statusStartKey   = "quiet.start"
	statusEndKey     = "quiet.end"
	statusVolumeKey  = "quiet.volume"
	statusDNDKey     = "quiet.dnd_until"
)

func newStatusService(t *testing.T, at time.Time, values map[string]any) *quiet.Service {
	t.Helper()
	return quiet.New(quiet.Options{
		Reader:   fakeQuietReader{values: values},
		Now:      func() time.Time { return at },
		Location: func() *time.Location { return time.UTC },
	})
}

// TestStatusSpeak checks the four spoken states from the issue, plus the awake
// bypass which is an "enabled but not quiet now" case.
func TestStatusSpeak(t *testing.T) {
	tool := quiettools.NewStatus(nil)
	cases := []struct {
		name string
		out  quiettools.StatusOut
		want string
	}{
		{
			name: "off",
			out:  quiettools.StatusOut{Enabled: false, Reason: string(quiet.ReasonOff)},
			want: "Quiet hours are off.",
		},
		{
			name: "in window",
			out:  quiettools.StatusOut{Enabled: true, Start: "22:30", End: "07:00", InWindow: true, Reason: string(quiet.ReasonWindow)},
			want: "It's quiet until 7:00 am. Quiet hours run from 10:30 pm to 7:00 am.",
		},
		{
			name: "outside window",
			out:  quiettools.StatusOut{Enabled: true, Start: "22:30", End: "07:00", Reason: string(quiet.ReasonOff)},
			want: "Quiet hours run from 10:30 pm to 7:00 am; it's not quiet now.",
		},
		{
			name: "awake bypass",
			out:  quiettools.StatusOut{Enabled: true, Start: "22:30", End: "07:00", Reason: string(quiet.ReasonAwake)},
			want: "Quiet hours run from 10:30 pm to 7:00 am; it's not quiet now.",
		},
		{
			name: "do not disturb",
			out:  quiettools.StatusOut{Reason: string(quiet.ReasonDND), DNDUntil: time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)},
			want: "Do not disturb until 2:00 pm.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tool.Speak(tc.out); got != tc.want {
				t.Fatalf("Speak = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatusHandlerOff covers the off state (no window, no DND).
func TestStatusHandlerOff(t *testing.T) {
	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	svc := newStatusService(t, at, map[string]any{
		statusEnabledKey: false,
		statusStartKey:   "22:30",
		statusEndKey:     "07:00",
		statusVolumeKey:  35,
	})
	out, err := quiettools.NewStatus(svc).Handler(context.Background(), quiettools.StatusIn{})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if out.Enabled || out.Reason != string(quiet.ReasonOff) {
		t.Fatalf("out = %+v, want disabled and off", out)
	}
	if got := quiettools.NewStatus(svc).Speak(out); got != "Quiet hours are off." {
		t.Fatalf("Speak = %q", got)
	}
}

// TestStatusHandlerWindowCrossingMidnight covers a timed daily window that
// crosses midnight: quiet inside it, and not quiet before it starts.
func TestStatusHandlerWindowCrossingMidnight(t *testing.T) {
	values := map[string]any{
		statusEnabledKey: true,
		statusStartKey:   "22:30",
		statusEndKey:     "07:00",
		statusVolumeKey:  35,
	}

	t.Run("in window after midnight start", func(t *testing.T) {
		at := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
		svc := newStatusService(t, at, values)
		out, err := quiettools.NewStatus(svc).Handler(context.Background(), quiettools.StatusIn{})
		if err != nil {
			t.Fatalf("Handler: %v", err)
		}
		if !out.Enabled || !out.InWindow || out.Reason != string(quiet.ReasonWindow) {
			t.Fatalf("out = %+v, want enabled, in window, window reason", out)
		}
		if got := quiettools.NewStatus(svc).Speak(out); got != "It's quiet until 7:00 am. Quiet hours run from 10:30 pm to 7:00 am." {
			t.Fatalf("Speak = %q", got)
		}
	})

	t.Run("outside window before start", func(t *testing.T) {
		at := time.Date(2026, 11, 12, 21, 0, 0, 0, time.UTC)
		svc := newStatusService(t, at, values)
		out, err := quiettools.NewStatus(svc).Handler(context.Background(), quiettools.StatusIn{})
		if err != nil {
			t.Fatalf("Handler: %v", err)
		}
		if !out.Enabled || out.InWindow || out.Reason != string(quiet.ReasonOff) {
			t.Fatalf("out = %+v, want enabled, outside window, off reason", out)
		}
		if got := quiettools.NewStatus(svc).Speak(out); got != "Quiet hours run from 10:30 pm to 7:00 am; it's not quiet now." {
			t.Fatalf("Speak = %q", got)
		}
	})
}

// TestStatusHandlerDNDOnly covers a timed do-not-disturb period with the daily
// window off, so the reply is the DND line.
func TestStatusHandlerDNDOnly(t *testing.T) {
	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	svc := newStatusService(t, at, map[string]any{
		statusEnabledKey: false,
		statusStartKey:   "22:30",
		statusEndKey:     "07:00",
		statusVolumeKey:  35,
		statusDNDKey:     "2026-11-12T14:00:00Z",
	})
	out, err := quiettools.NewStatus(svc).Handler(context.Background(), quiettools.StatusIn{})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if out.Reason != string(quiet.ReasonDND) {
		t.Fatalf("out = %+v, want do-not-disturb reason", out)
	}
	if got := quiettools.NewStatus(svc).Speak(out); got != "Do not disturb until 2:00 pm." {
		t.Fatalf("Speak = %q", got)
	}
}
