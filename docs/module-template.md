# Module Template

Canonical layout and checklist for a new business module in this modular
monolith. Read it with [Architecture](architecture.md) (dependency and schema
rules) and [Operations](operations.md) (runtime behavior).

## Quick start

`cmd/gen` (Go stdlib `text/template` + `embed`) generates code in the
 layout, so `test/arch` (including the
layering ratchet) passes for generated code with no allowlist entries.

```bash
make gen module=billing                              # module skeleton + bootstrap entry
make gen resource=invoice module=billing             # tenant-scoped CRUD slice
make gen command=IssueInvoice module=billing         # service/command_issue_invoice.go + POST route
make gen query=ListOverdueInvoices module=billing    # service/query_list_overdue_invoices.go + GET route
make gen module=billing dry_run=1                    # print a unified diff, write nothing
make gen command=IssueInvoice module=billing no_http=1   # use case only, no handler/route
```

The targets call `go run ./cmd/gen <module|resource|command|query> [--dry-run]
[--no-http] [--plural <plural>] [--module <module>] <name>` (flags may follow
the name; `go run ./cmd/gen -h` prints the usage). `scripts/new-module.sh
<name>` remains as a thin wrapper for `go run ./cmd/gen module <name>`.

```mermaid
flowchart LR
    A["make gen ..."] --> B["render every file in memory<br/>(gofmt, marker edits)"]
    B -- "dry_run=1" --> D["print unified diff"]
    B --> C["write files"]
    C --> E["sqlc generate (module/resource)<br/>go build ./...<br/>go vet (+ -tags=integration)<br/>go test module + test/arch"]
    E -- fails --> X["restore/remove every file<br/>this run wrote (incl. sqlc/)"]
    E -- passes --> F["review migration, grants, vi strings<br/>make migrate-run · make openapi"]
```

| Command | Creates | Edits (at `gen:` markers) |
| --- | --- | --- |
| `module=<name>` | `module.go`, `build.go`, `generate.go`, `tenant_purge.go`, `sqlc.yaml`, `README.md`, `module_test.go`, `catalog/` (permissions, errors, en/vi), `service/{ports,authorize,health}.go`, `repository/health.go`, `handler/health.go`, `dto/dto.go`, `queries/health.sql`, unit tests, `migrations/<UTC ts>_<name>_init.{up,down}.sql` | `internal/bootstrap/modules.go` (import, `Build`, `List`, tenant-purger registration; idempotent) |
| `resource=<r> module=<m>` | `migrations/<UTC ts>_<m>_create_<plural>.{up,down}.sql`, `queries/<r>.sql`, `service/<r>.go`, `repository/<r>.go` (+ shared `repository/errors.go`), `repository/<r>_purge.go`, `handler/<r>.go`, `dto/<r>.go`, `routes_<r>.go`, service/handler tests with hand-written fakes, `<r>_integration_test.go` | `service/ports.go`, `module.go`, `build.go`, `catalog/{permissions,errors}.go`, `catalog/locales/{en,vi}.json`, `README.md` |
| `command=<Name>` / `query=<Name>` | `service/{command,query}_<name>.go` (+ test); unless `no_http=1`: `handler/`, `dto/`, `routes_*.go` (+ handler test) | `module.go`, `build.go`, `README.md` |

Rules the generator enforces:

- Module and resource names are lower_snake_case (Go keywords, layout names,
  `vendor`/`testdata` rejected); use-case names are PascalCase with at least
  two words. Nothing is ever overwritten.
- `resource`, `command`, and `query` only extend a module created by `cmd/gen`
  (its `module.go` carries the `// gen:routes` marker); keep the `gen:`
  marker comments in generated files and in `internal/bootstrap/modules.go`.
- A migration version is a UTC timestamp newer than every existing migration,
  so `module` then `resource` in the same second never collide.
- The resource table is `<module>_<plural>` (`--plural` for irregular
  nouns); sqlc's model is the singularized table name. It is tenant-owned, so
  the generator also emits `ENABLE`/`FORCE ROW LEVEL SECURITY`, the
  `tenant_isolation` policy, a merge-patch `PATCH` with an ETag compare-and-swap
  `UPDATE`, and a tenant `Purger` registered through the module's
  `tenant_purge.go`.

Generated code is owner/admin-only until you grant roles in `catalog/permissions.go`,
and commands/queries return `501 <module>.not_implemented` until their
`Handle` is written, so an unfinished use case never reports success. Do not
commit a probe module created only to try the generator.

## 1. Module anatomy

```mermaid
flowchart TB
    subgraph M["internal/modules/&lt;name&gt;"]
        MOD["module.go<br/>Name · Build · RegisterRoutes"]
        H["handler<br/>bind · validate · respond"]
        S["service<br/>rules · ownership · tx · jobs"]
        R["repository<br/>SQL · row mapping"]
        Q["sqlc (generated)<br/>from queries/*.sql"]
        D["dto<br/>transport types"]
        CAT["catalog (leaf)<br/>permissions · errors · flags · locales"]
    end
    APP["internal/app<br/>Module registry, Infra, Deps"]
    C["internal/contracts/&lt;provider&gt;<br/>stdlib-only interfaces"]
    PG[(PostgreSQL)]

    APP -- "Register → RegisterRoutes(api, deps)" --> MOD
    APP -. "optional: RegisterEvents / RegisterJobs / RegisterSchedules / RegisterRoutes" .-> MOD
    APP -. "Catalogs.Add: Permissions /<br/>Errors / Locales / FeatureFlags" .-> CAT
    MOD --> H --> S --> R --> Q --> PG
    H --> D
    S -- "consumes" --> C
    H & S & R --> CAT
```

### Layout

