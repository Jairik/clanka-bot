package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Permission says how much a Tool may change the world.
type Permission string

const (
	PermissionRead    Permission = "read"
	PermissionAct     Permission = "act"
	PermissionConfirm Permission = "confirm"
)

// LocalOnly is the required privacy declaration for a Tool. The zero value is
// invalid so registration fails until the author makes an explicit choice.
type LocalOnly uint8

const (
	LocalOnlyUnset LocalOnly = iota
	LocalOnlyYes
	LocalOnlyNo
)

// Tool is one typed operation. In and Out are the source of truth for the JSON
// schema offered to the model.
type Tool[In, Out any] struct {
	Name        string
	Description string
	Permission  Permission
	LocalOnly   LocalOnly
	Timeout     time.Duration // zero uses the registry default
	Handler     func(ctx context.Context, in In) (Out, error)

	Speak    func(Out) string             // optional Fast path renderer
	Examples []string                     // optional Fast path sample phrasings
	Extract  func(text string) (In, bool) // optional Fast path argument filler

	// Summary, when set, renders the one-line question a confirm Tool asks
	// before it runs. It falls back to the Description.
	Summary func(In) string
}

// Info is the type-erased view of a registered Tool.
type Info struct {
	Name         string
	WireName     string
	Description  string
	Permission   Permission
	LocalOnly    bool
	Timeout      time.Duration
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Examples     []string
	HasSpeak     bool
	HasExtract   bool
}

type entry struct {
	info    Info
	schema  *jsonschema.Resolved
	run     func(ctx context.Context, args json.RawMessage) (any, error)
	speak   func(result any) (string, bool)
	extract func(text string) (json.RawMessage, bool)
	summary func(args json.RawMessage) string
}

// Registry holds Tools registered at compile time and executes them, plus the
// single pending Confirmation.
type Registry struct {
	defaultTimeout time.Duration

	mu    sync.RWMutex
	tools map[string]*entry

	// pending is the one confirm call waiting for a spoken yes. Confirmation
	// enforcement lives here so no caller, including the model, can run a
	// confirm Tool around it.
	pendingMu sync.Mutex
	pending   *PendingConfirmation
}

// NewRegistry creates an empty registry. defaultTimeout applies to Tools that
// declare none.
func NewRegistry(defaultTimeout time.Duration) *Registry {
	return &Registry{
		defaultTimeout: defaultTimeout,
		tools:          make(map[string]*entry),
	}
}

// Add registers one typed Tool. It is a free function because Go methods
// cannot introduce type parameters.
func Add[In, Out any](r *Registry, t Tool[In, Out]) error {
	if err := validateCanonical(t.Name); err != nil {
		return err
	}
	wire, _ := WireName(t.Name)
	switch t.Permission {
	case PermissionRead, PermissionAct, PermissionConfirm:
	default:
		return fmt.Errorf("tool %q: unknown permission %q", t.Name, t.Permission)
	}
	switch t.LocalOnly {
	case LocalOnlyYes, LocalOnlyNo:
	default:
		return fmt.Errorf("tool %q: local-only flag is required (LocalOnlyYes or LocalOnlyNo)", t.Name)
	}
	if t.Handler == nil {
		return fmt.Errorf("tool %q: handler is required", t.Name)
	}
	if t.Timeout < 0 {
		return fmt.Errorf("tool %q: timeout must not be negative", t.Name)
	}

	inSchema, err := jsonschema.For[In](nil)
	if err != nil {
		return fmt.Errorf("tool %q: input schema: %w", t.Name, err)
	}
	outSchema, err := jsonschema.For[Out](nil)
	if err != nil {
		return fmt.Errorf("tool %q: output schema: %w", t.Name, err)
	}
	inJSON, err := json.Marshal(inSchema)
	if err != nil {
		return fmt.Errorf("tool %q: marshal input schema: %w", t.Name, err)
	}
	outJSON, err := json.Marshal(outSchema)
	if err != nil {
		return fmt.Errorf("tool %q: marshal output schema: %w", t.Name, err)
	}
	// Resolve once at registration, then validate every call at the public
	// entry points. A malformed or schema-invalid model argument must fail
	// before the handler, not run an act Tool with zero values.
	resolved, err := inSchema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("tool %q: resolve input schema: %w", t.Name, err)
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = r.defaultTimeout
	}
	e := &entry{
		info: Info{
			Name:         t.Name,
			WireName:     wire,
			Description:  t.Description,
			Permission:   t.Permission,
			LocalOnly:    t.LocalOnly == LocalOnlyYes,
			Timeout:      timeout,
			InputSchema:  inJSON,
			OutputSchema: outJSON,
			Examples:     append([]string(nil), t.Examples...),
			HasSpeak:     t.Speak != nil,
			HasExtract:   t.Extract != nil,
		},
		schema: resolved,
		run: func(ctx context.Context, args json.RawMessage) (any, error) {
			if err := validateArgs(resolved, args); err != nil {
				return nil, fmt.Errorf("tool %q: %w", t.Name, err)
			}
			var in In
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, fmt.Errorf("tool %q: invalid arguments: %w", t.Name, err)
				}
			}
			return t.Handler(ctx, in)
		},
		speak: func(result any) (string, bool) {
			if t.Speak == nil {
				return "", false
			}
			out, ok := result.(Out)
			if !ok {
				return "", false
			}
			return t.Speak(out), true
		},
		extract: func(text string) (json.RawMessage, bool) {
			if t.Extract == nil {
				return nil, false
			}
			in, ok := t.Extract(text)
			if !ok {
				return nil, false
			}
			args, err := json.Marshal(in)
			if err != nil {
				return nil, false
			}
			return args, true
		},
		summary: func(args json.RawMessage) string {
			if t.Summary != nil {
				var in In
				if len(args) == 0 || json.Unmarshal(args, &in) == nil {
					if text := strings.TrimSpace(t.Summary(in)); text != "" {
						return text
					}
				}
			}
			return defaultSummary(t.Name, t.Description)
		},
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("tool %q: already registered", t.Name)
	}
	r.tools[t.Name] = e
	return nil
}

