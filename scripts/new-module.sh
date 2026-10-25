#!/usr/bin/env bash
#
# Scaffold a new business module under internal/modules/<name>.
#
# Usage:
#   scripts/new-module.sh <name>
#   scripts/new-module.sh --dry-run <name>
#   scripts/new-module.sh <name> --dry-run
#
# <name> must be lower_snake_case. The scaffold compiles, passes its own
# tests, and self-registers its permissions, error codes, and i18n strings
# through the app provider hooks. The only manual wiring step is adding the
# module to internal/bootstrap/modules.go.

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/new-module.sh [--dry-run] <name>

Create internal/modules/<name>/ with module.go (Build constructor + catalog
providers), module_test.go, catalog/ (permissions.go, errors.go,
locales/en.json, locales/vi.json), handler/, service/, repository/, dto/,
queries/<name>.sql, and README.md, and append the module's block to sqlc.yaml.

Options:
  --dry-run    Print the paths that would be created; write nothing.
  -h, --help   Show this help.

<name> must match [a-z][a-z0-9]*(_[a-z0-9]+)* and must not be a Go keyword,
a reserved package name, or a Go-reserved directory (vendor/testdata).
Existing modules are never overwritten.
EOF
}

dry_run=0
name=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run)
      dry_run=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      if [ "$#" -eq 0 ]; then
        break
      fi
      if [ "$#" -gt 1 ] || [ -n "$name" ]; then
        echo "error: expected exactly one module name" >&2
        usage >&2
        exit 2
      fi
      name="$1"
      shift
      break
      ;;
    -*)
      echo "error: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      if [ -n "$name" ]; then
        echo "error: expected exactly one module name" >&2
        usage >&2
        exit 2
      fi
      name="$1"
      ;;
  esac
  shift
done

if [ -z "$name" ]; then
  usage >&2
  exit 2
fi

# Reject anything that is not lower_snake_case. This also rejects path
# separators, dots, whitespace, leading digits, and traversal sequences.
if [[ ! "$name" =~ ^[a-z][a-z0-9]*(_[a-z0-9]+)*$ ]]; then
  echo "error: module name must be lower_snake_case (for example billing_probe)" >&2
  exit 2
fi

# Reserved names either collide with imports/top-level packages or produce a
# package that cannot compile as a module root.
case "$name" in
  break|case|chan|const|continue|default|defer|else|fallthrough|for|func|go|goto|if|import|interface|map|package|range|return|select|struct|switch|type|var)
    echo "error: '$name' is a Go keyword" >&2
    exit 2
    ;;
  vendor|testdata)
    echo "error: '$name' is a Go-reserved directory and would be ignored or mis-handled by the go tool" >&2
    exit 2
    ;;
  app|main|internal|modules|platform|pkg|handler|service|repository|dto|sqlc|config|testutil|middleware|authz|audit|jobs|session|bootstrap|contracts|catalog|queries)
    echo "error: '$name' is reserved by the repository layout" >&2
    exit 2
    ;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)

if [ ! -f "$repo_root/go.mod" ]; then
  echo "error: go.mod not found at $repo_root" >&2
  exit 1
fi

module_path=$(awk '$1 == "module" { print $2; exit }' "$repo_root/go.mod")
case "$module_path" in
  "")
    echo "error: could not read the module path from $repo_root/go.mod" >&2
    exit 1
    ;;
  *[[:space:]]*)
    echo "error: invalid module path in go.mod: $module_path" >&2
    exit 1
    ;;
esac

