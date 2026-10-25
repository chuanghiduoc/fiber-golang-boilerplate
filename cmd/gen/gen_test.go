package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testModulePath = "example.com/app"

// fakeBootstrap is the part of internal/bootstrap/modules.go cmd/gen edits.
const fakeBootstrap = `package bootstrap

import (
	"context"

	"example.com/app/internal/app"
	"example.com/app/internal/modules/admin"
)

func Build(infra app.Infra) ([]app.Module, error) {
	_ = context.Background()
	// gen:module-build

	return []app.Module{
		admin.Build(),
		// gen:module-list
	}, nil
}
`

// newTestRepo creates a minimal repository layout and a generator over it
// whose verification steps are recorded instead of executed.
func newTestRepo(t *testing.T) (gen *generator, verified *[]string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module " + testModulePath + "\n\ngo 1.27\n",
		"migrations/20260101000000_platform_init_schema.up.sql": "SELECT 1;\n",
		bootstrapFile: fakeBootstrap,
	}
	for rel, content := range files {
		target := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var ran []string
	g := &generator{
		repo:       repo,
		modulePath: testModulePath,
		out:        &bytes.Buffer{},
		now:        func() time.Time { return time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC) },
		run: func(_ context.Context, name string, args ...string) error {
			ran = append(ran, step{name, args}.String())
			return nil
		},
	}
	return g, &ran
}

// tree returns every file below the generator root with its content.
func tree(t *testing.T, g *generator) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(g.repo.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := g.repo.ReadFile(filepath.FromSlash(rel))
		out[rel] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNames(t *testing.T) {
	cases := map[string]string{
		pascal("line_item"):                "LineItem",
		camel("line_item"):                 "lineItem",
		snakeFromPascal("IssueInvoice"):    "issue_invoice",
		snakeFromPascal("ExportCSVReport"): "export_csv_report",
		pluralSnake("invoice"):             "invoices",
		pluralSnake("line_item"):           "line_items",
		pluralSnake("category"):            "categories",
		pluralSnake("key"):                 "keys",
		pluralSnake("box"):                 "boxes",
		pluralSnake("batch"):               "batches",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	for _, bad := range []string{"Billing", "billing-x", "1billing", "bill__ing", "../x", "type", "service", "vendor"} {
		if validateModuleName(bad) == nil {
			t.Errorf("module name %q must be rejected", bad)
		}
	}
	for _, bad := range []string{"issueInvoice", "Issue", "Issue_Invoice"} {
		if validateUseCaseName(bad) == nil {
			t.Errorf("use-case name %q must be rejected", bad)
		}
	}
}

func TestInsertAtMarker(t *testing.T) {
	src := "a\n\t// gen:x\nb\n"
	out, err := insertAtMarker("f", src, "// gen:x", "one\ntwo")
	if err != nil {
		t.Fatal(err)
	}
	if want := "a\n\tone\n\ttwo\n\t// gen:x\nb\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if _, err := insertAtMarker("f", "a\n", "// gen:x", "x"); err == nil {
		t.Fatal("a missing marker must fail")
	}
	if _, err := insertAtMarker("f", "// gen:x\n// gen:x\n", "// gen:x", "x"); err == nil {
		t.Fatal("a duplicated marker must fail")
	}
}

func TestEnsureImports(t *testing.T) {
	src := "package p\n\nimport (\n\t\"context\"\n\n\t\"example.com/app/a\"\n)\n"
	out, err := ensureImports("p.go", src, "context", "time", "example.com/app/b")
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := formatGo("p.go", []byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := "package p\n\nimport (\n\t\"context\"\n\t\"time\"\n\n\t\"example.com/app/a\"\n\t\"example.com/app/b\"\n)\n"
	if string(formatted) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", formatted, want)
	}

	// A stdlib-only block gets a separate module group.
	stdOnly := "package p\n\nimport (\n\t\"context\"\n)\n"
	out, err = ensureImports("p.go", stdOnly, "example.com/app/b")
	if err != nil {
		t.Fatal(err)
	}
	if formatted, err = formatGo("p.go", []byte(out)); err != nil {
		t.Fatal(err)
	}
	if want := "package p\n\nimport (\n\t\"context\"\n\n\t\"example.com/app/b\"\n)\n"; string(formatted) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", formatted, want)
	}
}