// Execute runs a Tool by canonical name with JSON arguments. Registry
// timeouts and user-facing/internal error rules are applied here. A confirm
// Tool never runs: Execute returns a ConfirmationRequiredError and leaves no
// pending call, so a bypass attempt cannot be confirmed later.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (any, error) {
	e, ok := r.lookup(name)
	if !ok {
		return nil, fmt.Errorf("tool: unknown tool %q", name)
	}
	if err := validateArgs(e.schema, args); err != nil {
		return nil, fmt.Errorf("tool %q: %w", name, err)
	}
	if e.info.Permission == PermissionConfirm {
		return nil, &ConfirmationRequiredError{Pending: PendingConfirmation{
			Tool:    e.info.Name,
			Args:    append(json.RawMessage(nil), args...),
			Summary: e.summary(args),
		}}
	}
	return r.run(ctx, e, args)
}

// lookup returns a registered entry.
func (r *Registry) lookup(name string) (*entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	return e, ok
}

// run applies the Tool timeout and runs its handler.
func (r *Registry) run(ctx context.Context, e *entry, args json.RawMessage) (any, error) {
	if e.info.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.info.Timeout)
		defer cancel()
	}
	result, err := e.run(ctx, args)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tool %q: %w", e.info.Name, err)
	}
	return result, nil
}

// Speak renders a Tool result for the Fast path.
func (r *Registry) Speak(name string, result any) (string, bool) {
	r.mu.RLock()
	e, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "", false
	}
	return e.speak(result)
}

// Extract fills a Tool's arguments from an utterance for the Fast path.
func (r *Registry) Extract(name, text string) (json.RawMessage, bool) {
	r.mu.RLock()
	e, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return e.extract(text)
}

// Info returns a registered Tool's metadata.
func (r *Registry) Info(name string) (Info, bool) {
	r.mu.RLock()
	e, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return Info{}, false
	}
	info := e.info
	info.Examples = append([]string(nil), e.info.Examples...)
	return info, true
}

// Names returns registered canonical names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateArgs enforces the Tool's generated input schema before its handler
// runs. Empty arguments are the legitimate "no arguments" form (an omitted
// object), not invalid JSON; a required field still fails. Unknown fields are
// rejected by the generated schema's additionalProperties: false.
func validateArgs(schema *jsonschema.Resolved, args json.RawMessage) error {
	var value any = map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 {
		if err := json.Unmarshal(args, &value); err != nil {
			return fmt.Errorf("invalid JSON arguments: %w", err)
		}
	}
	if schema == nil {
		return errors.New("invalid arguments: input schema is not resolved")
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// FailureMessage returns the only text an error may show the user.
func FailureMessage(err error) string {
	var toolErr *ToolError
	if errors.As(err, &toolErr) && toolErr.Say != "" {
		return toolErr.Say
	}
	return DefaultFailureMessage
}