| Path | Responsibility |
| --- | --- |
| `module.go` | `package <name>`: `app.Module`, catalog providers, route table |
| `build.go` | `Build(infra app.Infra, settings Settings, deps Deps) (*Module, error)`, the module's `Settings` and the contracts it consumes in `Deps` (see [section 2](#2-scaffold-skeleton)) |
| `generate.go`, `sqlc.yaml` | `//go:generate sqlc generate -f sqlc.yaml`; the module's own sqlc configuration (paths relative to the file) |
| `module_test.go` | catalog registration/localization and a route test through `app.New` |
| `README.md` | ownership, routes, permissions, tables, jobs, contracts |
| `catalog/catalog.go` | `Owner` (== `Module.Name()`), `Register()`, `Locales()` (embedded) |
| `catalog/permissions.go` | `authz.Permission` constants + `Permissions() []authz.Grant` |
| `catalog/errors.go` | `apperror.Code` constants + `Errors() []apperror.Spec` |
| `catalog/flags.go` | optional: `features.Key` constants + `FeatureFlags()` |
| `catalog/locales/{en,vi}.json` | keys `<name>.*` or `errors.<code>.*` |
| `dto/` | request/response transport types; no HTTP or SQL |
| `handler/` | bind/validate HTTP, call service, write responses |
| `service/` | use cases, authorization (`authz.Authorize`), ownership checks; `ports.go` declares the interfaces it needs; `command_*.go` / `query_*.go` for CQRS slices |
| `repository/` | implements the service ports with sqlc and maps rows to service types; no HTTP |
| `queries/*.sql` | sqlc source owned by this module |
| `sqlc/` | generated by `make sqlc-generate` (do not edit) |
| `migrations/<UTC timestamp>_<name>_<purpose>.{up,down}.sql` | repository-level, owned by the module (`test/arch` checks the owner prefix and up/down pairs) |

When one service grows large, split it into cohesive subpackages behind the
same module boundary. The identity module is the reference:
`service/core` (domain types, ports, errors, transaction interfaces),
`service/authn` (registration, login, lockout, sessions, password flows,
email verification, Google OAuth), `service/mfa` (TOTP, recovery codes,
challenges, step-up, passkeys), `service/membership` (members, roles, rank
hierarchy, invitations, admin user operations, stats), `service/tenancy`
(tenant lifecycle, trial expiry, deletion finalize/purge), and
`service/privacy` (exports, erasure, finalizer). Each package exports a
concrete service struct; handlers and `build.go` consume only the narrow
interfaces they use. Shared services and helpers live in `core`, never in a
package-level registry.

### Optional module hooks

`app.Module` is only `Name()`. The HTTP surface is the optional (during the
huma conversion: required for a mounted module) `app.RouteProvider`; everything
else is an optional interface detected by type assertion:

| Interface | Method | Invoked by |
| --- | --- | --- |
| `app.RouteProvider` | `RegisterRoutes(*httpapi.API, deps)` | `app.Register` |
| `app.PermissionProvider` | `Permissions() []authz.Grant` | `app.Catalogs.Register` |
| `app.ErrorProvider` | `Errors() []apperror.Spec` | `app.Catalogs.Register` |
| `app.LocaleProvider` | `Locales() fs.FS` | `app.LocaleSources` → `i18n.New` |
| `app.FlagProvider` | `FeatureFlags() []features.Definition` | `app.Catalogs.Register` |
| `app.ScheduleProvider` | `RegisterSchedules(*scheduler.Registry)` | `app.Register`, only when `deps.Schedules` is non-nil |
| `app.EventProvider` | `RegisterEvents(*events.Bus)` | `app.Register`, only when `deps.Events` is non-nil |
| `app.JobProvider` | `RegisterJobs(jobs.Registrar)` | `app.Register`, only when `deps.Jobs` is non-nil |

After every module is registered, durable event subscriptions are wired into
the worker (`events.RegisterJobs`).

### Import direction

| Package | Owns | May import | Must not import |
| --- | --- | --- | --- |
| `module.go` | identity, `Build`, catalog providers, routes | `internal/app`, own subpackages, `internal/contracts/*`, `platform`, `pkg` | another module |
| `catalog` | permissions, error codes, flags, en/vi strings | `platform/authz`, `platform/features`, `pkg/apperror`, stdlib | anything else (leaf, so every layer may import it) |
| `handler` | HTTP binding, validation, status/response shape, mapping service types to DTOs | own `dto`, own `service`, `platform/httpx`, `*http` adapters, `pkg` | own `repository`, sqlc, pgx |
| `service` | rules, authorization decisions (`authz.Authorize`), transactions, jobs | own service types and repository/port interfaces, `internal/contracts`, framework-free `platform`/`pkg` cores | Fiber/fasthttp (even transitively), sqlc, pgx, own `dto`/`handler` |
| `repository` | SQL, sqlc rows, persistence errors, mapping rows to service types | own `sqlc`, own service/domain types (existing code also uses `dto`), `internal/database`, pgx | own `handler`, Fiber, another module's `sqlc` |
| `dto` | transport structs, validation tags | stdlib (or `pkg/pagination`) | repository, sqlc, Fiber |

`test/arch/layering_test.go` enforces the `service`/`handler` bans;
violations that predate the rule
are frozen in `test/arch/layering_allowlist.txt`. New code must not add to
it, and fixing one means deleting its line. `test/arch/framework_free_test.go`
walks the transitive imports of every `service`/`repository` and of the core
platform packages and fails on Fiber or fasthttp. Fiber code goes in
`handler/` or a platform `<pkg>http` adapter (`authzhttp`, `audithttp`,
`tenanthttp`, `featureshttp`, `apikeyhttp`, `apperrorhttp`).

### Reference module: `files`

`internal/modules/files` is the reference implementation of the layering
rules. Copy its shape.

| Path | Role |
| --- | --- |
| `service/ports.go` | service types (`File`, `NewFile`, `FileStats`, `ErrStoragePathTaken`) and the ports: `FileRepository`, `Transactor`, and the transaction-bound `TxRepos{Files, Audit, Events}` |
| `service/*_service.go` | use cases; imports only ports, `internal/contracts`, platform cores (`authz`, `audit`, `tenant`), `pkg/storage`, `pkg/apperror`, `pkg/pagination` |
| `service/events.go` | maps `File` to the `internal/contracts/files` event payloads and publishes them through `TxRepos.Events` |
| `repository/file_repository.go` | implements `service.FileRepository` over sqlc; `toFile` maps rows to `service.File`, and Postgres errors become `apperror.ErrNotFound` / `service.ErrStoragePathTaken` |
| `repository/tx.go` | `Transactor.InTx` opens a `database.TxManager` transaction and hands the service a `TxRepos` whose repository, audit writer, and event publisher are all bound to that `pgx.Tx` |
| `handler/mapping.go` | `service.File` → `dto.FileResponse` (download URL, `deletedAt`) |
| `adapter/admin.go` | `service.File` → `internal/contracts/files` types for the admin module |
| `build.go` | the only place that wires concrete types: repository, `NewTransactor(infra.Tx, infra.Audit, infra.Events)`, services, handler, adapter |