func TestRegisterInBootstrapIsIdempotent(t *testing.T) {
	once, err := registerInBootstrap(fakeBootstrap, testModulePath, "billing")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := registerInBootstrap(once, testModulePath, "billing")
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Fatal("a second registration must not change the file")
	}
	for _, want := range []string{`"example.com/app/internal/modules/billing"`, "billingModule, _, err := billing.Build(infra, billing.Settings{})", "\t\tbillingModule,\n"} {
		if strings.Count(once, want) != 1 {
			t.Errorf("want exactly one %q in:\n%s", want, once)
		}
	}
}

func TestMergeLocaleRejectsDuplicates(t *testing.T) {
	out, err := mergeLocale("en.json", []byte(`{"b":"2","a":"<1>"}`), map[string]string{"c": "Không"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"a\": \"<1>\",\n  \"b\": \"2\",\n  \"c\": \"Không\"\n}\n"; string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if _, err := mergeLocale("en.json", out, map[string]string{"a": "x"}); err == nil {
		t.Fatal("an existing key must not be replaced")
	}
}

func TestUnifiedDiff(t *testing.T) {
	got := unifiedDiff("f.txt", false, "a\nb\nc\n", "a\nB\nc\n")
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if created := unifiedDiff("n.txt", true, "", "x\n"); !strings.HasPrefix(created, "--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,1 @@\n+x\n") {
		t.Fatalf("created diff = %q", created)
	}
}

func TestGenerateEverySliceAndVerify(t *testing.T) {
	g, ran := newTestRepo(t)
	ctx := context.Background()
	if err := g.generateModule(ctx, "billing"); err != nil {
		t.Fatalf("module: %v", err)
	}
	if err := g.generateResource(ctx, "billing", "invoice", ""); err != nil {
		t.Fatalf("resource: %v", err)
	}
	if err := g.generateUseCase(ctx, kindCommand, "billing", "IssueInvoice", true); err != nil {
		t.Fatalf("command: %v", err)
	}
	if err := g.generateUseCase(ctx, kindQuery, "billing", "ListOverdueInvoices", false); err != nil {
		t.Fatalf("query: %v", err)
	}

	files := tree(t, g)
	for _, want := range []string{
		"internal/modules/billing/module.go",
		"internal/modules/billing/build.go",
		"internal/modules/billing/service/ports.go",
		"internal/modules/billing/service/invoice.go",
		"internal/modules/billing/repository/invoice.go",
		"internal/modules/billing/handler/command_issue_invoice.go",
		"internal/modules/billing/service/query_list_overdue_invoices.go",
		"migrations/20261025010203_billing_init.up.sql",
		"migrations/20261025010204_billing_create_invoices.up.sql",
		"migrations/20261025010204_billing_create_invoices.down.sql",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if _, ok := files["internal/modules/billing/handler/query_list_overdue_invoices.go"]; ok {
		t.Error("--no-http must not generate a handler")
	}
	// gofmt aligns struct fields, so compare with whitespace collapsed.
	module := strings.Join(strings.Fields(files["internal/modules/billing/module.go"]), " ")
	for _, want := range []string{"Invoices *billinghandler.InvoiceHandler", "m.registerInvoiceRoutes(group, deps)", "m.registerIssueInvoiceRoute(group, deps)"} {
		if !strings.Contains(module, want) {
			t.Errorf("module.go is missing %q", want)
		}
	}
	if !strings.Contains(files["internal/modules/billing/catalog/locales/vi.json"], "errors.billing.invoice_not_found.title") {
		t.Error("vi.json is missing the resource error title")
	}
	if !strings.Contains(files["internal/modules/billing/service/ports.go"], `"example.com/app/pkg/pagination"`) {
		t.Error("ports.go must import pagination for the resource port")
	}
	if len(*ran) == 0 || !strings.Contains(strings.Join(*ran, "\n"), "go test -count=1 ./test/arch/...") {
		t.Fatalf("verification steps = %v", *ran)
	}
	if err := g.generateResource(ctx, "billing", "invoice", ""); err == nil {
		t.Fatal("generating an existing resource must fail")
	}
}

func TestFailedVerificationRollsBackEveryWrite(t *testing.T) {
	g, _ := newTestRepo(t)
	ctx := context.Background()
	if err := g.generateModule(ctx, "billing"); err != nil {
		t.Fatal(err)
	}
	// Simulate sqlc output that the failing run regenerates.
	if err := g.repo.MkdirAll(filepath.FromSlash("internal/modules/billing/sqlc"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := g.repo.WriteFile(filepath.FromSlash("internal/modules/billing/sqlc/db.go"), []byte("package sqlc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := tree(t, g)

	g.run = func(_ context.Context, name string, args ...string) error {
		if name == "sqlc" {
			// The regenerated directory changes before the later failure.
			if err := g.repo.WriteFile(filepath.FromSlash("internal/modules/billing/sqlc/invoice.sql.go"), []byte("package sqlc\n"), 0o600); err != nil {
				return err
			}
			return nil
		}
		if len(args) > 0 && args[0] == "test" {
			return errors.New("tests failed")
		}
		return nil
	}
	err := g.generateResource(ctx, "billing", "invoice", "")
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("generateResource = %v, want a rolled-back failure", err)
	}
	after := tree(t, g)
	if len(after) != len(before) {
		t.Fatalf("rollback left %d files, want %d", len(after), len(before))
	}
	for rel, content := range before {
		if after[rel] != content {
			t.Errorf("%s changed after rollback", rel)
		}
	}
	if _, err := g.repo.Stat(filepath.FromSlash("internal/modules/billing/routes_invoice.go")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("created file survived rollback: %v", err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	g, ran := newTestRepo(t)
	out := &bytes.Buffer{}
	g.out, g.dryRun = out, true
	before := tree(t, g)
	if err := g.generateModule(context.Background(), "billing"); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatalf("dry run executed %v", *ran)
	}
	if after := tree(t, g); len(after) != len(before) || after[bootstrapFile] != before[bootstrapFile] {
		t.Fatal("dry run changed the tree")
	}
	for _, want := range []string{"+++ b/internal/modules/billing/module.go", "--- a/" + bootstrapFile, "+\tbillingModule,"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output is missing %q", want)
		}
	}
}

func TestRefusesHandWrittenModules(t *testing.T) {
	g, _ := newTestRepo(t)
	if err := g.repo.MkdirAll(filepath.FromSlash("internal/modules/legacy"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := g.repo.WriteFile(filepath.FromSlash("internal/modules/legacy/module.go"), []byte("package legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.generateResource(context.Background(), "legacy", "thing", ""); err == nil {
		t.Fatal("a module without gen: markers must be refused")
	}
	if err := g.generateModule(context.Background(), "legacy"); err == nil {
		t.Fatal("an existing module directory must be refused")
	}
}

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions("resource", []string{"invoice", "--module", "billing", "--dry-run"})
	if err != nil || opts.name != "invoice" || opts.module != "billing" || !opts.dryRun {
		t.Fatalf("opts = %+v, err = %v", opts, err)
	}
	if _, err := parseOptions("resource", []string{"invoice"}); !errors.Is(err, errUsage) {
		t.Fatalf("missing --module = %v, want usage", err)
	}
	if _, err := parseOptions("module", []string{"a", "b"}); !errors.Is(err, errUsage) {
		t.Fatalf("two names = %v, want usage", err)
	}
}
