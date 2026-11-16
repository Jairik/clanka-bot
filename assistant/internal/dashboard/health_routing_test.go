package dashboard_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
)

type routingView struct {
	Services []struct{ Name, Status, StatusLabel, Detail string }
	Routing  struct{ Name, Status, StatusLabel, Detail string }
}

// TestHealthViewRoutingState maps the in-memory semantic build state to one
// Health row next to the service rows: no router, no index and a failed rebuild
// are never rendered as a fake ok; only a built index is.
func TestHealthViewRoutingState(t *testing.T) {
	lastAttempt := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	rebuildErr := errors.New("embed service is down")
	cases := []struct {
		name   string
		state  func() (int, error, time.Time)
		status string
		label  string
		detail string // substring
	}{
		{
			name:   "nil router is not configured",
			status: "not-configured",
			label:  "not configured",
			detail: "not configured in this deployment",
		},
		{
			name:   "empty index is not built yet",
			state:  func() (int, error, time.Time) { return 0, nil, time.Time{} },
			status: "error",
			label:  "not built yet",
			detail: "No Tools are indexed",
		},
		{
			name:   "failed rebuild shows the error",
			state:  func() (int, error, time.Time) { return 0, rebuildErr, lastAttempt },
			status: "error",
			label:  "rebuild failed",
			detail: "embed service is down",
		},
		{
			name:   "failed rebuild with an old index is still an error",
			state:  func() (int, error, time.Time) { return 3, rebuildErr, lastAttempt },
			status: "error",
			label:  "rebuild failed",
			detail: "embed service is down",
		},
		{
			name:   "built index shows the count",
			state:  func() (int, error, time.Time) { return 3, nil, lastAttempt },
			status: "ok",
			label:  "ok",
			detail: "3 Tools indexed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A healthy shared snapshot: a degraded Fast path must not fail it.
			src := status.New(status.Options{
				Services: []dashboard.ServiceCheck{
					{Name: "embed", Check: func(context.Context) error { return nil }},
				},
			})
			handler := newHandler(t, dashboard.Options{Status: src, ExamplesState: tc.state})
			var view routingView
			rec := getJSON(t, handler, "/api/ui/health", &view)
			if rec.Code != 200 {
				t.Fatalf("health status = %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "Semantic routing") {
				t.Fatalf("health body lacks the Semantic routing row: %s", rec.Body.String())
			}
			row := view.Routing
			if row.Name != "Semantic routing" || row.Status != tc.status || row.StatusLabel != tc.label {
				t.Errorf("routing row = %+v, want status %q label %q", row, tc.status, tc.label)
			}
			if !strings.Contains(row.Detail, tc.detail) {
				t.Errorf("routing detail = %q, want %q", row.Detail, tc.detail)
			}
			if !src.Status(context.Background()).OK() {
				t.Error("a degraded semantic index changed StatusReport.OK()")
			}
			if len(view.Services) != 1 || view.Services[0].Name != "embed" || view.Services[0].Status != "ok" {
				t.Errorf("service rows changed with routing state: %+v", view.Services)
			}
		})
	}
}