Transactional side effects never expose `pgx.Tx` to the service. The use case
writes through the collaborators it is given:

```go
err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
	file, err := m.apply(ctx, tx.Files)
	if err != nil {
		return err
	}
	if err := tx.Audit.RecordTx(ctx, event); err != nil { // Tier 1 audit row
		return filesAuditWriteError(err)
	}
	// Domain event in the same transaction (events.PublishTx); webhooks
	// subscribes durably, so files never imports it.
	return publishEvent(ctx, tx.Events, m.eventType, m.dataVersion, file.TenantID, m.eventData(file))
})
```

`files.Build(infra, settings, files.Deps{})` takes no other module: events go through
`infra.Events`. Unit tests use an in-memory `FileRepository` and a fake
`Transactor` (`service/upload_service_mocks_test.go`); rollback is covered
against PostgreSQL in `test/integration`.

Why a `catalog` subpackage: the module root imports its handler and service, so
a handler referencing a root constant would be an import cycle. The leaf can be
imported by every layer and by tests.

Keep the module root small. If a module needs several vertical slices (for
example `orders` and `invoices`), prefer separate modules over a large shared
package.

## 2. Scaffold skeleton

`make gen module=billing` produces this composition (excerpts; the
generated files carry full comments). Health is the skeleton's only use case:
it gives sqlc a query to compile (sqlc fails on an empty `queries/`) and is a
real, permission-protected database probe for the module.

`build.go` uses the one constructor shape every module shares, so a module can
grow settings, dependencies, and fallible setup without changing its signature
or the bootstrap call:

```go
type Settings struct{} // the module's own config section, filled by internal/bootstrap
type Deps struct{}     // internal/contracts values from modules built earlier

func Build(infra app.Infra, settings Settings, deps Deps) (*Module, error) {
	handlers := Handlers{
		Health: billinghandler.NewHealthHandler(billingservice.NewHealthService(billingrepository.NewHealthRepository(infra.Pool))),
		// gen:build
	}
	return New(handlers), nil
}
```

| Every module, without exception, is `Build(infra app.Infra, settings Settings, deps Deps) (*Module, error)`. `Settings` is its own config section; `Deps` holds the contracts it consumes from modules built earlier. A module exposes the contracts it provides through methods on `*Module` (`identityModule.Principals()`, `apikeysModule.Verifier()`), never through a second return value. |

`module.go` keeps one `Handlers` struct and the route table; generated slices
add a field and a `register<Slice>Routes` call at the markers:

```go
type Handlers struct {
	Health   *billinghandler.HealthHandler
	Invoices *billinghandler.InvoiceHandler // added by gen resource
	// gen:handlers
}

func (m *Module) RegisterRoutes(api *httpapi.API, deps app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/billing/health", OperationID: "billingHealth",
		Tags: []string{"billing"}, Summary: "Billing module health",
	}, httpapi.Policy{Permission: billingcatalog.PermissionRead, RateLimit: httpapi.RateLimitRelaxed}, m.handlers.Health.Health)
	m.registerInvoiceRoutes(api) // added by gen resource
	// gen:routes
}
```

`service/ports.go` declares what the use cases need; `repository/` implements
it and asserts the match at compile time
(`var _ billingservice.InvoiceRepository = (*InvoiceRepository)(nil)`):

```go
type InvoiceRepository interface {
	CreateInvoice(ctx context.Context, in InvoiceInput) (Invoice, error)
	GetInvoice(ctx context.Context, id uuid.UUID) (Invoice, error)
	ListInvoices(ctx context.Context, cursor pagination.DBCursor) ([]Invoice, error)
	PatchInvoice(ctx context.Context, id uuid.UUID, in InvoicePatch, expected *time.Time) (Invoice, error)
	DeleteInvoice(ctx context.Context, id uuid.UUID) error
}
```

### Generated resource slice

`make gen resource=invoice module=billing`:

| Layer | Generated behavior |
| --- | --- |
| Migration | `billing_invoices`: `UUID PRIMARY KEY DEFAULT uuidv7()`, `tenant_id UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT`, `name VARCHAR(255)` with a non-blank `CHECK`, `created_at`/`updated_at` (`update_updated_at()` trigger), index `(tenant_id, created_at DESC, id DESC)`, `ENABLE`/`FORCE ROW LEVEL SECURITY` and the reviewed `tenant_isolation` policy calling `app_tenant_visible(tenant_id)`; the down file drops the table |
| Queries | `Create`/`Get`/`List`/`Patch`/`Delete` with a `tenant_id` predicate each, plus a bounded `DeleteTenant<Resource>Batch` purge query; `List` uses `(created_at, id) < (@cursor_created_at, @cursor_id)`; `Patch` uses `CASE WHEN @set_<field>` so an omitted field is never clobbered |
| Repository | takes `infra.DB` (the RLS-scoped handle, never `infra.Pool`), binds `tenant.Require(ctx)` and returns its error when the context has no tenant, maps `pgx.ErrNoRows`/zero deleted rows to `apperror.ErrNotFound`, maps sqlc rows to `service.Invoice`; `<r>_purge.go` implements the tenancy `Purger` in `PurgeBatchSize` pages |
| Service | `authz.Authorize` first (fail closed), input normalization (422), a pointer-field `InvoicePatch` rejects an empty patch, a row from another tenant is `404 billing.invoice_not_found`, a row escaping the tenant filter in a list fails loudly; imports no sqlc, pgx, Fiber, or `dto` |
| Handler/DTO | `httpx.BindAndValidate`, `httpx.ParamID`, `httpx.CursorQuery`, `pkg/pagination.List[T]`, `Location` on create, httpapi operation metadata; `PATCH` takes `InvoicePatchRequest` pointer fields |
| Routes | `rl.Relaxed → authzhttp.Require(read)` for reads; `rl.Normal → authzhttp.Require(write/delete) → deps.Idempotency → audithttp.Guard(ModeRequired)` for writes; update is `PATCH /invoices/:id` with partial semantics |
| Catalog | `billing:invoices:{read,write,delete}` (owner/admin-only), `billing.invoice_not_found` with en/vi titles |
| Tenancy | `Build` appends `repository.NewInvoiceTenantPurger(infra.DB)`; the module's `tenant_purge.go` exposes it through `TenantPurgers` (`app.TenantPurgeProvider`), and bootstrap registers it at the `gen:module-purge` marker so a deleted tenant's rows are erased with no manual wiring |
| Tests | service test over a fake repository (CRUD, validation, tenant isolation, pagination, fail-closed), handler test over a fake service, `invoice_integration_test.go` (`-tags=integration`) running the routes through `app.New` on PostgreSQL with `testutil.Scoped`/`NewTxManager` (the RLS app role, not the BYPASS fixture pool): CRUD, empty patch 422, cross-tenant id → 404, three succeeded audit rows, and a tenant purge that leaves another tenant's rows intact |

