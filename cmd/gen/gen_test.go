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

	_ = admin.Build()
	registerTenantPurgers(tenancyRegistry,
		// gen:module-purge
	)

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
	for _, want := range []string{`"example.com/app/internal/modules/billing"`, "billingModule, err := billing.Build(infra, billing.Settings{}, billing.Deps{})"} {
		if strings.Count(once, want) != 1 {
			t.Errorf("want exactly one %q in:\n%s", want, once)
		}
	}
	// Once in the module list and once in the non-identity purger list.
	if strings.Count(once, "\t\tbillingModule,\n") != 2 {
		t.Errorf("want billingModule in the module and purger lists:\n%s", once)
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
		"internal/modules/billing/tenant_purge.go",
		"internal/modules/billing/service/ports.go",
		"internal/modules/billing/service/invoice.go",
		"internal/modules/billing/repository/invoice.go",
		"internal/modules/billing/repository/invoice_purge.go",
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
	for _, want := range []string{"Invoices *billinghandler.InvoiceHandler", "m.registerInvoiceRoutes(api)", "m.registerIssueInvoiceRoute(api)"} {
		if !strings.Contains(module, want) {
			t.Errorf("module.go is missing %q", want)
		}
	}
	if !strings.Contains(files["internal/modules/billing/catalog/locales/vi.json"], "errors.billing.invoice_not_found.title") {
		t.Error("vi.json is missing the resource error title")
	}
	if health := files["internal/modules/billing/service/health_test.go"]; strings.Contains(health, "Context(1)") {
		t.Error("generated service tests must pass a tenant UUID, not an integer id")
	}
	sqlcConfig := files["internal/modules/billing/sqlc.yaml"]
	if !strings.Contains(sqlcConfig, `schema: ["../../../migrations/*.up.sql"]`) {
		t.Error("sqlc.yaml must include every up migration so generated resource tables are visible")
	}
	for _, want := range []string{`db_type: "uuid"`, `go_type: "github.com/google/uuid.UUID"`, `db_type: "bytea"`} {
		if !strings.Contains(sqlcConfig, want) {
			t.Errorf("sqlc.yaml is missing %q:\n%s", want, sqlcConfig)
		}
	}
	for _, path := range []string{
		"internal/modules/billing/service/invoice.go",
		"internal/modules/billing/service/invoice_test.go",
		"internal/modules/billing/repository/invoice.go",
		"internal/modules/billing/handler/invoice.go",
		"internal/modules/billing/dto/invoice.go",
		"internal/modules/billing/invoice_integration_test.go",
	} {
		src := files[path]
		if !strings.Contains(src, "uuid.UUID") {
			t.Errorf("%s must use uuid.UUID ids", path)
		}
		if strings.Contains(src, "int64") {
			t.Errorf("%s must not use int64 ids", path)
		}
		if strings.Contains(src, "id <= 0") {
			t.Errorf("%s must compare ids with uuid.Nil, not a numeric zero", path)
		}
		if strings.Contains(src, "item.ID >=") {
			t.Errorf("%s must compare UUID ids through String(), not with >=", path)
		}
	}
	if integ := files["internal/modules/billing/invoice_integration_test.go"]; !strings.Contains(integ, "Sessions: integrationSessions{}") {
		t.Error("generated integration test must wire a session store so authz has the caller's permissions")
	}
	migration := files["migrations/20261025010204_billing_create_invoices.up.sql"]
	for _, want := range []string{
		"id UUID NOT NULL DEFAULT uuidv7()",
		"CONSTRAINT pk_billing_invoices PRIMARY KEY (id)",
		"name TEXT NOT NULL",
		"CONSTRAINT fk_billing_invoices_tenant FOREIGN KEY (tenant_id)",
		"CONSTRAINT chk_billing_invoices_name",
		"CHECK (btrim(name) <> '' AND length(name) <= 255)",
	} {
		if !strings.Contains(migration, want) {
			t.Errorf("resource migration is missing %q:\n%s", want, migration)
		}
	}
	for _, unwanted := range []string{"CREATE TABLE IF NOT EXISTS", "CREATE INDEX IF NOT EXISTS", "VARCHAR"} {
		if strings.Contains(migration, unwanted) {
			t.Errorf("resource migration must fail fast and follow schema conventions; found %q:\n%s", unwanted, migration)
		}
	}
	if !strings.Contains(files["internal/modules/billing/service/ports.go"], `"example.com/app/pkg/pagination"`) {
		t.Error("ports.go must import pagination for the resource port")
	}

	// Tenancy: the repository uses the RLS-scoped handle, the table has RLS and
	// the reviewed policy, and a bounded purger is wired into bootstrap.
	build := strings.Join(strings.Fields(files["internal/modules/billing/build.go"]), " ")
	for _, want := range []string{
		"NewInvoiceRepository(infra.DB)",
		"NewInvoiceTenantPurger(infra.DB)",
		"m.tenantPurgers = append(m.tenantPurgers, billingrepository.NewInvoiceTenantPurger(infra.DB))",
	} {
		if !strings.Contains(build, want) {
			t.Errorf("build.go is missing %q:\n%s", want, build)
		}
	}
	if strings.Contains(build, "NewInvoiceRepository(infra.Pool)") {
		t.Error("resource repository must use infra.DB (RLS-scoped), not the raw pool")
	}
	for _, want := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"FORCE ROW LEVEL SECURITY",
		"CREATE POLICY tenant_isolation ON billing_invoices",
		"app_tenant_visible(tenant_id)",
	} {
		if !strings.Contains(migration, want) {
			t.Errorf("migration is missing %q:\n%s", want, migration)
		}
	}
	if query := files["internal/modules/billing/queries/invoice.sql"]; !strings.Contains(query, "DeleteTenantInvoiceBatch") {
		t.Error("resource queries must include a bounded tenant purge query")
	}
	if purge := files["internal/modules/billing/repository/invoice_purge.go"]; !strings.Contains(purge, "PurgeTenant") ||
		!strings.Contains(purge, "tenancycontracts.PurgeBatchSize") ||
		!strings.Contains(purge, "tenancycontracts.CheckPurgeBudget(ctx)") {
		t.Error("resource repository must include a budget-aware bounded Tenancy Purger")
	}
	if module := files["internal/modules/billing/tenant_purge.go"]; !strings.Contains(module, "TenantPurgers") ||
		!strings.Contains(module, "tenancycontracts.Provider") ||
		!strings.Contains(module, "tenancycontracts.CheckPurgeBudget(ctx)") {
		t.Error("module must expose a budget-aware composite tenant purger")
	}
	if service := files["internal/modules/billing/service/invoice.go"]; strings.Contains(service, "row %d escaped") ||
		!strings.Contains(service, "row %s escaped") {
		t.Error("generated service must format UUID ids as text")
	}
	if bootstrapSrc := files[bootstrapFile]; !strings.Contains(bootstrapSrc, "\t\tbillingModule,\n") {
		t.Errorf("bootstrap must register the billing module's purgers:\n%s", bootstrapSrc)
	}

	// Partial update: PATCH (not PUT) with a pointer patch input.
	routes := files["internal/modules/billing/routes_invoice.go"]
	if !strings.Contains(routes, "Method: http.MethodPatch") || strings.Contains(routes, "Method: http.MethodPut") {
		t.Errorf("resource must mount PATCH, not PUT:\n%s", routes)
	}
	if service := files["internal/modules/billing/service/invoice.go"]; !strings.Contains(service, "type InvoicePatch struct") ||
		!strings.Contains(service, "Name *string") {
		t.Error("service must expose pointer-field partial update input")
	}
	if dto := files["internal/modules/billing/dto/invoice.go"]; !strings.Contains(dto, "InvoicePatchRequest") ||
		!strings.Contains(dto, "Name *string") {
		t.Error("dto must expose pointer-field partial update input")
	}
	if integration := files["internal/modules/billing/invoice_integration_test.go"]; !strings.Contains(integration, "fx.Scoped()") ||
		!strings.Contains(integration, "DB: db") ||
		!strings.Contains(integration, "NewInvoiceTenantPurger(db).PurgeTenant") {
		t.Error("integration test must exercise the RLS app pool and the generated tenant purger, not the bypass pool")
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
		if name == "go" && len(args) >= 2 && args[0] == "tool" && args[1] == "sqlc" {
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
