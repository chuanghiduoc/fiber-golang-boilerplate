package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

//go:embed templates
var templates embed.FS

// generator plans and applies one cmd/gen run against a repository root.
type generator struct {
	repo       *os.Root // repository root; every file access goes through it
	modulePath string
	dryRun     bool
	out        io.Writer
	now        func() time.Time
	// run executes one verification command in root; tests replace it.
	run func(ctx context.Context, name string, args ...string) error
}

// step is one post-generation verification command.
type step struct {
	name string
	args []string
}

func (s step) String() string { return strings.Join(append([]string{s.name}, s.args...), " ") }

// moduleData names one module in every spelling the templates need.
type moduleData struct {
	ModulePath string // Go module path from go.mod
	Name       string // billing_probe: directory, package, catalog owner
	Ident      string // billingprobe: import-alias prefix and dto package
	Pascal     string // BillingProbe: Swagger tag
	Route      string // billing-probe: URL segment
}

// resourceData names one CRUD resource.
type resourceData struct {
	Name         string // line_item
	Pascal       string // LineItem
	Camel        string // lineItem
	Plural       string // line_items
	PluralPascal string // LineItems
	PluralRoute  string // line-items
	Route        string // line-item
	Human        string // line item
	HumanPlural  string // line items
	Title        string // Line item
	Table        string // billing_line_items
	Model        string // BillingLineItem: sqlc's singularized table struct
}

// useCaseData names one command or query.
type useCaseData struct {
	Kind       string // command | query
	IsCommand  bool
	Name       string // IssueInvoice
	Camel      string // issueInvoice
	Snake      string // issue_invoice
	Route      string // issue-invoice
	Human      string // Issue invoice
	Permission string // catalog constant: PermissionWrite | PermissionRead
}

type templateData struct {
	M moduleData
	R resourceData
	U useCaseData
}

func newModuleData(modulePath, name string) moduleData {
	return moduleData{
		ModulePath: modulePath,
		Name:       name,
		Ident:      strings.ReplaceAll(name, "_", ""),
		Pascal:     pascal(name),
		Route:      strings.ReplaceAll(name, "_", "-"),
	}
}

func newResourceData(m moduleData, name, pluralName string) resourceData {
	human := strings.ReplaceAll(name, "_", " ")
	table := m.Name + "_" + pluralName
	model := pascal(m.Name + "_" + name)
	if pluralName == m.Name {
		// widgets/widget: the table is the module name itself.
		table = m.Name
		model = pascal(name)
	}
	return resourceData{
		Name:         name,
		Pascal:       pascal(name),
		Camel:        camel(name),
		Plural:       pluralName,
		PluralPascal: pascal(pluralName),
		PluralRoute:  strings.ReplaceAll(pluralName, "_", "-"),
		Route:        strings.ReplaceAll(name, "_", "-"),
		Human:        human,
		HumanPlural:  strings.ReplaceAll(pluralName, "_", " "),
		Title:        strings.ToUpper(human[:1]) + human[1:],
		Table:        table,
		Model:        model,
	}
}

func newUseCaseData(kind, name string) useCaseData {
	snake := snakeFromPascal(name)
	human := strings.ReplaceAll(snake, "_", " ")
	permission := "PermissionRead"
	if kind == kindCommand {
		permission = "PermissionWrite"
	}
	return useCaseData{
		Kind:       kind,
		IsCommand:  kind == kindCommand,
		Name:       name,
		Camel:      camel(snake),
		Snake:      snake,
		Route:      strings.ReplaceAll(snake, "_", "-"),
		Human:      strings.ToUpper(human[:1]) + human[1:],
		Permission: permission,
	}
}

const (
	kindCommand = "command"
	kindQuery   = "query"
)

// render executes one embedded template. Go output is gofmt-formatted.
func render(name string, data templateData) ([]byte, error) {
	raw, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("execute template %s: %w", name, err)
	}
	if strings.HasSuffix(name, ".go.tmpl") {
		return formatGo(name, b.Bytes())
	}
	return b.Bytes(), nil
}

// readModulePath returns the module path declared in root/go.mod.
func readModulePath(repo *os.Root) (string, error) {
	raw, err := repo.ReadFile("go.mod")
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", errors.New("go.mod has no module directive")
}

// findRoot walks up from dir to the directory holding go.mod.
func findRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found in this directory or any parent")
		}
		dir = parent
	}
}

// migrationVersion matches a timestamp migration version prefix.
var migrationVersion = regexp.MustCompile(`^(\d{14})_`)