Commands and queries (`make gen command=IssueInvoice module=billing`) get
`IssueInvoiceInput`/`IssueInvoiceResult` and an `IssueInvoiceHandler` in
`service/command_issue_invoice.go` that authorizes (`billing:write`; queries
use `billing:read`) and returns `501 billing.not_implemented` until written.
The HTTP adapter (`handler/`, `dto/`, `routes_command_issue_invoice.go`) is a
`POST` for a command (idempotency + required audit guard) and a `GET` for a
query; `no_http=1` skips it.

The generated `README.md` records ownership and lists every generated slice;
keep it current, it is the first file the next team reads.

### Module-owned configuration

`app.Infra` carries infrastructure plus only the shared configuration in
`Infra.Platform` (`App`, `JWT`, `Tenant`). A module that needs its own
`config` section adds fields to `Settings`; `bootstrap.Build(cfg, infra)`
fills it, so no module can read another module's settings (identity, files,
and webhooks do this).

## 3. Route wiring

`app.Register` mounts every `app.RouteProvider` operation. A module never
mounts global middleware or another module's operations. Register through the
single helper; it builds the one correct chain:

```mermaid
flowchart LR
    A["rate limit tier"] --> B["authenticate<br/>(API key or JWT / JWT-only)"]
    B --> C["authzhttp.Require / RequireAny"]
    C --> D["RequireMFA<br/>(automatic for platform:*)"]
    D --> E["Policy.Feature<br/>(before locks and audit)"]
    E --> F["deps.Idempotency<br/>(safe mutations only)"]
    F --> G["audithttp.Guard<br/>(Tier 2 privileged)"]
    G --> H[handler]
```

| Rule | Detail |
| --- | --- |
| One registration helper | `httpapi.Register(api, httpapi.Op{...}, httpapi.Policy{...}, handler)`. It attaches policy metadata, adds the OpenAPI security requirement, and wires the chain. Never register a Fiber route directly. |
| Auth chain | `Authenticate` accepts `Authorization: ApiKey` (verified by `deps.APIKeyVerifier`) or JWT + session validation; its API-key scopes become the subject's permissions, so the same authz check gates both. `JWTOnly: true` selects the JWT-only chain (sessions, MFA factors, invitation accept, erasure). |
| Rate-limit tier | `Strict` for credential/token endpoints, `Normal` for mutations and uploads, `Relaxed` for reads. Public routes still get a limit. |
| Permissions | Every non-public operation names `Permission` or `AnyOf`. Public is an explicit reviewed allowlist entry (currently the auth flows, the HMAC webhook receiver, and the probes). |
| Idempotency | Only on mutating operations safe to replay. Never on login/refresh/logout/password-reset/email-verification/OAuth or routes returning a plaintext secret/token (API-key create/rotate, webhook endpoint/source create and `rotate-secret`) — the replay cache would store it for 24h. |
| Audit | Privileged mutations are `Tier1Action` (service `audit.TxRecorder.RecordTx` in the transaction) or `Guard` with `ModeRequired` after idempotency. Only the caller's own notification state may use a reviewed best-effort Guard. |
| Order | The helper owns the order; `test/arch` rejects a mutation with neither audit form, a public route without an allowlist entry, a platform permission without MFA, and a feature gate after idempotency or the Guard. |
| Feature gate | `Policy.Feature` is the only handler-free gate; it runs after authorization and before idempotency/audit. Do not add a `features.Require` call in a handler for a route-level flag. |
| ETag/CAS | Config resources return `ETag`. The conditional UPDATE itself carries the version predicate (`WHERE ... AND updated_at = $n`); 0 rows is 412 when the row exists, 404 when it does not. |
| Responses | Return the resource directly (`httpapi.Empty` for no body), `pagination.List[T]` for lists, and `*apperror.AppError` for failures. `201`/`202` set `Location`; token/secret/export routes set `NoStore`. |
| Paths | `/api/v1/<plural-kebab-collection>`, caller-owned under `/me`, custom methods only as `POST /resource/{id}/verb`. |

Reference route table:

```go
func (m *Module) RegisterRoutes(api *httpapi.API, deps app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/widgets", OperationID: "listWidgets",
		Tags: []string{"widgets"}, Summary: "List widgets",
	}, httpapi.Policy{
		Permission: widgetscatalog.PermissionRead, RateLimit: httpapi.RateLimitRelaxed,
	}, m.handler.List)
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/widgets", OperationID: "createWidget",
		Tags: []string{"widgets"}, Summary: "Create a widget",
	}, httpapi.Policy{
		Permission: widgetscatalog.PermissionWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{Action: "widget.created", Resource: "widget"}},
	}, m.handler.Create)
}
```

For caller-owned routes use `/me/...` and the profile/session permissions; for
an admin sub-surface use `/members/{memberId}/...` or `/roles`, never `/admin`.

### Legacy exceptions (do not copy)

