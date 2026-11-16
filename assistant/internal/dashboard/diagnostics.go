package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
)

const (
	// maxDiagnosticsBytes caps the whole bundle. An over-cap bundle is refused
	// outright rather than truncated into invalid JSON.
	maxDiagnosticsBytes = 1 << 20
	// diagnosticsRequestLimit is the number of recent Activity entries the
	// optional ?requests=1 adds. It matches the Activity page's one-page limit.
	diagnosticsRequestLimit = 50
	// diagnosticsActivityNotice is the opt-in warning shown on the Health page
	// and repeated in the bundle so a forwarded copy keeps the caveat.
	diagnosticsActivityNotice = "This bundle includes recent transcripts and replies, which can contain private text. Review it before sharing."
	diagnosticsRedacted       = "[redacted]"
	diagnosticsOmitted        = "[local-only result omitted]"
)

// errDiagnosticsTooLarge is the one build failure the route translates to a
// plain refusal. It never carries bundle contents.
var errDiagnosticsTooLarge = errors.New("dashboard: diagnostics bundle exceeds 1 MiB")

// diagnosticsInput is the narrow, server-free input to the bundle builder.
// The route is the only place that fills it from a Handler; keeping it small
// makes the privacy and size rules testable without an HTTP server.
type diagnosticsInput struct {
	GeneratedAt time.Time
	StartedAt   time.Time
	APIVersion  int
	UIPin       string
	UIVersion   string
	Health      healthView
	Status      tool.StatusReport
	Settings    []diagnosticSetting
	Tools       []diagnosticTool
	Secrets     []diagnosticSecret
	Voice       diagnosticVoice
	Wakeword    diagnosticWakeword
	Activity    []diagnosticActivity
}

// diagnosticsBundle is the on-disk JSON shape. Every field is deliberately a
// plain, bounded value: no live objects, no raw secret metadata and no note
// text.
type diagnosticsBundle struct {
	GeneratedAt    string               `json:"generated_at"`
	UptimeSeconds  int64                `json:"uptime_seconds"`
	APIVersion     int                  `json:"api_version"`
	UI             diagnosticUI         `json:"ui"`
	Health         healthView           `json:"health"`
	Status         diagnosticStatus     `json:"status"`
	Settings       []diagnosticSetting  `json:"settings"`
	Tools          []diagnosticTool     `json:"tools"`
	Secrets        []diagnosticSecret   `json:"secrets"`
	Voice          diagnosticVoice      `json:"voice"`
	Wakeword       diagnosticWakeword   `json:"wakeword"`
	Activity       []diagnosticActivity `json:"activity,omitempty"`
	ActivityNotice string               `json:"activity_notice,omitempty"`
}

type diagnosticUI struct {
	Pin             string `json:"pin"`
	ReportedVersion string `json:"reported_version"`
}

type diagnosticStatus struct {
	CheckedAt string             `json:"checked_at"`
	Timeout   string             `json:"timeout"`
	Services  []tool.StatusCheck `json:"services"`
	Location  tool.StatusCheck   `json:"location"`
	Modules   []tool.StatusCheck `json:"modules"`
}

// diagnosticSetting carries the effective value only when its key is not
// sensitive. Secrets live in the secrets service and never reach this list.
type diagnosticSetting struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
}

// diagnosticTool is one Tool's effective policy. Names, layers, kinds and
// enabled flags are non-secret by design; this is the module list support asks
// for, with Permission, Local-only and enabled state.
type diagnosticTool struct {
	Name             string `json:"name"`
	Enabled          bool   `json:"enabled"`
	Permission       string `json:"permission"`
	Declared         string `json:"declared_permission"`
	LocalOnly        bool   `json:"local_only"`
	EnabledSource    string `json:"enabled_source"`
	PermissionSource string `json:"permission_source"`
}

// diagnosticSecret is metadata only. There is deliberately no value, last4,
// topic or updated timestamp: those are either secret material or a hint that
// narrows it.
type diagnosticSecret struct {
	Name   string `json:"name"`
	Set    bool   `json:"set"`
	Source string `json:"source"`
}

// diagnosticVoice is a count-and-name summary. Voice source notes are private
// owner text and are not included.
type diagnosticVoice struct {
	Active   string `json:"active"`
	Default  string `json:"default"`
	Total    int    `json:"total"`
	Custom   int    `json:"custom"`
	Stale    int    `json:"stale"`
	Revision string `json:"revision"`
}

type diagnosticWakeword struct {
	Enabled bool                      `json:"enabled"`
	Addr    string                    `json:"addr,omitempty"`
	Models  []diagnosticWakewordModel `json:"models"`
}

