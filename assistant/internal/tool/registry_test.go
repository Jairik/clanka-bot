package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type echoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
}

type echoOut struct {
	Text string `json:"text"`
}

func echoTool() tool.Tool[echoIn, echoOut] {
	return tool.Tool[echoIn, echoOut]{
		Name:        "test.echo",
		Description: "Echo text back.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in echoIn) (echoOut, error) {
			return echoOut{Text: in.Text}, nil
		},
		Speak: func(out echoOut) string { return out.Text },
	}
}

func TestRegistrationExposesMetadataAndGeneratedSchema(t *testing.T) {
	reg := tool.NewRegistry(2 * time.Second)
	if err := tool.Add(reg, echoTool()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	info, ok := reg.Info("test.echo")
	if !ok {
		t.Fatal("Info did not find registered tool")
	}
	if info.Name != "test.echo" || info.WireName != "test__echo" {
		t.Errorf("names = %q / %q, want test.echo / test__echo", info.Name, info.WireName)
	}
	if info.Description != "Echo text back." {
		t.Errorf("Description = %q", info.Description)
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q", info.Permission)
	}
	if info.LocalOnly {
		t.Error("LocalOnly = true, want false")
	}
	if info.Timeout != 2*time.Second {
		t.Errorf("Timeout = %s, want registry default 2s", info.Timeout)
	}
	if !info.HasSpeak || info.HasExtract {
		t.Errorf("hooks = speak:%v extract:%v, want speak:true extract:false", info.HasSpeak, info.HasExtract)
	}

	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(info.InputSchema, &schema); err != nil {
		t.Fatalf("input schema is not JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q", schema.Type)
	}
	if _, ok := schema.Properties["text"]; !ok {
		t.Error("schema has no text property")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "text" {
		t.Errorf("required = %v, want [text]", schema.Required)
	}

	if info.Timeout != 2*time.Second {
		t.Errorf("Timeout = %s, want registry default 2s", info.Timeout)
	}
}

func TestRegistrationRequiresLocalOnlyFlag(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.LocalOnly = tool.LocalOnlyUnset

	err := tool.Add(reg, tl)
	if err == nil {
		t.Fatal("Add without LocalOnly succeeded, want error")
	}
	if !strings.Contains(err.Error(), "local-only") {
		t.Fatalf("error %q does not explain the local-only requirement", err)
	}
	if _, ok := reg.Info("test.echo"); ok {
		t.Fatal("tool was registered despite invalid LocalOnly")
	}
}

func TestRegistrationRejectsInvalidTools(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*tool.Tool[echoIn, echoOut])
	}{
		{"bad name", func(tl *tool.Tool[echoIn, echoOut]) { tl.Name = "test__echo" }},
		{"missing handler", func(tl *tool.Tool[echoIn, echoOut]) { tl.Handler = nil }},
		{"bad permission", func(tl *tool.Tool[echoIn, echoOut]) { tl.Permission = "admin" }},
		{"negative timeout", func(tl *tool.Tool[echoIn, echoOut]) { tl.Timeout = -time.Second }},
		{"agent only with examples", func(tl *tool.Tool[echoIn, echoOut]) {
			tl.AgentOnly = true
			tl.Examples = []string{"echo hi"}
		}},
		{"agent only with extract", func(tl *tool.Tool[echoIn, echoOut]) {
			tl.AgentOnly = true
			tl.Extract = func(text string) (echoIn, bool) { return echoIn{Text: text}, true }
		}},
		{"agent only and local only", func(tl *tool.Tool[echoIn, echoOut]) {
			tl.AgentOnly = true
			tl.LocalOnly = tool.LocalOnlyYes
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tl := echoTool()
			tt.mut(&tl)
			if err := tool.Add(tool.NewRegistry(time.Second), tl); err == nil {
				t.Fatal("Add succeeded, want error")
			}
		})
	}
}

