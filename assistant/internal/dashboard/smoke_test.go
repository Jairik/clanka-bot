package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type fakeSmokeResponder struct {
	reply      pipeline.Reply
	err        error
	routing    time.Duration
	toolExec   time.Duration
	transcripts []string
}

func (f *fakeSmokeResponder) Ask(ctx context.Context, req tool.Request, transcript string) (pipeline.Reply, error) {
	f.transcripts = append(f.transcripts, transcript)
	if tl := latency.FromContext(ctx); tl != nil {
		if f.routing > 0 {
			tl.Set(latency.StageRouting, f.routing)
		}
		if f.toolExec > 0 {
			tl.Add(latency.StageToolExecute, f.toolExec)
		}
	}
	return f.reply, f.err
}

// TestSmokeEndpointContract asserts the full JSON contract of POST /api/ui/smoke.
func TestSmokeEndpointContract(t *testing.T) {
	responder := &fakeSmokeResponder{
		reply: pipeline.Reply{
			Path: tool.PathFast,
			Tool: "time.now",
			Text: "It's 3:04 PM.",
		},
		routing:  15 * time.Millisecond,
		toolExec: 2 * time.Millisecond,
	}

	h := newHandler(t, dashboard.Options{
		Typed: responder,
	})

	req := request(http.MethodPost, "/api/ui/smoke", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/ui/smoke status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Route  string `json:"route"`
		Tool   string `json:"tool"`
		Reply  string `json:"reply"`
		Stages struct {
			RoutingMS int64 `json:"routing_ms"`
			ToolMS    int64 `json:"tool_ms"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode smoke response: %v\nbody: %s", err, rec.Body.String())
	}

	if body.Route != "fast_path" {
		t.Errorf("route = %q, want %q", body.Route, "fast_path")
	}
	if body.Tool != "time.now" {
		t.Errorf("tool = %q, want %q", body.Tool, "time.now")
	}
	if body.Reply != "It's 3:04 PM." {
		t.Errorf("reply = %q, want %q", body.Reply, "It's 3:04 PM.")
	}
	if body.Stages.RoutingMS != 15 {
		t.Errorf("stages.routing_ms = %d, want 15", body.Stages.RoutingMS)
	}
	if body.Stages.ToolMS != 2 {
		t.Errorf("stages.tool_ms = %d, want 2", body.Stages.ToolMS)
	}

	if len(responder.transcripts) != 1 || responder.transcripts[0] != "what time is it" {
		t.Errorf("responder transcripts = %+v, want ['what time is it']", responder.transcripts)
	}
}

// TestSmokeEndpointRefusesUnauthenticated proves an unauthenticated caller is rejected.
func TestSmokeEndpointRefusesUnauthenticated(t *testing.T) {
	h := newHandler(t, dashboard.Options{
		Typed: &fakeSmokeResponder{},
	})

	// No owner header.
	req := request(http.MethodPost, "/api/ui/smoke", "")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(h, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated smoke status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// Wrong owner.
	reqWrong := request(http.MethodPost, "/api/ui/smoke", "impostor@example.com")
	reqWrong.Header.Set("Sec-Fetch-Site", "same-origin")
	recWrong := serve(h, reqWrong)

	if recWrong.Code != http.StatusForbidden {
		t.Errorf("wrong owner smoke status = %d, want %d", recWrong.Code, http.StatusForbidden)
	}
}

// TestSmokeEndpointRefusesCrossSite proves CSRF protection guards the smoke endpoint.
func TestSmokeEndpointRefusesCrossSite(t *testing.T) {
	h := newHandler(t, dashboard.Options{
		Typed: &fakeSmokeResponder{},
	})

	req := request(http.MethodPost, "/api/ui/smoke", owner)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := serve(h, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site smoke status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestSmokeEndpointWrongRouteReturnsAgentPath proves that when the pipeline falls
// through (e.g. embed service stopped), the response exposes route = "agent_path".
func TestSmokeEndpointWrongRouteReturnsAgentPath(t *testing.T) {
	responder := &fakeSmokeResponder{
		reply: pipeline.Reply{
			Path: tool.PathAgent,
			Text: "Agent call skipped for smoke.",
		},
		routing: 50 * time.Millisecond,
	}

	h := newHandler(t, dashboard.Options{
		Typed: responder,
	})

	req := request(http.MethodPost, "/api/ui/smoke", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("smoke status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Route string `json:"route"`
		Tool  string `json:"tool"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Route != "agent_path" {
		t.Errorf("route = %q, want agent_path", body.Route)
	}
}

// TestSmokeEndpointDoesNotRecordActivity proves the smoke turn does not add an Activity row.
func TestSmokeEndpointDoesNotRecordActivity(t *testing.T) {
	actStore := activity.New(50)
	responder := &fakeSmokeResponder{
		reply: pipeline.Reply{
			Path: tool.PathFast,
			Tool: "time.now",
			Text: "It's 3:04 PM.",
		},
	}

	h := newHandler(t, dashboard.Options{
		Activity: actStore,
		Typed:    responder,
	})

	req := request(http.MethodPost, "/api/ui/smoke", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("smoke status = %d, want %d", rec.Code, http.StatusOK)
	}

	if actStore.Len() != 0 {
		t.Fatalf("smoke turn added %d entries to activity store, want 0", actStore.Len())
	}
}