type diagnosticWakewordModel struct {
	Name      string  `json:"name"`
	Threshold float64 `json:"threshold"`
	Command   bool    `json:"command"`
	Enabled   bool    `json:"enabled"`
}

// diagnosticActivity is one opt-in recent request. Local-only flag and text
// are separated so a local-only result can never carry its output into the
// bundle.
type diagnosticActivity struct {
	ID          string           `json:"id"`
	Time        string           `json:"time"`
	Source      string           `json:"source"`
	Path        string           `json:"path"`
	Tool        string           `json:"tool"`
	Outcome     string           `json:"outcome"`
	Error       string           `json:"error,omitempty"`
	Transcript  string           `json:"transcript,omitempty"`
	Reply       string           `json:"reply,omitempty"`
	ResponseMS  *int64           `json:"response_ms,omitempty"`
	Stages      []diagnosticMS   `json:"stages,omitempty"`
	Checkpoints []diagnosticMS   `json:"checkpoints,omitempty"`
	Calls       []diagnosticCall `json:"calls,omitempty"`
	LocalOnly   bool             `json:"local_only"`
	Summary     bool             `json:"summary,omitempty"`
}

type diagnosticMS struct {
	Name string `json:"name"`
	MS   int64  `json:"ms"`
}

type diagnosticCall struct {
	Name      string `json:"name"`
	LocalOnly bool   `json:"local_only"`
	Result    string `json:"result,omitempty"`
}

// buildDiagnosticsBundle marshals one bounded JSON document. It is pure: it
// takes no services and logs nothing, so the privacy and size rules can be
// tested directly.
func buildDiagnosticsBundle(in diagnosticsInput) ([]byte, error) {
	uptime := int64(0)
	if in.StartedAt.Before(in.GeneratedAt) {
		uptime = int64(in.GeneratedAt.Sub(in.StartedAt) / time.Second)
	}
	notice := ""
	if len(in.Activity) > 0 {
		notice = diagnosticsActivityNotice
	}
	data, err := json.Marshal(diagnosticsBundle{
		GeneratedAt:   in.GeneratedAt.UTC().Format(time.RFC3339),
		UptimeSeconds: uptime,
		APIVersion:    in.APIVersion,
		UI: diagnosticUI{
			Pin:             in.UIPin,
			ReportedVersion: in.UIVersion,
		},
		Health: in.Health,
		Status: diagnosticStatus{
			CheckedAt: in.Status.CheckedAt.UTC().Format(time.RFC3339),
			Timeout:   in.Status.Timeout.String(),
			Services:  in.Status.Services,
			Location:  in.Status.Location,
			Modules:   in.Status.Modules,
		},
		Settings:       in.Settings,
		Tools:          in.Tools,
		Secrets:        in.Secrets,
		Voice:          in.Voice,
		Wakeword:       in.Wakeword,
		Activity:       in.Activity,
		ActivityNotice: notice,
	})
	if err != nil {
		return nil, err
	}
	if len(data) > maxDiagnosticsBytes {
		return nil, errDiagnosticsTooLarge
	}
	return data, nil
}

// diagnosticsTag is the safe timestamp used in the download filename. It is
// built from digits and letters only, so it cannot carry a path or a header
// injection.
func diagnosticsTag(now time.Time) string {
	return now.UTC().Format("20060102T150405Z")
}

