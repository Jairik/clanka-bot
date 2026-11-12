// Command newtool scaffolds one Tool module: a typed module package, a config
// struct, one placeholder Tool, a conformance test wired to the shared suite,
// and the module-list line in tools.go.
//
// It is the `make new-tool name=<module>.<tool>` implementation. The rest of
// the recipe, including Permission, Local-only, Examples and the routing eval,
// is in docs/agents/adding-a-tool.md.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

// namePattern is the canonical Tool name shape: dotted lowercase snake_case.
// It mirrors internal/tool's name rule so a scaffold cannot create a package
// the registry would later reject at startup.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// scaffoldName is one parsed `<module>.<operation>` scaffold request.
type scaffoldName struct {
	Module    string // package directory and tools.<module> config section
	Canonical string // the full canonical Tool name
	Spoken    string // those words with the punctuation removed for Examples
}

func main() {
	root := flag.String("root", ".", "assistant module root (the directory holding go.mod and tools/)")
	name := flag.String("name", "", "canonical Tool name as <module>.<tool>; falls back to the name environment variable make exports")
	flag.Parse()

	// make passes a command-line `name` through the environment without
	// interpolating it into the shell, so this path is safe for any input.
	if *name == "" {
		*name = os.Getenv("name")
	}
	if err := scaffold(*root, *name, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "new-tool:", err)
		os.Exit(1)
	}
}

// scaffold validates the request, writes the package, then wires the module
// list. It returns before touching the tree when the name or an existing file
// is invalid, and rolls the package back if the module-list edit fails.
func scaffold(root, rawName string, out io.Writer) error {
	name, err := parseName(rawName)
	if err != nil {
		return err
	}
	root, err = checkRoot(root)
	if err != nil {
		return err
	}
	toolsFile := filepath.Join(root, "tools", "tools.go")
	source, err := os.ReadFile(toolsFile)
	if err != nil {
		return fmt.Errorf("read tools/tools.go: %w", err)
	}
	if err := checkExisting(root, name, source); err != nil {
		return err
	}
	files, err := render(name)
	if err != nil {
		return err
	}
	wired, err := wireModule(source, name.Module)
	if err != nil {
		return fmt.Errorf("edit tools/tools.go: %w", err)
	}

	moduleDir := filepath.Join(root, "tools", name.Module)
	if err := writePackage(moduleDir, files); err != nil {
		return err
	}
	// A last-moment re-read keeps a concurrent edit to the module list from
	// being overwritten by this scaffold.
	current, err := os.ReadFile(toolsFile)
	if err != nil {
		removeDir(moduleDir)
		return fmt.Errorf("re-read tools/tools.go: %w", err)
	}
	if !bytes.Equal(current, source) {
		removeDir(moduleDir)
		return errors.New("tools/tools.go changed while scaffolding; re-run the generator")
	}
	if err := writeFileAtomic(toolsFile, wired); err != nil {
		removeDir(moduleDir)
		return fmt.Errorf("write tools/tools.go: %w", err)
	}

	fmt.Fprintf(out, "created tools/%s/ (%s.go, samples.go, conformance_test.go)\n", name.Module, name.Module)
	fmt.Fprintf(out, "wired %s.NewModule() into tools/tools.go\n", name.Module)
	fmt.Fprintln(out, "next: replace the placeholder Tool and follow docs/agents/adding-a-tool.md")
	return nil
}

// parseName validates the canonical name and derives the package and spoken
// forms. Validation happens before any filesystem write.
func parseName(raw string) (scaffoldName, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return scaffoldName{}, errors.New("name is required: make new-tool name=<module>.<tool>")
	}
	if !namePattern.MatchString(raw) {
		return scaffoldName{}, fmt.Errorf("name %q must be <module>.<tool> in lowercase snake_case, e.g. greeting.hello", raw)
	}
	module, operation, _ := strings.Cut(raw, ".")
	words := strings.ReplaceAll(operation, "_", " ")
	return scaffoldName{
		Module:    module,
		Canonical: raw,
		Spoken:    module + " " + words,
	}, nil
}