target_rel="internal/modules/$name"
target="$repo_root/$target_rel"
sqlc_file="$repo_root/sqlc.yaml"
# URL segments and doc paths use dashes; Go identifiers and codes keep '_'.
route_name=${name//_/-}

if [ -e "$target" ]; then
  echo "error: module already exists: $target_rel" >&2
  echo "Refusing to overwrite it." >&2
  exit 1
fi
if [ -f "$sqlc_file" ] && grep -q "internal/modules/$name/queries/" "$sqlc_file"; then
  echo "error: sqlc.yaml already has a block for internal/modules/$name" >&2
  exit 1
fi

files=(
  "$target_rel/module.go"
  "$target_rel/module_test.go"
  "$target_rel/catalog/catalog.go"
  "$target_rel/catalog/permissions.go"
  "$target_rel/catalog/errors.go"
  "$target_rel/catalog/locales/en.json"
  "$target_rel/catalog/locales/vi.json"
  "$target_rel/handler/handler.go"
  "$target_rel/service/service.go"
  "$target_rel/repository/repository.go"
  "$target_rel/dto/dto.go"
  "$target_rel/queries/$name.sql"
  "$target_rel/README.md"
)

if [ "$dry_run" -eq 1 ]; then
  echo "Dry run for module '$name' (module path: $module_path)"
  for rel in "${files[@]}"; do
    echo "  would create $rel"
  done
  echo "  would append the internal/modules/$name block to sqlc.yaml"
  echo "No files written."
  exit 0
fi

mkdir -p "$repo_root/internal/modules"
tmp_dir="$repo_root/internal/modules/.${name}.tmp.$$"
published=0
sqlc_backup=""

cleanup() {
  if [ -n "${tmp_dir:-}" ] && [ -d "$tmp_dir" ]; then
    rm -rf "$tmp_dir"
  fi
  # If publication succeeded but the compile/test check fails, do not leave
  # a broken module (or its sqlc.yaml block) behind.
  if [ "$published" -eq 1 ]; then
    if [ -d "$target" ]; then
      rm -rf "$target"
    fi
    if [ -n "$sqlc_backup" ] && [ -f "$sqlc_backup" ]; then
      mv "$sqlc_backup" "$sqlc_file"
    fi
  fi
  if [ -n "$sqlc_backup" ] && [ -f "$sqlc_backup" ]; then
    rm -f "$sqlc_backup"
  fi
}
trap cleanup EXIT

if [ -e "$tmp_dir" ]; then
  echo "error: temporary path already exists: $tmp_dir" >&2
  exit 1
fi

mkdir -p "$tmp_dir/handler" "$tmp_dir/service" "$tmp_dir/repository" "$tmp_dir/dto" \
  "$tmp_dir/catalog/locales" "$tmp_dir/queries"

# Replace the long token first: __MODULE_PATH__ contains the shorter token as
# a prefix. The escaped copy keeps sed from treating '&' or '|' specially.
escaped_module_path=$(printf '%s' "$module_path" | sed 's/[&|]/\\&/g')

render() {
  sed -e "s|__MODULE_PATH__|$escaped_module_path|g" \
      -e "s|__ROUTE__|$route_name|g" \
      -e "s|__MODULE__|$name|g"
}

cat <<'EOF' | render > "$tmp_dir/module.go"
// Package __MODULE__ owns the __MODULE__ business capability.
//
// It implements app.Module and owns this module's route table. Keep business
// rules in service, persistence in repository, HTTP in handler, and public
// identifiers (permissions, error codes, i18n) in catalog.
package __MODULE__

import (
	"io/fs"

	"github.com/gofiber/fiber/v3"

	"__MODULE_PATH__/internal/app"
	__MODULE__catalog "__MODULE_PATH__/internal/modules/__MODULE__/catalog"
	__MODULE__handler "__MODULE_PATH__/internal/modules/__MODULE__/handler"
	__MODULE__repository "__MODULE_PATH__/internal/modules/__MODULE__/repository"
	__MODULE__service "__MODULE_PATH__/internal/modules/__MODULE__/service"
	"__MODULE_PATH__/internal/platform/authz"
	"__MODULE_PATH__/internal/platform/authz/authzhttp"
	"__MODULE_PATH__/pkg/apperror"
)

// Module wires the __MODULE__ HTTP surface into the application.
type Module struct {
	handler *__MODULE__handler.Handler
}

// New builds the module from its handler (tests). Production uses Build.
func New(handler *__MODULE__handler.Handler) *Module {
	return &Module{handler: handler}
}

// Build constructs the repository, service, and handler from the shared
// infrastructure. It must not perform I/O. Take another module's capability
// only as an internal/contracts interface parameter, wired in
// internal/bootstrap/modules.go.
func Build(infra app.Infra) *Module {
	repo := __MODULE__repository.New(infra.Pool)
	return New(__MODULE__handler.New(__MODULE__service.New(repo)))
}

// Name identifies the module in the registry and owns its catalog entries.
func (m *Module) Name() string { return __MODULE__catalog.Owner }

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return __MODULE__catalog.Permissions() }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) ErrorCodes() map[apperror.Code]apperror.Spec { return __MODULE__catalog.ErrorCodes() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return __MODULE__catalog.Locales() }