// nextMigrationVersion returns a UTC timestamp version newer than every
// existing timestamp migration, so two runs in the same second (module then
// resource) never share a version.
func (g *generator) nextMigrationVersion(reserved []string) (string, error) {
	const layout = "20060102150405"
	entries, err := fs.ReadDir(g.repo.FS(), "migrations")
	if err != nil {
		return "", fmt.Errorf("read migrations: %w", err)
	}
	next := g.now().UTC().Truncate(time.Second)
	names := append([]string{}, reserved...)
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	for _, name := range names {
		match := migrationVersion.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		existing, err := time.Parse(layout, match[1])
		if err != nil {
			continue
		}
		if !existing.Before(next) {
			next = existing.Add(time.Second)
		}
	}
	return next.Format(layout), nil
}

func (g *generator) moduleDir(name string) string { return path.Join("internal/modules", name) }

// readRepoFile reads a repository-relative file.
func (g *generator) readRepoFile(rel string) (string, error) {
	raw, err := g.repo.ReadFile(native(rel))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// exists reports whether a repository-relative path exists.
func (g *generator) exists(rel string) (bool, error) {
	_, err := g.repo.Stat(native(rel))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// requireGeneratedModule fails unless internal/modules/<name> was created by
// cmd/gen (its module.go carries the gen: markers).
func (g *generator) requireGeneratedModule(name string) error {
	if err := validateModuleName(name); err != nil {
		return err
	}
	src, err := g.readRepoFile(g.moduleDir(name) + "/module.go")
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("module %q does not exist; create it first with: make gen module=%s", name, name)
	}
	if err != nil {
		return err
	}
	if !strings.Contains(src, markerRoutes) {
		return fmt.Errorf("module %q was not created by cmd/gen (no %q marker in module.go); extend it by hand", name, markerRoutes)
	}
	return nil
}

// editFile applies edit to a repository file's current content (or to the
// content already planned for it) and records the result as an update.
func (g *generator) editFile(p *plan, rel string, edit func(src string) (string, error)) error {
	src := ""
	planned := -1
	for i, op := range p.ops {
		if op.path == rel {
			src, planned = string(op.content), i
		}
	}
	if planned < 0 {
		current, err := g.readRepoFile(rel)
		if err != nil {
			return err
		}
		src = current
	}
	out, err := edit(src)
	if err != nil {
		return err
	}
	content := []byte(out)
	if strings.HasSuffix(rel, ".go") {
		if content, err = formatGo(rel, content); err != nil {
			return err
		}
	}
	if planned >= 0 {
		p.ops[planned].content = content
		return nil
	}
	p.update(rel, content)
	return nil
}

// execute prints the plan (dry run) or applies it, runs the verification
// steps, and rolls every write back if one fails.
func (g *generator) execute(ctx context.Context, p *plan, steps []step, nextSteps []string) error {
	if g.dryRun {
		if err := p.printDiff(g.out); err != nil {
			return err
		}
		fmt.Fprintln(g.out, "Dry run: nothing written. Verification that would run:")
		for _, s := range steps {
			fmt.Fprintf(g.out, "  %s\n", s)
		}
		return nil
	}
	tx, err := p.apply()
	if err != nil {
		return err
	}
	for _, s := range steps {
		fmt.Fprintf(g.out, "==> %s\n", s)
		if err := g.run(ctx, s.name, s.args...); err != nil {
			rollbackErr := tx.rollback()
			if rollbackErr != nil {
				return errors.Join(fmt.Errorf("%s failed: %w", s, err), fmt.Errorf("rollback incomplete: %w", rollbackErr))
			}
			return fmt.Errorf("%s failed (every file this run wrote was rolled back): %w", s, err)
		}
	}
	fmt.Fprintln(g.out, "Generated and verified:")
	for _, rel := range p.paths() {
		fmt.Fprintf(g.out, "  %s\n", rel)
	}
	if len(nextSteps) > 0 {
		fmt.Fprintln(g.out, "Next steps:")
		for i, s := range nextSteps {
			fmt.Fprintf(g.out, "  %d. %s\n", i+1, s)
		}
	}
	return nil
}

// verifySteps are the checks every run must pass before it is kept.
func verifySteps(module string, sqlc bool) []step {
	pkgs := "./internal/modules/" + module + "/..."
	var steps []step
	if sqlc {
		steps = append(steps, step{"sqlc", []string{"generate", "-f", "internal/modules/" + module + "/sqlc.yaml"}})
	}
	return append(steps,
		step{"go", []string{"build", "./..."}},
		step{"go", []string{"vet", pkgs}},
		step{"go", []string{"vet", "-tags=integration", pkgs}},
		step{"go", []string{"test", "-count=1", pkgs}},
		step{"go", []string{"test", "-count=1", "./test/arch/..."}},
	)
}