// checkRoot confirms the target is the assistant module, not a random tree.
func checkRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	for _, required := range []string{"go.mod", "config.yaml", filepath.Join("tools", "tools.go")} {
		if _, err := os.Stat(filepath.Join(absolute, required)); err != nil {
			return "", fmt.Errorf("%s is not the assistant module root: %s is missing", absolute, required)
		}
	}
	return absolute, nil
}

// checkExisting rejects a name that already has a package, a module-list line
// or a canonical Tool constant. This is what makes "existing name" fail before
// any file is overwritten.
func checkExisting(root string, name scaffoldName, toolsSource []byte) error {
	moduleDir := filepath.Join(root, "tools", name.Module)
	if _, err := os.Stat(moduleDir); err == nil {
		return fmt.Errorf("module tools/%s already exists; add the Tool to that module instead", name.Module)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat tools/%s: %w", name.Module, err)
	}
	if bytes.Contains(toolsSource, []byte(name.Module+".NewModule()")) {
		return fmt.Errorf("module %s is already wired in tools/tools.go", name.Module)
	}
	taken, err := toolNameTaken(filepath.Join(root, "tools"), name.Canonical)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("Tool %s is already declared in a module", name.Canonical)
	}
	return nil
}

// toolNameTaken scans the module sources for the canonical name as a string
// literal. Existing modules declare it as `const Name = "<module>.<tool>"`.
func toolNameTaken(toolsDir, canonical string) (bool, error) {
	needle := []byte(strconv.Quote(canonical))
	taken := false
	err := filepath.WalkDir(toolsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(source, needle) {
			taken = true
			return filepath.SkipAll
		}
		return nil
	})
	return taken, err
}

// generatedFile is one file in the new module package.
type generatedFile struct {
	Name    string
	Content []byte
}

// render formats every generated file so the scaffold needs no hand edits.
func render(name scaffoldName) ([]generatedFile, error) {
	examples := []string{
		name.Spoken,
		"please " + name.Spoken,
		name.Spoken + " please",
	}
	data := struct {
		scaffoldName
		DefaultReply string
		Examples     []string
	}{
		scaffoldName: name,
		DefaultReply: "The " + name.Spoken + " Tool is ready.",
		Examples:     examples,
	}
	files := []struct {
		name     string
		template string
	}{
		{name.Module + ".go", moduleTemplate},
		{"samples.go", samplesTemplate},
		{"conformance_test.go", conformanceTemplate},
	}
	rendered := make([]generatedFile, 0, len(files))
	for _, file := range files {
		content, err := renderTemplate(file.name, file.template, data)
		if err != nil {
			return nil, err
		}
		rendered = append(rendered, generatedFile{Name: file.name, Content: content})
	}
	return rendered, nil
}

// renderTemplate executes one template and runs it through gofmt's formatter.
func renderTemplate(label, source string, data any) ([]byte, error) {
	tmpl, err := template.New(label).Funcs(template.FuncMap{"quote": strconv.Quote}).Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse %s template: %w", label, err)
	}
	var raw bytes.Buffer
	if err := tmpl.Execute(&raw, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", label, err)
	}
	formatted, err := format.Source(raw.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated %s: %w", label, err)
	}
	return formatted, nil
}

