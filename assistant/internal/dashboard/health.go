package dashboard

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// healthView is the Health page's whole data model. The panel is rendered from
// the same view for both a full page load and an htmx refresh. The check
// results come from internal/status, which the system.status Tool also uses,
// so the page and a spoken status never disagree about a service.
type healthView struct {
	Owner         string
	Page          string
	CheckedAt     string
	HealthTimeout string
	Services      []checkView
	Location      checkView
	Modules       []checkView
}

type checkView struct {
	Name        string
	Status      string
	StatusLabel string
	Detail      string
}

// health runs the shared checks inside one bounded snapshot and maps them to
// the template's rows.
func (h *Handler) health(ctx context.Context) healthView {
	report := h.healthStatus.Status(ctx)
	return healthView{
		Owner:         h.owner,
		Page:          "health",
		CheckedAt:     report.CheckedAt.Format("2006-01-02 15:04:05 UTC"),
		HealthTimeout: report.Timeout.String(),
		Services:      checkViews(report.Services),
		Location:      checkViewOf(report.Location),
		Modules:       checkViews(report.Modules),
	}
}

func checkViews(checks []tool.StatusCheck) []checkView {
	views := make([]checkView, 0, len(checks))
	for _, check := range checks {
		views = append(views, checkViewOf(check))
	}
	return views
}

func checkViewOf(check tool.StatusCheck) checkView {
	return checkView{
		Name:        check.Name,
		Status:      string(check.State),
		StatusLabel: status.Label(check.State),
		Detail:      check.Detail,
	}
}
