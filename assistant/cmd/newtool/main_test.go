package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestNewToolCreatesBuildableConformingModule is the acceptance test for the
// public seam: build the command, point it at a real assistant tree, run it
// once, then build and run the generated module's real conformance test. When
// ROUTING_EVAL_EMBED_URL is set, the nested test also checks Example routing
// against the real MiniLM service.
func TestNewToolCreatesBuildableConformingModule(t *testing.T) {
	binary := buildNewTool(t)
	assistant := copyAssistant(t)
	const name = "scaffold_check.ping_now"

	if output, err := runNewTool(t, binary, assistant, name); err != nil {
		t.Fatalf("new-tool %s failed: %v\n%s", name, err, output)
	}

	moduleDir := filepath.Join(assistant, "tools", "scaffold_check")
	for _, file := range []string{"scaffold_check.go", "samples.go", "conformance_test.go"} {
		content := readFile(t, filepath.Join(moduleDir, file))
		if !strings.Contains(content, "docs/agents/adding-a-tool.md") {
			t.Errorf("%s does not point at docs/agents/adding-a-tool.md", file)
		}
	}
	moduleSource := readFile(t, filepath.Join(moduleDir, "scaffold_check.go"))
	for _, want := range []string{
		`const Name = "scaffold_check.ping_now"`,
		"func DefaultConfig() Config",
		"func (m *Module) Configure(section *yaml.Node) error",
		"Permission:  tool.PermissionRead",
		"LocalOnly:   tool.LocalOnlyNo",
	} {
		if !strings.Contains(moduleSource, want) {
			t.Errorf("generated module is missing %q", want)
		}
	}
	samplesSource := readFile(t, filepath.Join(moduleDir, "samples.go"))
	if !strings.Contains(samplesSource, "ConformanceSamples() map[string]any") {
		t.Error("generated samples.go has no ConformanceSamples")
	}
	testSource := readFile(t, filepath.Join(moduleDir, "conformance_test.go"))
	if !strings.Contains(testSource, "conformance.Run(t, scaffold_check.NewModule())") {
		t.Error("generated conformance test does not call conformance.Run")
	}

	toolsSource := readFile(t, filepath.Join(assistant, "tools", "tools.go"))
	for _, want := range []string{
		`"github.com/Jairik/clanka-bot/assistant/tools/scaffold_check"`,
		"scaffold_check.NewModule(),",
	} {
		if !strings.Contains(toolsSource, want) {
			t.Errorf("tools/tools.go is missing %q", want)
		}
	}
	assertNoStagingLeftovers(t, filepath.Join(assistant, "tools"))

	// The generated module must compile and pass the real shared contract.
	// Read the embed URL here so Go's test cache keys on it: the nested test
	// inherits it, but the cache cannot see that from the subprocess.
	_ = os.Getenv("ROUTING_EVAL_EMBED_URL")
	cmd := exec.Command("go", "test", "./tools/scaffold_check", "-run", "TestModuleConformance", "-count=1", "-v")
	cmd.Dir = assistant
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated module does not build or pass conformance: %v\n%s", err, output)
	}
}

// TestNewToolRejectsInvalidAndExistingNames makes every rejection happen
// before a write: the copied tree must be byte-identical afterwards.
func TestNewToolRejectsInvalidAndExistingNames(t *testing.T) {
	binary := buildNewTool(t)
	assistant := copyAssistant(t)
	before := treeDigest(t, assistant)

	cases := []struct {
		name string
		want string
	}{
		{"", "name is required"},
		{"nodot", "must be <module>.<tool>"},
		{"Upper.case", "must be <module>.<tool>"},
		{"a.b.c", "must be <module>.<tool>"},
		{"bad-name.ok", "must be <module>.<tool>"},
		{"1starts.bad", "must be <module>.<tool>"},
		{"dots.under-score", "must be <module>.<tool>"},
		{"foo__bar.ping", "must not contain"},
		{"foo.ping__pong", "must not contain"},
		{"type.ping", "cannot be a Go package"},
		{"main.ping", "cannot be a Go package"},
		{"vendor.ping", "cannot be a Go package"},
		{"testdata.ping", "cannot be a Go package"},
		{"context.ping", "would collide"},
		{"fmt.ping", "would collide"},
		{"tool.ping", "would collide"},
		{"yaml.ping", "would collide"},
		{"clock.ping", "already exists"},
		{"time.now", "already declared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runNewTool(t, binary, assistant, tc.name)
			if err == nil {
				t.Fatalf("new-tool %q succeeded, want rejection", tc.name)
			}
			if !strings.Contains(output, tc.want) {
				t.Errorf("error output %q does not mention %q", output, tc.want)
			}
			if after := treeDigest(t, assistant); !reflect.DeepEqual(before, after) {
				t.Error("rejected scaffold changed the tree")
			}
			assertNoStagingLeftovers(t, filepath.Join(assistant, "tools"))
		})
	}
}

// TestNewToolRequiresTheAssistantRoot keeps a mistyped -root from creating a
// package in the wrong tree.
func TestNewToolRequiresTheAssistantRoot(t *testing.T) {
	binary := buildNewTool(t)
	if output, err := runNewToolAt(t, binary, t.TempDir(), "greeting.hello"); err == nil {
		t.Fatalf("new-tool accepted a non-module root:\n%s", output)
	}
}

// buildNewTool builds the command once so tests exercise the CLI, not an
// internal function.
func buildNewTool(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "newtool")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build newtool: %v\n%s", err, output)
	}
	return binary
}

// runNewTool runs the command with -root and -name against a copied tree.
func runNewTool(t *testing.T, binary, root, name string) (string, error) {
	t.Helper()
	return runNewToolAt(t, binary, root, name)
}

// runNewToolAt removes an inherited make `name` variable from the child
// environment so only the -name flag decides the request.
func runNewToolAt(t *testing.T, binary, root, name string) (string, error) {
	t.Helper()
	cmd := exec.Command(binary, "-root", root, "-name", name)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "name=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// copyAssistant copies the module into a temp tree, mirroring what a user's
// checkout looks like before running the scaffold.
func copyAssistant(t *testing.T) string {
	t.Helper()
	source := filepath.Join("..", "..")
	destination := filepath.Join(t.TempDir(), "assistant")
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
	if err != nil {
		t.Fatalf("copy assistant tree: %v", err)
	}
	return destination
}

// copyFile copies one regular file with its mode.
func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

// treeDigest hashes every path and file so a rejected scaffold can be proven
// to have changed nothing, including hidden staging files.
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	digest := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			digest[relative] = "dir"
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		digest[relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("digest %s: %v", root, err)
	}
	return digest
}

// assertNoStagingLeftovers catches a failed write leaving temp files behind.
func assertNoStagingLeftovers(t *testing.T, toolsDir string) {
	t.Helper()
	entries, err := os.ReadDir(toolsDir)
	if err != nil {
		t.Fatalf("read tools directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".new-tool-") {
			t.Errorf("scaffold left staging file %q", entry.Name())
		}
	}
}

// readFile reads a generated or edited file as a string.
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