// Optional hooks, implement only when needed:
//   FeatureFlags() []features.Definition   (app.FlagProvider)
//   RegisterJobs(jobs.Registrar)           (app.JobProvider)
//   RegisterEvents(*events.Bus)            (app.EventProvider)
//   RegisterSchedules(*scheduler.Registry) (app.ScheduleProvider)
//   RegisterV2Routes(fiber.Router, app.Deps) (app.V2Module)

// RegisterRoutes mounts routes below /api/v1. Every route needs
// authzhttp.Require (test/arch enforces it). Mount group middleware only on a
// prefix this module owns exclusively; see docs/module-template.md.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	group := v1.Group("/__ROUTE__", deps.Authenticate())
	group.Get("/health", deps.Limiters.Relaxed, authzhttp.Require(__MODULE__catalog.PermissionRead), m.handler.Health)
}
EOF

cat <<'EOF' | render > "$tmp_dir/module_test.go"
package __MODULE__

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"__MODULE_PATH__/config"
	"__MODULE_PATH__/internal/app"
	__MODULE__catalog "__MODULE_PATH__/internal/modules/__MODULE__/catalog"
	"__MODULE_PATH__/internal/platform/authz"
	"__MODULE_PATH__/internal/platform/i18n"
	"__MODULE_PATH__/pkg/apperror"
)

func testConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		JWT:       config.JWTConfig{Secret: "__MODULE__-scaffold-test-secret"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

func TestCatalogRegistersAndLocalizes(t *testing.T) {
	m := Build(app.Infra{Config: testConfig()})
	if err := app.RegisterCatalogs(m); err != nil {
		t.Fatalf("RegisterCatalogs: %v", err)
	}
	if !authz.IsKnownPermission(__MODULE__catalog.PermissionRead) {
		t.Errorf("%s is not registered", __MODULE__catalog.PermissionRead)
	}
	if _, ok := apperror.SpecFor(__MODULE__catalog.CodeNotFound); !ok {
		t.Errorf("%s is not registered", __MODULE__catalog.CodeNotFound)
	}
	bundle, err := i18n.New("en", app.LocaleSources(m)...)
	if err != nil {
		t.Fatalf("locales: %v", err)
	}
	if !bundle.Has("vi", "errors."+string(__MODULE__catalog.CodeNotFound)+".title") {
		t.Error("missing vi title for the module error code")
	}
}

func TestHealthRouteRequiresAuthentication(t *testing.T) {
	cfg := testConfig()
	application := app.New(cfg, app.Deps{})
	application.Register(Build(app.Infra{Config: cfg}))

	resp, err := application.Fiber().Test(httptest.NewRequest(http.MethodGet, "/api/v1/__ROUTE__/health", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a token", resp.StatusCode)
	}
}
EOF

cat <<'EOF' | render > "$tmp_dir/catalog/catalog.go"
// Package catalog declares the __MODULE__ module's permissions
// (permissions.go), error codes (errors.go), and i18n strings (locales/). It
// is a leaf package so the module root, handlers, and services can all use
// the constants without an import cycle; the module root registers them
// through the app provider hooks. Values are public API: never change them
// once released.
package catalog

import (
	"embed"
	"io/fs"

	"__MODULE_PATH__/internal/platform/authz"
	"__MODULE_PATH__/pkg/apperror"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "__MODULE__"

// Register adds this catalog to the process-wide catalogs. At startup
// app.RegisterCatalogs does the same through the module provider hooks;
// handler/service unit tests that do not build the app call it directly.
// Repeating it is a no-op.
func Register() error {
	if err := authz.Register(Owner, Permissions()...); err != nil {
		return err
	}
	return apperror.Register(Owner, ErrorCodes())
}

//go:embed locales/*.json
var localesFS embed.FS

// Locales returns en.json/vi.json for app.LocaleProvider. Keys must start
// with "__MODULE__." or "errors.".
func Locales() fs.FS {
	sub, err := fs.Sub(localesFS, "locales")
	if err != nil {
		// fs.Sub only fails for an invalid path literal: a build bug.
		panic(err)
	}
	return sub
}
EOF

cat <<'EOF' | render > "$tmp_dir/catalog/permissions.go"
package catalog

import "__MODULE_PATH__/internal/platform/authz"

// Permissions owned by __MODULE__. Admins receive every permission
// implicitly; grant other roles deliberately.
const (
	PermissionRead  authz.Permission = "__MODULE__:read"
	PermissionWrite authz.Permission = "__MODULE__:write"
)

// Permissions returns the module's grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	return []authz.Grant{
		{Permission: PermissionRead},
		{Permission: PermissionWrite},
	}
}
EOF

cat <<'EOF' | render > "$tmp_dir/catalog/errors.go"
package catalog

import (
	"net/http"

	"__MODULE_PATH__/pkg/apperror"
)

// Error codes owned by __MODULE__. Add an en/vi title for each one in
// locales/.
const (
	CodeNotFound apperror.Code = "__MODULE__.not_found"
)

// ErrorCodes returns the module's error catalog for app.ErrorProvider.
func ErrorCodes() map[apperror.Code]apperror.Spec {
	return map[apperror.Code]apperror.Spec{
		CodeNotFound: {
			Status:  http.StatusNotFound,
			Title:   "Not found",
			DocPath: "/errors/__ROUTE__-not-found",
		},
	}
}
EOF

cat <<'EOF' | render > "$tmp_dir/catalog/locales/en.json"
{
  "errors.__MODULE__.not_found.title": "Not found"
}
EOF

cat <<'EOF' | render > "$tmp_dir/catalog/locales/vi.json"
{
  "errors.__MODULE__.not_found.title": "Không tìm thấy"
}
EOF

cat <<'EOF' | render > "$tmp_dir/handler/handler.go"
// Package handler owns HTTP transport for the __MODULE__ module.
package handler

import (
	"github.com/gofiber/fiber/v3"

	__MODULE__dto "__MODULE_PATH__/internal/modules/__MODULE__/dto"
	__MODULE__service "__MODULE_PATH__/internal/modules/__MODULE__/service"
	"__MODULE_PATH__/pkg/response"
)

// Handler binds requests, validates input, and calls the service.
type Handler struct {
	service *__MODULE__service.Service
}

// New returns a handler backed by service.
func New(service *__MODULE__service.Service) *Handler {
	return &Handler{service: service}
}

// Health is a placeholder used by the scaffold; replace it with real handlers.
func (h *Handler) Health(c fiber.Ctx) error {
	if err := h.service.Ping(c.Context()); err != nil {
		return err
	}
	return response.Success(c, __MODULE__dto.HealthResponse{
		Module: "__MODULE__",
		Status: "ok",
	})
}
EOF

cat <<'EOF' | render > "$tmp_dir/service/service.go"
// Package service owns business rules for the __MODULE__ module.
package service

import "context"

// Repository is the persistence surface this service needs. Keep the
// interface narrow and declare it here so tests can replace it.
type Repository interface {
	Ping(ctx context.Context) error
}

// Service coordinates validation, transactions, and repository calls.
type Service struct {
	repository Repository
}

// New returns a service backed by repository.
func New(repository Repository) *Service {
	return &Service{repository: repository}
}

// Ping is a placeholder; replace it with real use cases.
func (s *Service) Ping(ctx context.Context) error {
	return s.repository.Ping(ctx)
}
EOF

cat <<'EOF' | render > "$tmp_dir/repository/repository.go"
// Package repository owns persistence for the __MODULE__ module.
package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the module's PostgreSQL access point. Keep SQL and row
// mapping here; handlers and services must not reach the database directly.
// After make sqlc-generate, wrap the module's generated sqlc.Queries.
type Repository struct {
	pool *pgxpool.Pool
}

// New returns a repository backed by pool.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Ping is a placeholder; remove it when the module has real queries.
func (r *Repository) Ping(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	return r.pool.Ping(ctx)
}
EOF

cat <<'EOF' | render > "$tmp_dir/dto/dto.go"
// Package dto contains the __MODULE__ module's transport types.
package dto

// Item is a placeholder response; replace it with the module's real types.
type Item struct {
	ID int64 `json:"id"`
}

// CreateItemRequest is a placeholder request with validation.
type CreateItemRequest struct {
	Name string `json:"name" validate:"required,max=255"`
}

// HealthResponse is the scaffold's placeholder health response.
type HealthResponse struct {
	Module string `json:"module"`
	Status string `json:"status"`
}
EOF

cat <<'EOF' | render > "$tmp_dir/queries/$name.sql"
-- sqlc source owned by the __MODULE__ module. Reference only tables this
-- module owns (named __MODULE__ or __MODULE___*); test/arch rejects SQL that
-- touches another module's tables. Replace the placeholder, then run
-- make sqlc-generate.

-- name: Ping :one
SELECT 1::int AS ok;
EOF

cat <<'EOF' | render > "$tmp_dir/README.md"
# __MODULE__ module

Scaffolded by `scripts/new-module.sh`. Replace the placeholder `Ping`/`Health`
code and fill in this file before shipping.

## Ownership

- Tables: `__MODULE__` / `__MODULE___*` (one module owns each table; never
  query another module's tables — `test/arch` checks `queries/*.sql`)
- Routes: `GET /api/v1/__ROUTE__/health` (placeholder)
- Permissions: `__MODULE__:read`, `__MODULE__:write` (admin-only until granted)
- Error codes: `__MODULE__.not_found`
- Jobs / events / schedules: none
- Contracts: add one under `internal/contracts/__MODULE__/` if another module
  needs this module's data

## Layout

- `module.go` — `app.Module`, `Build` constructor, catalog providers, routes
- `catalog/` — permissions, error codes, en/vi strings (public identifiers)
- `handler/` — HTTP transport
- `service/` — business rules and repository interface
- `repository/` — persistence
- `queries/` — sqlc source (block appended to `sqlc.yaml`)
- `dto/` — request/response types

## Next steps

1. Add the module to `internal/bootstrap/modules.go` (`__MODULE__.Build(infra)`
   in the returned `List`). That is the only central edit.
2. Add a migration: `make migrate-create module=__MODULE__ name=<purpose>`.
3. Replace `queries/__MODULE__.sql`, run `make sqlc-generate`, and implement
   repository -> service -> handler and routes.
4. Run `make check`, `go test -race ./...`, and `make swagger` if routes
   changed.

See [`docs/module-template.md`](../../../docs/module-template.md).
EOF

if command -v gofmt >/dev/null 2>&1; then
  gofmt -w \
    "$tmp_dir/module.go" \
    "$tmp_dir/module_test.go" \
    "$tmp_dir/catalog/catalog.go" \
    "$tmp_dir/catalog/permissions.go" \
    "$tmp_dir/catalog/errors.go" \
    "$tmp_dir/handler/handler.go" \
    "$tmp_dir/service/service.go" \
    "$tmp_dir/repository/repository.go" \
    "$tmp_dir/dto/dto.go"
else
  echo "warning: gofmt not found; generated Go files may need formatting" >&2
fi

# Check again immediately before the move so a concurrent creation cannot be
# silently overwritten.
if [ -e "$target" ]; then
  echo "error: module already exists: $target_rel" >&2
  exit 1
fi

if [ -f "$sqlc_file" ]; then
  sqlc_backup="$sqlc_file.new-module.$$"
  cp "$sqlc_file" "$sqlc_backup"
fi

mv "$tmp_dir" "$target"
published=1

if [ -f "$sqlc_file" ]; then
  cat <<EOF >> "$sqlc_file"
  - engine: "postgresql"
    queries: "internal/modules/$name/queries/"
    schema: "migrations/"
    gen:
      go:
        package: "sqlc"
        out: "internal/modules/$name/sqlc"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_empty_slices: true
        omit_unused_structs: true
EOF
fi

# Fail closed on a broken scaffold. A dependency/build failure elsewhere in the
# repository is reported verbatim; the partially published module and its
# sqlc.yaml block are removed.
if command -v go >/dev/null 2>&1; then
  if ! (cd "$repo_root" && go build "./internal/modules/$name/..." && go vet "./internal/modules/$name/..." && go test -count=1 "./internal/modules/$name/..."); then
    echo "error: generated module failed to build, vet, or test: ./$target_rel/..." >&2
    echo "Removing $target_rel and its sqlc.yaml block; nothing partial is left behind." >&2
    exit 1
  fi
  echo "Build, vet, and test passed: ./$target_rel/..."
else
  echo "warning: go not found; skipped the compile/test check for $target_rel" >&2
fi
published=0

echo "Created module '$name'"
for rel in "${files[@]}"; do
  echo "  $rel"
done
if [ -f "$sqlc_file" ]; then
  echo "  sqlc.yaml (appended the internal/modules/$name block)"
fi
echo
echo "Remaining manual steps:"
echo "  1. Register the module in internal/bootstrap/modules.go:"
echo "       add \"$module_path/$target_rel\" to the imports and"
echo "       $name.Build(infra) to the returned List (the only central edit)."
echo "  2. Add a migration:  make migrate-create module=$name name=<purpose>"
echo "  3. Replace queries/$name.sql, then: make sqlc-generate"
echo "  4. Implement repository -> service -> handler and real routes."
echo "  5. Run: make check && go test -race ./... (test/arch then covers the"
echo "     module's routes, permissions, and SQL ownership automatically)."