| Exception | Today | Target |
| --- | --- | --- |
| Public routes | The `publicOperations` allowlist in `test/arch/routes_test.go` (anonymous/token-possession auth flows, the HMAC webhook receiver, and the probes) is the complete list of operations with `Public: true`. Every other operation, including all `/files` operations and `DELETE /me/sessions` (`sessions:revoke`), names a permission. | Adding to the list is a reviewed product decision. |
| Best-effort audit | Only the caller's own notification read/preference state may use `Guard` in `ModeBestEffort`; `test/arch` requires an allowlist entry with a reason. Every privileged mutation is Tier 1 or required-mode Guarded. | Never extend the list with privileged mutations. |
| `identity` user reads/updates | The global profile is `GET`/`PATCH /me/profile` (owner only). Another member is read/managed through `GET`/`PATCH /members/{memberId}` and requires the matching `admin:users:*` permission plus the role hierarchy. | Keep the route permission; keep row-level ownership in the service. |
| Membership references from other modules | `files`, `notifications`, `notification_preferences`, `api_keys`, and `feature_flag_targets` keep `(tenant_id, user_id) REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT` (users are global; the membership is the tenant-scoped principal). Memberships are never hard-deleted (removal is a status), so the key only guarantees the user is or was a member of that tenant and never cascades across the module boundary. | No new cross-module foreign keys; core identity references need explicit owner approval. |

## 4. Permissions and catalog

A module owns its permissions in `catalog/permissions.go` and returns them from
`Permissions() []authz.Grant`. One permission is one stable capability named
`<resource>:<verb>` (lower-case segments joined by `:`, validated at
registration). The resource is usually the module name (`files:read`,
`webhooks:write`) but may be narrower (`profile:write`, `sessions:revoke`);
back-office capabilities use an `admin:` prefix (`admin:features:write`,
`admin:api_keys:rotate`).

```go
const (
	PermissionRead   authz.Permission = "widgets:read"
	PermissionWrite  authz.Permission = "widgets:write"
	PermissionDelete authz.Permission = "widgets:delete"
)

// Admins receive every permission implicitly; grant other roles deliberately.
func Permissions() []authz.Grant {
	return []authz.Grant{
		{Permission: PermissionRead, Roles: []string{authz.RoleMember}},
		{Permission: PermissionWrite},  // owner/admin-only
		{Permission: PermissionDelete}, // owner/admin-only
	}
}
```

`app.Catalogs.Register` adds every module's grants at startup, owned by
`Module.Name()`. Admins receive every registered permission by construction.
Startup fails on an invalid name, an unknown role, or a permission another
module already owns, so two modules never silently share one. Do not authorize
by role string in a handler; unknown roles have no permissions.

**Shared permissions stay central.** A permission checked by more than one
module cannot be owned by one of them without a cross-module import. Today that
is only the back-office set in `internal/platform/authz` (`admin:users:*`,
`admin:files:*`, `admin:stats:read`): the admin module's routes require them
and the identity/files services re-check them. Prefer designs where the module
that registers the route owns the permission.

Permission checklist:

1. Add constants and `Grant`s in `catalog/permissions.go`.
2. Grant `user` only when end users need the capability.
3. Protect every route with `authzhttp.Require` (or `RequireAny` for a documented
   union); `test/arch` fails on an unprotected route.
4. Add a role-matrix assertion in `module_test.go` if grants are non-trivial.
5. Mention the permission names in the module README.

Other catalog entries follow the same pattern:

| Entry | File | Provider | Rule |
| --- | --- | --- | --- |
| Error codes | `catalog/errors.go` | `app.ErrorProvider` | add `errors.<code>.title` to both `en.json` and `vi.json`; `i18n.New` fails on missing or mismatched keys |
| Locales | `catalog/locales/{en,vi}.json` | `app.LocaleProvider` | same keys and `{placeholders}` in both |
| Feature flags | `catalog/flags.go` | `app.FlagProvider` | see [Feature flags](feature-flags.md) |

Handler and service unit tests that do not build the app call
`<name>catalog.Register()` in an `init` of a `_test.go` file; an unregistered
code renders as 500 `system.internal` and logs an error.

## 5. Audit

| Tier | Mechanism | On audit write failure |
| --- | --- | --- |
| Tier 1 (privileged, in-transaction) | service calls `audit.TxRecorder.RecordTx` in the mutation transaction | transaction rolls back; `503 system.audit_unavailable` |
| Tier 2 (privileged route) | `audithttp.Guard` after idempotency (the zero `audithttp.Policy` mode is `ModeRequired`; best-effort must be set explicitly with `ModeBestEffort`, and any other value is treated as required) writes a started intent row, then completes it as succeeded/failed/denied | request fails with `system.audit_unavailable` |
| Fallback | global `audithttp.Middleware` | logged and counted in `audit_write_failures_total`; request succeeds |

The fallback middleware records a successful (2xx) POST/PUT/PATCH/DELETE with
actor, route template, request ID, IP, user agent, and status. It skips routes
marked guarded, requests a Tier 1 service already recorded, and idempotent
replays. Its `Action` is `<METHOD> <route template>`, for example
`POST /api/v1/widgets/`. `status` and `path` are reserved fallback metadata
keys: the middleware overwrites any value a handler put there.

Tier 1 services build the event with `audit.NewServiceEvent(ctx, action,
resource)`, set `ResourceID` and non-sensitive `Metadata`, and pass it to
`RecordTx`. Explicit domain actions use `<module>.<resource>.<verb>`, such as
`widgets.widget.published`. A best-effort explicit write outside a transaction
must count and log its own failure, because `Recorder.Record` only persists the
event:

```go
event := audit.Event{
	Action:     "widgets.widget.published",
	Resource:   "widget",
	ResourceID: strconv.FormatInt(widget.ID, 10),
	Metadata:   map[string]any{"ownerId": ownerID},
}
ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
defer cancel()
if err := recorder.Record(ctx, event); err != nil {
	metrics.AuditWriteFailuresTotal.Inc()
	slog.Warn("audit write failed", "action", event.Action, "error", err)
}
```

`recorder` is the `audit.Recorder` injected into the service (`audit.Noop{}`
in tests).

Rules:

- Metadata must be JSON-marshalable and never contain passwords, tokens, email
  bodies, reset links, full request bodies, or PII. Record identifiers and
  outcomes.
- Never silently drop an audit failure.
- `audit_events` is append-only: database triggers reject `DELETE`, `TRUNCATE`,
  and every update except completing a started intent. Do not add update/delete
  paths.

## 6. Idempotency

`deps.Idempotency` is opt-in per route. With an `Idempotency-Key` header:

| Situation | Result |
| --- | --- |
| First request, 2xx | status, body, `Content-Type`, `Location`, `ETag` cached 24h |
| Retry with same key (and body) | replayed with `Idempotent-Replayed: true` |
| Same key, different non-multipart body (≤ 1 MiB, SHA-256 bound) | `409 Conflict` |
| Duplicate while the first is still running (2-minute owner lock) | `409 Conflict` |
| Cache read/write failure | fail open: the handler runs normally |

Apply it to create/update/delete/restore-style routes. Do not apply it to
token-minting or credential-verification endpoints, GET/HEAD/OPTIONS, or routes
whose side effect is not safe to repeat. Multipart bodies are not bound: reuse a
key only for the same file operation.

Use Redis (`CACHE_DRIVER=redis`) in any multi-replica deployment; the memory
cache is per process, so a retry landing on another replica executes again. See
[Operations](operations.md#6-idempotency-rate-limits-and-redis) for TTLs, outage behavior,
and cleanup.

## 7. Tables, queries, and migrations

### Ownership

One module owns each table:

| Owner | Tables |
| --- | --- |
| `identity` | `tenants`, `tenant_settings`, `users`, `user_passwords`, `user_identities`, `memberships`, `data_exports`, `invitations`, `sessions`, `refresh_tokens`, `password_reset_tokens`, `email_verification_tokens` |
| `files` | `files` |
| `features` | `feature_flags`, `feature_flag_targets` |
| `notifications` | `notifications`, `notification_preferences` |
| `webhooks` | `webhook_*` |
| `apikeys` | `api_keys` |
| platform (`jobs`, `audit`, `scheduler`) | `jobs`, `audit_events`, `scheduler_leases`, `scheduler_runs` |

New modules name tables after the module or with a `<name>_` prefix (`widgets`,
`widgets_events`); `test/arch/table_ownership_test.go` derives ownership from
that convention, so only differently named tables need a `tableOwners` entry.
`TestCodeSQLOnlyTouchesOwnedTables` checks two SQL sources and fails on a
reference to another owner's table, a table with no owner, or a CTE named like
another owner's table (a CTE must not shadow it):

| Source | Owner |
| --- | --- |
| `internal/modules/*/queries/*.sql` | the module |
| Inline SQL string literals in non-test Go under `internal/modules/**`, `internal/platform/**`, `pkg/**`, and `cmd/**` (`sqlc/` and `testdata/` skipped) | the module, `platform/<pkg>`, `pkg/<pkg>`, or `cmd/<command>` |

Inline SQL is detected by UPPERCASE keywords (`FROM`, `JOIN`, `INSERT INTO`,
`DELETE FROM`, `UPDATE ... SET`, ...) followed by an identifier, so write
inline SQL keywords in uppercase or the scan misses it. Platform tables
(`jobs`, `audit_events`, `scheduler_*`) belong to their `platform/<pkg>`.
`information_schema` and `pg_*` catalog references are metadata, not
application tables. Reviewed exceptions go in `crossModuleReads`:
`platform/features` reads `feature_flags`/`feature_flag_targets` for its
snapshot, and `cmd/migrate` reads `tenants` while verifying the baseline;
modules have none (tenants/users are read through
`internal/contracts/identity`). Never read or write another module's table,
even in a JOIN; use a contract/read model or a job.

`TestSQLCallsUseConstantStrings` (`test/arch/sql_literals_test.go`) type-checks
`internal/`, `pkg/`, and `cmd/` and rejects any `Exec`/`Query`/`QueryRow`
(DBTX-shaped) or `pgx.Batch.Queue` whose SQL argument is not a compile-time
constant. The only reviewed dynamic sites are listed in `dynamicSQLAllowlist`
with a reason; the list may not grow without raising its reviewed ceiling, and
an entry that no longer matches a call fails the test so it is removed.

### Schema conventions

| Topic | Convention |
| --- | --- |
| Primary key | `UUID DEFAULT uuidv7()` (PostgreSQL 18 native; Go code that needs the id before insert calls `uuid.NewV7()`) |
| Tenant | tenant-owned tables: `tenant_id UUID NOT NULL REFERENCES tenants(id)` with no default, so every row states its tenant; lead query indexes with `tenant_id` |
| Timestamps | `created_at`/`updated_at` `TIMESTAMP WITH TIME ZONE NOT NULL`; shared `update_updated_at()` trigger for mutable tables |
| Soft delete | `deleted_at` plus partial unique indexes for active rows |
| Pagination | `(created_at DESC, id DESC)` index |
| Foreign keys | fine inside the module; avoid cross-module ones. An unavoidable core reference (`user_id`) needs owner agreement, documentation, `(tenant_id, user_id)` → `memberships (tenant_id, user_id)`, and `ON DELETE RESTRICT`, never `CASCADE` |
| Indexes | every ownership and lookup path used by list/detail endpoints, in the same migration as the table |

### Migration rules

```mermaid
flowchart TD
    A{Has any database<br/>been deployed from<br/>this tree or fork?}
    A -- "no (upstream boilerplate)" --> B["extend or add the owner's section in<br/>migrations/20260101000000_platform_init_schema.{up,down}.sql"]
    A -- yes --> C["make migrate-create module=widgets name=init<br/>→ &lt;UTC YYYYMMDDHHMMSS&gt;_widgets_init.{up,down}.sql"]
    C --> D{Timestamp older than the<br/>newest migration on main?}
    D -- yes --> E[regenerate before merging]
    D -- no --> F["cmd/migrate applies it<br/>(only DDL path)"]
    E --> F
    B --> F
```

- Only `cmd/migrate` applies DDL; the API and seed commands assume the schema
  exists.
- The fresh-install schema is one init migration (listed in
  [Data Model](data-model.md)) with one `-- owner: <module>` section per owner:
  shared functions, identity, platform tables, then modules in FK order. A
  new module's section goes after every table it references.
- Never edit a migration once a database has applied it, and never reuse or
  renumber a shipped one. Timestamp versions do not collide across branches;
  golang-migrate only applies versions above the recorded one.
- `test/arch/migrations_test.go` fails on duplicate versions (including two
  zero-padded spellings of one number), a missing up or down file, any
  version that is not a 14-digit UTC timestamp, a name that does not start
  with a module (or `platform`) owner, and a file or `-- owner:` section
  that creates another owner's table.
- The down migration reverses only what its up created. It is for local/test
  reset and reviewed emergency use, not the production rollback plan.
- Keep migrations short. Backfills, reindexes, and large rewrites are jobs, not
  DDL transactions. Use expand/contract for incompatible column changes and
  make the API tolerate both shapes during rollout.
- On a live table use `CREATE INDEX CONCURRENTLY` when the runner permits it;
  otherwise schedule a maintenance window and document the lock.
- Test on a copy of a production-shaped database and take a verified
  backup/PITR snapshot first. A migration is not a backup.

Example table:

```sql
CREATE TABLE IF NOT EXISTS widgets (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    -- identity user ID; intentionally no cross-module foreign key
    owner_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_widgets_tenant_owner_created
    ON widgets (tenant_id, owner_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trigger_widgets_updated_at ON widgets;
CREATE TRIGGER trigger_widgets_updated_at
    BEFORE UPDATE ON widgets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();
```

### Queries and sqlc

Put SQL in `internal/modules/<name>/queries/*.sql`. Each module owns its sqlc
configuration in `internal/modules/<name>/sqlc.yaml` (there is no root
`sqlc.yaml`); paths are relative to that file, and `generate.go` carries
`//go:generate sqlc generate -f sqlc.yaml`:

```yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "queries/"
    schema: ["../../../migrations/*.up.sql"]
    gen:
      go:
        package: "sqlc"
        out: "sqlc"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_empty_slices: true
        omit_unused_structs: true
```

Then run `make sqlc-generate` (every `internal/modules/*/sqlc.yaml`) or
`go generate ./internal/modules/<name>/`. CI regenerates every module config
and fails on drift, and also fails when a module has `queries/` but no
`sqlc.yaml`. Generated code lands only in the module's own
`sqlc/` package; prefix query names (`ListWidgetsByOwner`, `CreateWidget`) and
never import another module's generated types (the architecture test enforces
it). sqlc is not a substitute for a migration: the schema must be applied
first.

