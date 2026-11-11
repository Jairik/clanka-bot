// Package tool holds the in-process Tool registry contract shared by the Fast
// path and the Agent path.
package tool

import (
	"fmt"
	"regexp"
	"strings"
)

// Names are canonical dotted snake_case: "<domain>.<operation>".
var namePart = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// WireName returns the OpenAI-compatible function name for a canonical Tool name.
func WireName(name string) (string, error) {
	if err := validateCanonical(name); err != nil {
		return "", err
	}
	return strings.Replace(name, ".", "__", 1), nil
}

// CanonicalName returns the canonical Tool name for a wire name.
func CanonicalName(wire string) (string, error) {
	if strings.Contains(wire, ".") {
		return "", fmt.Errorf("tool: wire name %q must not contain %q", wire, ".")
	}
	parts := strings.Split(wire, "__")
	if len(parts) != 2 {
		return "", fmt.Errorf("tool: wire name %q must contain exactly one %q", wire, "__")
	}
	name := parts[0] + "." + parts[1]
	if err := validateCanonical(name); err != nil {
		return "", fmt.Errorf("tool: invalid wire name %q: %w", wire, err)
	}
	return name, nil
}

func validateCanonical(name string) error {
	if strings.Contains(name, "__") {
		return fmt.Errorf("tool: name %q must not contain %q", name, "__")
	}
	parts := strings.Split(name, ".")
	if len(parts) != 2 {
		return fmt.Errorf("tool: name %q must be <domain>.<operation>", name)
	}
	for _, part := range parts {
		if !namePart.MatchString(part) {
			return fmt.Errorf("tool: name %q is not dotted snake_case", name)
		}
	}
	return nil
}