// wireModule adds the import and module-list line to the one module list.
func wireModule(source []byte, module string) ([]byte, error) {
	text := string(source)
	importBlock := strings.Index(text, "import (")
	if importBlock < 0 {
		return nil, errors.New("no import block")
	}
	importEnd := strings.Index(text[importBlock:], "\n)")
	if importEnd < 0 {
		return nil, errors.New("unterminated import block")
	}
	importAt := importBlock + importEnd + 1
	importLine := "\t" + strconv.Quote("github.com/Jairik/clanka-bot/assistant/tools/"+module) + "\n"
	text = text[:importAt] + importLine + text[importAt:]

	const listMarker = "return &Set{modules: []tool.Module{"
	listStart := strings.Index(text, listMarker)
	if listStart < 0 || strings.Index(text[listStart+len(listMarker):], listMarker) >= 0 {
		return nil, errors.New("module list marker not found exactly once")
	}
	listEnd := strings.Index(text[listStart:], "\n\t}}")
	if listEnd < 0 {
		return nil, errors.New("module list closing brace not found")
	}
	insertAt := listStart + listEnd + 1
	line := "\t\t" + module + ".NewModule(),\n"
	text = text[:insertAt] + line + text[insertAt:]

	formatted, err := format.Source([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("format edited tools.go: %w", err)
	}
	return formatted, nil
}

// writePackage stages the package beside its destination and moves the files
// into a newly created directory, so a failure leaves nothing half-written.
func writePackage(moduleDir string, files []generatedFile) error {
	if _, err := os.Stat(moduleDir); err == nil {
		return fmt.Errorf("module directory %s already exists", moduleDir)
	}
	if err := os.Mkdir(moduleDir, 0o755); err != nil {
		return fmt.Errorf("create module directory: %w", err)
	}
	for _, file := range files {
		path := filepath.Join(moduleDir, file.Name)
		if err := os.WriteFile(path, file.Content, 0o644); err != nil {
			removeDir(moduleDir)
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// writeFileAtomic replaces one file through a same-directory temp file.
func writeFileAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".new-tool-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// removeDir is best-effort rollback after a failed scaffold.
func removeDir(path string) {
	_ = os.RemoveAll(path)
}

// moduleTemplate is the generated module package: config, module value and one
// placeholder Tool. Every replacement point points at the agent guide.
const moduleTemplate = `// Package {{.Module}} is the {{.Module}} Tool module.
// Generated by make new-tool; the recipe is docs/agents/adding-a-tool.md.
package {{.Module}}

import (
	"context"
	"errors"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Name is the canonical {{.Canonical}} Tool name.
const Name = {{quote .Canonical}}

// Config is the tools.{{.Module}} section; replace Reply with the real settings.
type Config struct {
	// Reply is the placeholder Tool's spoken text.
	Reply string ` + "`yaml:\"reply\"`" + `
}

// DefaultConfig is the starter config used without a tools.{{.Module}} section.
func DefaultConfig() Config { return Config{Reply: {{quote .DefaultReply}}} }

// validate rejects an empty reply, which would otherwise Speak nothing.
func (c Config) validate() error {
	if strings.TrimSpace(c.Reply) == "" {
		return errors.New("reply must not be empty")
	}
	return nil
}

// Module is the {{.Module}} Tool domain.
type Module struct {
	cfg Config
}

// NewModule builds the {{.Module}} module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return {{quote .Module}} }

// Configure decodes tools.{{.Module}} over the code defaults.
func (m *Module) Configure(section *yaml.Node) error {
	cfg := DefaultConfig()
	if section != nil {
		if err := section.Decode(&cfg); err != nil {
			return err
		}
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// In has no fields; add fields when the Tool takes arguments.
type In struct{}

// Out is the placeholder result; replace it with the real operation's output.
type Out struct {
	Reply string ` + "`json:\"reply\"`" + `
}

// New builds the {{.Canonical}} Tool. Choose Permission and LocalOnly for the
// real operation; read and LocalOnlyNo are only the safe starter values.
// See docs/agents/adding-a-tool.md.
func New(cfg Config) tool.Tool[In, Out] {
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Placeholder for {{.Canonical}}; replace it with the real operation.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, In) (Out, error) {
			return Out{Reply: cfg.Reply}, nil
		},
		Speak: func(out Out) string {
			return out.Reply
		},
		// Each Example must route to this Tool on the semantic Fast path; replace
		// these starter phrasings with real user wording (docs/agents/adding-a-tool.md).
		Examples: []string{
			{{range .Examples}}{{quote .}},
			{{end}}
		},
	}
}

// Register adds {{.Canonical}} to the registry; use deps for the shared seams.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	return tool.Add(r, New(m.cfg))
}
`

// samplesTemplate gives the conformance suite one successful output for Speak.
const samplesTemplate = `package {{.Module}}

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: Out{Reply: DefaultConfig().Reply},
	}
}
`

// conformanceTemplate is the external test package; an internal test would
// import-cycle through conformance -> tools -> module.
const conformanceTemplate = `package {{.Module}}_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/tools/conformance"
	"github.com/Jairik/clanka-bot/assistant/tools/{{.Module}}"
)

// TestModuleConformance is the one-line shared contract check; the real-model
// routing part runs when ROUTING_EVAL_EMBED_URL is set (docs/agents/adding-a-tool.md).
func TestModuleConformance(t *testing.T) {
	conformance.Run(t, {{.Module}}.NewModule())
}
`