## 8. Cross-module contracts

A module never imports another module's package. When module B needs data or
behavior from module A, the provider exposes a stdlib-only interface under
`internal/contracts/<provider>/` (today `identity`, `files`, `notifications`,
`webhooks`) and the composition root wires it:

```mermaid
flowchart LR
    subgraph Root["composition root (bootstrap module list)"]
        direction LR
        PB["provider.Build(infra, Settings, Deps)<br/>→ *Module"] -- "Module.X()" --> CB["consumer.Build(infra, Settings, Deps{X})"]
    end
    P["provider module<br/>implements interface"] -. structural .-> I["internal/contracts/provider<br/>interface + value types"]
    C["consumer module<br/>depends on interface"] --> I
    PB --- P
    CB --- C
```

Real examples of provided contracts consumed downstream: identity's
`Principals()` (API keys), `Recipients()` (notifications), `Admin()` (admin),
and `TenantActive()` (webhooks); files' `Admin()` (admin).

```go
package identity // internal/contracts/identity

// User is the contract view of an identity user. No transport or SQL types.
type User struct {
	ID    uuid.UUID
	Email string
	Role  string
}

// UserReader is implemented by the identity module or a small adapter.
// Keep methods additive and version a breaking change.
type UserReader interface {
	GetUser(ctx context.Context, id uuid.UUID) (User, error)
}
```

Rules:

- Contracts are stdlib-only: no `internal/modules/*`, Fiber, sqlc,
  repositories, DTOs, storage, database, or session packages. Consumers import
  them with an alias such as `identitycontracts`.
- The provider implements the interface structurally and returns it from
  `Build` through `Deps`, and expose what it provides through methods on its `*Module`; only the composition root knows both sides.
- Expose identifiers and stable values, not repositories, rows, or HTTP DTOs.
- Errors are part of the contract. Map them to `apperror` at the transport
  boundary; never leak SQL errors or provider internals.
- Adding a method breaks implementers; prefer a new, narrowly named contract.
- For cross-module side effects, enqueue a durable job (the `jobs` table is
  the outbox; `Queue.EnqueueTx` commits with the business row) instead of
  calling another module inside a transaction.
- If a contract would be awkward for a read model, own a local read-only table
  populated by a job, or a narrow query owned by the data owner.

## 9. What must not be imported

| From | Forbidden |
| --- | --- |
| any `internal/modules/<a>` | `internal/modules/<b>` (any package) |
| `internal/platform/*`, `pkg/*` | any `internal/modules/*` |
| a module handler | its own repository, another module, Fiber global setup |
| a module service | Fiber/fasthttp (even transitively), sqlc, pgx, its own `dto`/`handler`, another module |
| a module repository | Fiber/fasthttp (even transitively), another module's `sqlc` |
| `internal/contracts/*` | any module implementation, repository, DTO, sqlc, storage, database, session, or Fiber |
| `cmd/api`, `internal/bootstrap` | module internals other than `Build`/`Deps` and the exported contract accessors |
| anything but `cmd/*`/`test/*` | `internal/bootstrap` |

`test/arch/imports_test.go` fails the build on cross-module and
platform/pkg-to-module imports; `framework_free_test.go` and
`layering_test.go` enforce the service/repository rows. Run `make arch` after adding a module. Do not
weaken the test to make a shortcut compile; introduce a contract instead.

## 10. Tests

