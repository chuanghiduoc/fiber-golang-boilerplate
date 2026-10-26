# Makefile for the API, the migrator, and developer tooling.
-include .env

# Tool versions. Keep SWAG_VERSION and SQLC_VERSION in sync with
# .github/workflows/ci.yml so generated code matches the CI drift checks.
SWAG_VERSION ?= v1.16.6
SQLC_VERSION ?= v1.31.1
AIR_VERSION ?= v1.67.4

# Build outputs. Windows executables need the .exe suffix.
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif
BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server$(EXE)
MIGRATE_BIN := $(BIN_DIR)/migrate$(EXE)

all: build test

# Build the API server and the migrator
build:
	@echo "Building $(SERVER_BIN) and $(MIGRATE_BIN)..."
	@go build -o $(SERVER_BIN) ./cmd/api
	@go build -o $(MIGRATE_BIN) ./cmd/migrate

# Run the application (schema first, then API)
run: migrate-run
	@go run ./cmd/api

# Apply pending migrations and verify the schema through cmd/migrate, the only
# schema initializer/migrator.
migrate-run:
	@go run ./cmd/migrate

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
	@go run github.com/air-verse/air@$(AIR_VERSION) -build.cmd "$(MAKE) build" -build.entrypoint "$(SERVER_BIN)"

# golang-migrate CLI helpers. Applying migrations goes through migrate-run;
# these are for authoring and for disposable local databases only.
DSN ?= postgres://$(DB_USERNAME):$(DB_PASSWORD)@$(DB_HOST):$(DB_PORT)/$(DB_DATABASE)?sslmode=$(DB_SSLMODE)&search_path=$(DB_SCHEMA)

# Roll back every migration. The init down migration DROPS THE
# WHOLE SCHEMA and all data, so it requires CONFIRM=yes.
migrate-down:
	@if [ "$(CONFIRM)" != "yes" ]; then \
		echo "WARNING: migrate-down runs every down migration, including the init schema,"; \
		echo "         which DROPS THE WHOLE SCHEMA AND ALL DATA in $(DB_DATABASE) on $(DB_HOST):$(DB_PORT)."; \
		echo "Re-run with 'make migrate-down CONFIRM=yes' only against a disposable database."; \
		exit 1; \
	fi
	@echo "WARNING: dropping the whole schema in $(DB_DATABASE) on $(DB_HOST):$(DB_PORT)..."
	@migrate -path migrations -database "$(DSN)" down

# Timestamp versions (UTC, 20060102150405) instead of -seq: two branches that
# each add a migration no longer collide on the same number. Files are named
# <ts>_<module>_<purpose>.{up,down}.sql; test/arch/migrations_test.go rejects
# duplicate versions, missing up/down pairs, and unknown owners.
# Usage: make migrate-create module=files name=add_checksum
migrate-create:
	@test -n "$(module)" -a -n "$(name)" || { echo "usage: make migrate-create module=<module|platform> name=<purpose>"; exit 1; }
	@migrate create -ext sql -dir migrations -format 20060102150405 -tz UTC $(module)_$(name)

# sqlc needs cgo, so it stays an external binary instead of `go run`. Each
# module owns internal/modules/<name>/sqlc.yaml (paths relative to it); this
# runs every one and stops at the first failure.
sqlc-generate:
	@command -v sqlc >/dev/null 2>&1 || { echo "sqlc not found; install $(SQLC_VERSION) (the CI version): go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)"; exit 1; }
	@sqlc version | grep -qx "$(SQLC_VERSION)" || echo "warning: sqlc $$(sqlc version) differs from $(SQLC_VERSION) used in CI; generated code may drift"
	@for config in internal/modules/*/sqlc.yaml; do \
		echo "sqlc generate -f $$config"; \
		sqlc generate -f "$$config" || exit 1; \
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

lint:
	@golangci-lint run ./...

# Seed the admin user (schema must already exist; migrate-run is a dependency)
seed: migrate-run
	@echo "Seeding database..."
	@go run ./cmd/seed

swagger:
	@go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION) init -g cmd/api/main.go -o docs

# Rename the module path (usage: make rename-module mod=github.com/yourname/yourproject)
rename-module:
	@bash scripts/rename-module.sh $(mod)

.PHONY: all build run migrate-run docker-run docker-down check vet arch test test-race test-integration clean watch migrate-down migrate-create sqlc-generate gen lint seed swagger rename-module