func TestAgentOnlyIsExposedButNotOnTheFastPath(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.AgentOnly = true
	tl.Examples = nil
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info, ok := reg.Info("test.echo")
	if !ok {
		t.Fatal("Info did not find the Agent-only Tool")
	}
	if !info.AgentOnly || info.HasExtract || len(info.Examples) != 0 {
		t.Fatalf("Agent-only metadata = %+v, want AgentOnly with no Fast path routing hooks", info)
	}
}

func TestRegistrationRejectsDuplicateNames(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, echoTool()); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := tool.Add(reg, echoTool()); err == nil {
		t.Fatal("duplicate Add succeeded, want error")
	}
}

func TestExecuteRoundTripsTypedValues(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, echoTool()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := got.(echoOut)
	if !ok {
		t.Fatalf("Execute returned %T, want echoOut", got)
	}
	if out.Text != "hi" {
		t.Fatalf("Text = %q, want hi", out.Text)
	}

	spoken, ok := reg.Speak("test.echo", out)
	if !ok || spoken != "hi" {
		t.Fatalf("Speak = %q, %v; want hi, true", spoken, ok)
	}
}

func TestExecuteRejectsUnknownToolAndBadArguments(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, echoTool()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := reg.Execute(context.Background(), "test.missing", nil); err == nil {
		t.Fatal("unknown tool executed, want error")
	}
	if _, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":42}`)); err == nil {
		t.Fatal("bad arguments executed, want error")
	}
}

func TestExtractReturnsTypedArguments(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.Examples = []string{"echo hi"}
	tl.Extract = func(text string) (echoIn, bool) {
		if !strings.HasPrefix(text, "echo ") {
			return echoIn{}, false
		}
		return echoIn{Text: strings.TrimPrefix(text, "echo ")}, true
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	args, ok := reg.Extract("test.echo", "echo hi")
	if !ok {
		t.Fatal("Extract returned false")
	}
	if string(args) != `{"text":"hi"}` {
		t.Fatalf("args = %s", args)
	}
	if _, ok := reg.Extract("test.echo", "nope"); ok {
		t.Fatal("Extract matched unmatched text")
	}
}

func TestExecuteValidatesTheDeclaredSchemaBeforeTheHandler(t *testing.T) {
	runs := 0
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.Handler = func(context.Context, echoIn) (echoOut, error) {
		runs++
		return echoOut{}, nil
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, args := range []string{
		`{"text":`,            // malformed JSON
		`{}`,                  // required field missing
		`null`,                // not an object
		`{"text":42}`,         // wrong type
		`{"text":"hi","x":1}`, // unknown field
	} {
		if _, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(args)); err == nil {
			t.Fatalf("Execute(%s) succeeded, want a schema error", args)
		}
	}
	if runs != 0 {
		t.Fatalf("handler ran %d times for rejected arguments, want 0", runs)
	}
	if _, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("valid arguments failed: %v", err)
	}
	if runs != 1 {
		t.Fatalf("handler ran %d times, want 1", runs)
	}
}

func TestNoArgumentToolsKeepTheirEmptyDefault(t *testing.T) {
	runs := 0
	reg := tool.NewRegistry(time.Second)
	def := tool.Tool[struct {
		ID string `json:"id,omitempty"`
	}, struct{}]{
		Name: "test.optional", Description: "Optional arguments.", Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct {
			ID string `json:"id,omitempty"`
		}) (struct{}, error) {
			runs++
			return struct{}{}, nil
		},
	}
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// An omitted object and an explicit empty object are the legitimate
	// "no arguments" forms, not malformed JSON.
	if _, err := reg.Execute(context.Background(), "test.optional", nil); err != nil {
		t.Fatalf("nil arguments: %v", err)
	}
	if _, err := reg.Execute(context.Background(), "test.optional", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("empty object: %v", err)
	}
	if runs != 2 {
		t.Fatalf("handler ran %d times, want 2", runs)
	}
}
