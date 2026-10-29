# Makefile for the API, the migrator, and developer tooling.
-include .env

# Go tool versions for air/sqlc/oasdiff/govulncheck live in go.mod `tool`
# directives (go get -tool), so the Makefile, CI, and Dependabot agree. The
# linter is pinned below and installed into bin/ on demand; the migrate CLI is
# not used (cmd/migrate owns up/down), and migration files are created with the
# shell so no extra binary is required.

# Build outputs. Windows executables need the .exe suffix.
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif
BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server$(EXE)
MIGRATE_BIN := $(BIN_DIR)/migrate$(EXE)
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint$(EXE)

all: build test

# Build the API server and the migrator
build:
	@echo "Building $(SERVER_BIN) and $(MIGRATE_BIN)..."
	@go build -o $(SERVER_BIN) ./cmd/api
	@go build -o $(MIGRATE_BIN) ./cmd/migrate

# Local-only build: the dev tag loads .env at runtime. Release builds (make
# build, Docker) omit it, so the shipped binaries never read a .env file.
build-dev:
	@echo "Building $(SERVER_BIN) and $(MIGRATE_BIN) with .env support..."
	@go build -tags dev -o $(SERVER_BIN) ./cmd/api
	@go build -tags dev -o $(MIGRATE_BIN) ./cmd/migrate

# Run the application (schema first, then API) with .env loaded by the dev build tag.
run: migrate-run
	@go run -tags dev ./cmd/api

# Create a local .env from .env.example with freshly generated secrets.
env:
	@bash scripts/init-env.sh

# Apply pending migrations and verify the schema through cmd/migrate, the only
# schema initializer/migrator. The dev build tag loads .env; release builds never do.
migrate-run:
	@go run -tags dev ./cmd/migrate

# Start the Compose stack (migrate job, then API)
docker-run:
	@docker compose up --build

# Stop the Compose stack
docker-down:
	@docker compose down

# Verify the application (fast local gate)
check: vet lint arch test

vet:
	@go vet ./...

# Enforce module dependency boundaries
arch:
	@go test ./test/arch

# Unit tests
test:
	@echo "Testing..."
	@go test ./... -v

# Unit tests with the race detector (required before merging; needs cgo)
test-race:
	@go test -race ./...

# Integration tests (Docker/Testcontainers, or set TEST_DATABASE_URL)
test-integration:
	@echo "Running integration tests..."
	@go test ./... -v -tags=integration -count=1 -timeout=10m

# Remove build outputs
clean:
	@echo "Cleaning..."
	@rm -rf $(BIN_DIR)

# Live reload: rebuilds and restarts the API on change
watch:
	@echo "Watching..."
	@go tool air -build.cmd "$(MAKE) build-dev" -build.entrypoint "$(SERVER_BIN)"

# Migration authoring/destructive helpers. Applying migrations goes through
# migrate-run; these are for local authoring and disposable databases only.

# Roll back every migration. The init down migration DROPS THE
# WHOLE SCHEMA and all data, so it requires CONFIRM=<DB_DATABASE>.
migrate-down:
	@test -n "$(DB_DATABASE)" || { echo "DB_DATABASE is not set; refusing to roll back"; exit 1; }
	@if [ "$(CONFIRM)" != "$(DB_DATABASE)" ]; then \
		echo "WARNING: migrate-down runs every down migration, including the init schema,"; \
		echo "         which DROPS THE WHOLE SCHEMA AND ALL DATA in $(DB_DATABASE) on $(DB_HOST):$(DB_PORT)."; \
		echo "Re-run with 'make migrate-down CONFIRM=$(DB_DATABASE)' only against a disposable database."; \
		exit 1; \
	fi
	@echo "WARNING: dropping the whole schema in $(DB_DATABASE) on $(DB_HOST):$(DB_PORT)..."
	@go run -tags dev ./cmd/migrate -down -confirm-database "$(DB_DATABASE)"

# Timestamp versions (UTC, 20060102150405) instead of -seq: two branches that
# each add a migration no longer collide on the same number. Files are named
# <ts>_<module>_<purpose>.{up,down}.sql; test/arch/migrations_test.go rejects
# duplicate versions, missing up/down pairs, and unknown owners.
# Usage: make migrate-create module=files name=add_checksum
migrate-create:
	@test -n "$(module)" -a -n "$(name)" || { echo "usage: make migrate-create module=<module|platform> name=<purpose>"; exit 1; }
	@version=$$(date -u +%Y%m%d%H%M%S); \
	base="migrations/$${version}_$(module)_$(name)"; \
	test ! -e "$${base}.up.sql" -a ! -e "$${base}.down.sql" || { echo "$${base}.up.sql or $${base}.down.sql already exists"; exit 1; }; \
	: > "$${base}.up.sql"; \
	: > "$${base}.down.sql"; \
	echo "created $${base}.up.sql and $${base}.down.sql"

# sqlc is a pinned go.mod tool, so its version matches CI by construction. Each
# module owns internal/modules/<name>/sqlc.yaml (paths relative to it); this
# runs every one and stops at the first failure.
sqlc-generate:
	@for config in internal/modules/*/sqlc.yaml; do \
		echo "go tool sqlc generate -f $$config"; \
		go tool sqlc generate -f "$$config" || exit 1; \
	done

# Code generator (cmd/gen, layered module layout). It writes the files, then runs
# sqlc, go build/vet, the module tests, and test/arch, and rolls every write
# back if a step fails. Usage:
#   make gen module=billing
#   make gen resource=invoice module=billing [plural=invoices]
#   make gen command=IssueInvoice module=billing [no_http=1]
#   make gen query=ListOverdueInvoices module=billing [no_http=1]
# Add dry_run=1 to print a unified diff and write nothing.
GEN_FLAGS = $(if $(dry_run),--dry-run) $(if $(no_http),--no-http)
gen:
	@if [ -n "$(resource)" ]; then \
		go run ./cmd/gen resource --module "$(module)" $(GEN_FLAGS) $(if $(plural),--plural "$(plural)") "$(resource)"; \
	elif [ -n "$(command)" ]; then \
		go run ./cmd/gen command --module "$(module)" $(GEN_FLAGS) "$(command)"; \
	elif [ -n "$(query)" ]; then \
		go run ./cmd/gen query --module "$(module)" $(GEN_FLAGS) "$(query)"; \
	elif [ -n "$(module)" ]; then \
		go run ./cmd/gen module $(GEN_FLAGS) "$(module)"; \
	else \
		go run ./cmd/gen -h; \
	fi

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	@GOBIN=$(CURDIR)/$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: $(GOLANGCI_LINT)
	@$(GOLANGCI_LINT) run ./...

# Seed the admin user (schema must already exist; migrate-run is a dependency)
seed: migrate-run
	@echo "Seeding database..."
	@go run -tags dev ./cmd/seed

# Regenerate the committed OpenAPI 3.1 document from the registered huma
# operations. No database or network access is required.
openapi:
	@go run ./cmd/openapi

# Rename the module path (usage: make rename-module mod=github.com/yourname/yourproject)
rename-module:
	@bash scripts/rename-module.sh $(mod)

.PHONY: all build build-dev run env migrate-run docker-run docker-down check vet arch test test-race test-integration clean watch migrate-down migrate-create sqlc-generate gen lint seed openapi rename-module
