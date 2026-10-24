#!/usr/bin/env bash
#
# Scaffold a new business module under internal/modules/<name>.
#
# Usage:
#   scripts/new-module.sh <name>
#   scripts/new-module.sh --dry-run <name>
#   scripts/new-module.sh <name> --dry-run
#
# <name> must be lower_snake_case. The scaffold is compile-safe but registers
# no routes; wire routes and replace the placeholder Ping methods when ready.

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/new-module.sh [--dry-run] <name>

Create internal/modules/<name>/ with module.go, handler/handler.go,
service/service.go, repository/repository.go, dto/dto.go, and README.md.

Options:
  --dry-run    Print the paths that would be created; write nothing.
  -h, --help   Show this help.

<name> must match [a-z][a-z0-9]*(_[a-z0-9]+)* and must not be a Go keyword
or a reserved package name. Existing modules are never overwritten.
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
  app|main|internal|modules|platform|pkg|handler|service|repository|dto|sqlc|config|testutil|middleware|authz|audit|jobs|session)
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

if [ -e "$target" ]; then
  echo "error: module already exists: $target_rel" >&2
  echo "Refusing to overwrite it." >&2
  exit 1
fi

files=(
  "$target_rel/module.go"
  "$target_rel/handler/handler.go"
  "$target_rel/service/service.go"
  "$target_rel/repository/repository.go"
  "$target_rel/dto/dto.go"
  "$target_rel/README.md"
)

if [ "$dry_run" -eq 1 ]; then
  echo "Dry run for module '$name' (module path: $module_path)"
  for rel in "${files[@]}"; do
    echo "  would create $rel"
  done
  echo "No files written."
  exit 0
fi

mkdir -p "$repo_root/internal/modules"
tmp_dir="$repo_root/internal/modules/.${name}.tmp.$$"

cleanup() {
  if [ -n "${tmp_dir:-}" ] && [ -d "$tmp_dir" ]; then
    rm -rf "$tmp_dir"
  fi
}
trap cleanup EXIT

if [ -e "$tmp_dir" ]; then
  echo "error: temporary path already exists: $tmp_dir" >&2
  exit 1
fi

mkdir -p "$tmp_dir/handler" "$tmp_dir/service" "$tmp_dir/repository" "$tmp_dir/dto"

# Replace the long token first: __MODULE_PATH__ contains the shorter token as
# a prefix. The escaped copy keeps sed from treating '&' or '|' specially.
escaped_module_path=$(printf '%s' "$module_path" | sed 's/[&|]/\\&/g')

render() {
  sed -e "s|__MODULE_PATH__|$escaped_module_path|g" \
      -e "s|__MODULE__|$name|g"
}

cat <<'EOF' | render > "$tmp_dir/module.go"
// Package __MODULE__ owns the __MODULE__ business capability.
//
// It implements app.Module and owns this module's route table. Keep business
// rules in service, persistence in repository, and HTTP in handler.
package __MODULE__

import (
	"github.com/gofiber/fiber/v3"

	"__MODULE_PATH__/internal/app"
	__MODULE__handler "__MODULE_PATH__/internal/modules/__MODULE__/handler"
)

// Module wires the __MODULE__ HTTP surface into the application.
type Module struct {
	handler *__MODULE__handler.Handler
}

// New builds the module from its handler. Construct the handler, service, and
// repository in cmd/api/main.go.
func New(handler *__MODULE__handler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return "__MODULE__" }

// RegisterRoutes mounts routes below /api/v1. The scaffold is intentionally
// empty; copy the patterns from docs/module-template.md when the handlers are
// ready.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
}
EOF

cat <<'EOF' | render > "$tmp_dir/handler/handler.go"
// Package handler owns HTTP transport for the __MODULE__ module.
package handler

import (
	"github.com/gofiber/fiber/v3"

	__MODULE__service "__MODULE_PATH__/internal/modules/__MODULE__/service"
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
	return c.JSON(fiber.Map{"module": "__MODULE__", "status": "ok"})
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

import "context"

// Repository is the module's PostgreSQL access point. Keep SQL and row
// mapping here; handlers and services must not reach the database directly.
type Repository struct{}

// New returns a repository. Replace the empty struct with the pgx pool and,
// once added, the module's sqlc queries.
func New() *Repository {
	return &Repository{}
}

// Ping is a placeholder; remove it when the module has real queries.
func (r *Repository) Ping(context.Context) error {
	return nil
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
EOF

cat <<'EOF' | render > "$tmp_dir/README.md"
# __MODULE__ module

Scaffolded by `scripts/new-module.sh`. Replace the placeholder `Ping` methods
and fill in this file before shipping.

## Ownership

- Tables: `__MODULE___...` (one module owns each table; never query another
  module's tables)
- Routes: `/api/v1/__MODULE__` (not registered yet)
- Permissions: `__MODULE__:read`, `__MODULE__:write`, ...
- Jobs: none
- Contracts: add one under `internal/contracts/` if another module needs a read

## Layout

- `module.go` — `app.Module` implementation and route table
- `handler/` — HTTP transport
- `service/` — business rules and repository interface
- `repository/` — persistence
- `dto/` — request/response types

## Next steps

1. Add permissions to `internal/platform/authz/permissions.go`.
2. Add the table/index migration (baseline only for an undeployed upstream,
   otherwise the next numbered migration) and `queries/__MODULE__.sql`.
3. Implement repository -> service -> handler and add routes in `module.go`.
4. Wire dependencies and register `__MODULE__.New(...)` in `cmd/api/main.go`.
5. Add tests and run `go fmt ./internal/modules/__MODULE__/...`, `make arch`, `make check`,
   `go test -race ./...`, and `make swagger` if routes changed.
6. Update this README with the final tables, routes, permissions, jobs, and
   contracts.

See [`docs/module-template.md`](../../../docs/module-template.md).
EOF

if command -v gofmt >/dev/null 2>&1; then
  gofmt -w \
    "$tmp_dir/module.go" \
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

mv "$tmp_dir" "$target"

echo "Created module '$name'"
for rel in "${files[@]}"; do
  echo "  $rel"
done
echo
echo "Next steps:"
echo "  1. Replace the placeholder Ping methods and add routes in"
echo "     $target_rel/module.go."
echo "  2. Add permissions, a migration, and queries."
echo "  3. Register the module in cmd/api/main.go."
echo "  4. Run: go fmt ./$target_rel/... && go build ./... && make arch && make check"