| Level | What to cover |
| --- | --- |
| `module_test.go` | `Name()` is stable, the module satisfies `app.Module`, catalog registers and localizes |
| Handler | Direct call (or a huma app from `app.New`) with a fake service: status, `Content-Type`, problem details, ETag/If-Match behavior |
| Service | fake the narrow repository interface: ownership checks, validation, error mapping, job enqueue paths |
| Repository integration | `-tags=integration` + `internal/testutil.SetupTestDB` (Testcontainers or `TEST_DATABASE_URL`): pagination, soft delete/restore, uniqueness, transaction rollback |
| Architecture | `make arch` |

A handler test should not need PostgreSQL:

```go
created, err := handler.Create(context.Background(), &CreateWidgetInput{Body: widgetsdto.WidgetRequest{Name: "demo"}})
require.NoError(t, err)
assert.Equal(t, "demo", created.Body.Name)
```

For full-stack route tests, construct `app.New` with a test config and register
the module, as `internal/app/app_test.go` does. Use unique names/IDs per test
and never depend on a shared production database.

Run before opening a PR:

```bash
go fmt ./internal/modules/<name>/...
make arch
make check
go test -race ./...
make openapi   # if route annotations changed
```

## 11. Register in the composition root

`internal/bootstrap/modules.go` holds the single ordered module list.
`make gen module=<name>` inserts the module above the `// gen:module-build`
and `// gen:module-list` markers (exactly once; a second run is a no-op). Pass
only contracts from modules built earlier:

```go
billingModule, err := billing.Build(infra, billing.Settings{}, billing.Deps{}) // inserted by cmd/gen
if err != nil {
	return nil, err
}
// a module that consumes a contract passes it in Deps:
// ordersModule, err := orders.Build(infra, orders.Settings{}, orders.Deps{Principals: identityModule.Principals()})
// gen:module-build

return &Modules{
	List: []app.Module{
		identityModule,
		// ...
		billingModule,
		// gen:module-list
	},
	// ...
}, nil
```

`cmd/api` startup order (`test/arch` builds the same list, so route,
permission, audit, overlap, and SQL ownership checks cover the new module with
no further edits):

```mermaid
flowchart LR
    A[config] --> B[database pool] --> C[storage] --> D[cache] --> E[email]
    E --> F["app.Infra<br/>pool · tx · cache · audit ·<br/>queue · queued email · storage · flags"]
    F --> G["bootstrap.Build(cfg, infra)"] --> H[app.Catalogs.Register] --> I["validate FEATURE_FLAGS<br/>against catalog"]
    I --> J["i18n.New(LocaleSources)"] --> K[Modules.Startup] --> L["app.New +<br/>Register(List...)"] --> M[worker + Listen]
```

Migrations are not in this chain: they run beforehand as the separate
`cmd/migrate` command.

Rules:

- `Build` must not perform I/O; startup checks that need the database go in a
  method exposed as `Modules.Startup` (see identity).
- Keep module-specific dependencies and settings in `Build` (a `Settings`
  struct for configuration); do not grow `app.Infra`, `Infra.Platform`, or
  `app.Deps` for one module.
- Register before `Listen`. Duplicate paths are a programming error; a catalog
  conflict fails `app.Catalogs.Register` and is a startup error.
- Durable work: implement `RegisterJobs(jobs.Registrar)` as webhooks does for
  `webhook.delivery` (and `RegisterEvents(*events.Bus)` for durable event
  subscriptions, as webhooks does for `events.*.webhooks`). The registrar runs every handler
  (and every durable event handler) under `authz.SystemContext`, so service
  calls without a subject pass `authz.Authorize`; outside a job, a subject-less
  service call is `403`. Wrap an undecodable or invalid payload error with
  `jobs.Permanent` so it dead-letters instead of retrying. Job kinds use a
  `<module>.<action>` prefix; slow kinds can move to a dedicated worker with
  `jobs.WithKinds`/`WithExcludeKinds`.
- Do not enqueue work during `RegisterRoutes`; a request or an explicit startup
  backfill job must be the trigger.

## 12. Add-a-module checklist

1. `make gen module=<name>` (registers it in the bootstrap list); keep the
   generated README and fill in ownership.
2. `make gen resource=<r> module=<name>` per CRUD resource and
   `make gen command=<Name>` / `query=<Name>` per use case; review the
   generated migration columns, then add real fields to the queries, service
   types, repository mapping, and DTOs.
3. Adjust grants, error codes, flags, and en/vi strings in `catalog/`
   (generated permissions are admin-only and the vi titles need review).
4. Other schema changes: `make migrate-create module=<name> name=<purpose>`,
   then `make sqlc-generate`.
5. Keep transport, rules, and SQL in their packages; hand-written routes
   follow the same chain (authenticate, rate limit, permission, idempotency
   where appropriate, audit guard for privileged mutations, then handler).
6. Implement `RegisterJobs`/`RegisterEvents`/`RegisterSchedules` for durable
   work.
7. Add unit, handler, and integration tests; run `make arch`.
8. Update the module README, [Architecture](architecture.md) if the module map
   changes, and the operation metadata (`Op`/`Policy`) if routes were added.
9. Run `gofmt`, `make check`, `go test -race ./...`, and `make openapi`.
10. Confirm no probe module, generated binary, or secret is staged. One module
    per commit is preferred.

## 13. Conventions for many modules

| Concern | Convention |
| --- | --- |
| Names | lower_snake_case directories; `<name>_` tables; `<resource>:<verb>` permissions (`admin:<resource>:<verb>` for back-office); `<name>.<action>` job kinds; audit actions `[admin.]<resource>[.<sub-resource>].<past-tense verb>` (`file.uploaded`, `webhook.endpoint.secret_rotated`, `admin.feature_flag.updated`, `api_key.rotated`, `notification.sent`) |
| One reason to change | no shared "common"/"utils" domain packages; mechanics belong in `platform`/`pkg`, shared business concepts in contracts |
| Small interfaces | define repository and contract interfaces at the consumer, keep them narrow, fake them in tests |
| No cross-module joins | a read model or contract is cheaper than an ownership dispute at 100 modules |
| Durable side effects | enqueue with/after the owning transaction; never `async.Go` for work that must survive a restart |
| Observability | one permission namespace, one job-kind prefix, one bounded route table per module |
| Documentation | the module README is part of the module: tables, routes, permissions, jobs, contracts, operational caveats |