// diagnostics serves one read-only support bundle. Identity, Host and
// same-origin rules are enforced by ServeHTTP before this handler runs, just
// as for every other Dashboard read.
func (h *Handler) diagnostics(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	now := h.now()
	includeRequests := r.URL.Query().Get("requests") == "1"
	data, err := buildDiagnosticsBundle(h.diagnosticsInput(r.Context(), now, includeRequests))
	if err != nil {
		// Log the failure class only, never the bundle's contents.
		h.logger.Error("diagnostics bundle refused", "error", err.Error())
		http.Error(w, "Diagnostics bundle is unavailable.", http.StatusInternalServerError)
		return
	}
	name := "clanka-diagnostics-" + diagnosticsTag(now) + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// diagnosticsInput assembles one bundle input from live services. It is the
// only place that reads secret, setting, Tool and Activity metadata; the pure
// builder never sees a service or a secret value.
func (h *Handler) diagnosticsInput(ctx context.Context, now time.Time, includeRequests bool) diagnosticsInput {
	in := diagnosticsInput{
		GeneratedAt: now,
		StartedAt:   h.startedAt,
		APIVersion:  APIVersion,
		Health:      h.health(ctx),
		Status:      h.healthStatus.Status(ctx),
	}
	in.UIPin = h.uiPin
	in.UIVersion = h.uiPin
	if artifact := h.uiArtifact; artifact != nil {
		in.UIPin = artifact.pin
		in.UIVersion = artifact.reportedVersion()
	}
	if h.settings != nil {
		for _, value := range h.settings.List() {
			in.Settings = append(in.Settings, diagnosticSetting{
				Key:    value.Key,
				Value:  redactDiagnosticSetting(value.Key, value.Value),
				Source: string(value.Source),
				Kind:   string(value.Kind),
			})
		}
	}
	if h.tools != nil {
		for _, state := range h.tools.List() {
			in.Tools = append(in.Tools, diagnosticToolOf(state))
		}
	}
	if h.secrets != nil {
		for _, meta := range h.secrets.List() {
			in.Secrets = append(in.Secrets, diagnosticSecret{
				Name:   meta.Name,
				Set:    meta.Set,
				Source: string(meta.Source),
			})
		}
	}
	in.Voice, in.Wakeword = h.diagnosticVoiceAndWakeword()
	if includeRequests && h.activity != nil {
		for _, entry := range h.activity.Recent(diagnosticsRequestLimit) {
			in.Activity = append(in.Activity, diagnosticActivityOf(entry))
		}
	}
	return in
}

func diagnosticToolOf(state toolpolicy.State) diagnosticTool {
	return diagnosticTool{
		Name:             state.Name,
		Enabled:          state.Enabled,
		Permission:       string(state.Permission),
		Declared:         string(state.Declared),
		LocalOnly:        state.LocalOnly,
		EnabledSource:    string(state.EnabledSource),
		PermissionSource: string(state.PermissionSource),
	}
}

// redactDiagnosticSetting never lets a value through for a key that names
// secret material. Effective settings are otherwise non-secret: the ntfy topic
// and provider tokens live in the secrets service and are emitted as metadata
// only.
func redactDiagnosticSetting(key string, value any) any {
	lower := strings.ToLower(key)
	for _, word := range []string{"token", "password", "secret", "credential", "topic", "api_key", "apikey"} {
		if strings.Contains(lower, word) {
			return diagnosticsRedacted
		}
	}
	return value
}

func diagnosticActivityOf(entry activity.Entry) diagnosticActivity {
	out := diagnosticActivity{
		ID:         entry.ID,
		Time:       entry.At.UTC().Format(time.RFC3339),
		Source:     entry.Source,
		Path:       entry.Path,
		Tool:       entry.Tool,
		Outcome:    entry.Outcome,
		Error:      entry.Error,
		ResponseMS: entry.ResponseMS,
		LocalOnly:  entry.LocalOnly,
		Summary:    entry.Summary,
	}
	// A local-only turn's text is deliberately confined to the authenticated
	// Activity page; only its metadata crosses into a shareable file.
	if !entry.LocalOnly {
		out.Transcript = entry.Transcript
		out.Reply = entry.Reply
	}
	for _, stage := range entry.Stages {
		out.Stages = append(out.Stages, diagnosticMS{Name: stage.Name, MS: stage.MS})
	}
	for _, checkpoint := range entry.Checkpoints {
		out.Checkpoints = append(out.Checkpoints, diagnosticMS{Name: checkpoint.Name, MS: checkpoint.MS})
	}
	for _, call := range entry.Calls {
		row := diagnosticCall{Name: call.Name, LocalOnly: call.LocalOnly}
		if !call.LocalOnly {
			row.Result = call.Result
		} else {
			row.Result = diagnosticsOmitted
		}
		out.Calls = append(out.Calls, row)
	}
	return out
}

// diagnosticVoiceAndWakeword reports names and counts only. It never reads a
// voice's source note or a secret.
func (h *Handler) diagnosticVoiceAndWakeword() (diagnosticVoice, diagnosticWakeword) {
	var voice diagnosticVoice
	if h.voices != nil {
		voice.Default = h.voices.DefaultName()
		voice.Revision = h.voices.Revision()
		if active, ok := h.voices.Active(); ok {
			voice.Active = active.Name
		}
		for _, v := range h.voices.List() {
			voice.Total++
			if !v.Builtin {
				voice.Custom++
			}
			if v.Stale {
				voice.Stale++
			}
		}
	}
	wake := diagnosticWakeword{Enabled: strings.TrimSpace(h.wakeword.Addr) != "", Addr: h.wakeword.Addr}
	for _, model := range h.wakeword.Models {
		wake.Models = append(wake.Models, diagnosticWakewordModel{
			Name:      model.Name,
			Threshold: model.Threshold,
			Command:   model.Command,
			Enabled:   model.Enabled,
		})
	}
	return voice, wake
}
