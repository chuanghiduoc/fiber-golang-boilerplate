# Simple Makefile for a Go project
-include .env

# Build the application
all: build test

build:
	@echo "Building..."
	@go build -o main.exe ./cmd/api
	@go build -o migrate.exe ./cmd/migrate

# Run the application (schema first, then API)
run: migrate-run
	@go run ./cmd/api

# Apply the single init schema through the project migration command
migrate-run:
	@go run ./cmd/migrate

# Create DB container
docker-run:
	@docker compose up --build

# Shutdown DB container
docker-down:
	@docker compose down

# Verify the application (fast local gate)
check: vet lint arch test

vet:
	@go vet ./...

# Enforce module dependency boundaries
arch:
	@go test ./test/arch

# Test the application
test:
	@echo "Testing..."
	@go test ./... -v

# Integration tests (Docker/Testcontainers, or set TEST_DATABASE_URL)
test-integration:
	@echo "Running integration tests..."
	@go test ./... -v -tags=integration -count=1 -timeout=10m

# Clean the binary
clean:
	@echo "Cleaning..."
	@rm -f main.exe migrate.exe

# Live Reload
watch:
	@command -v air >/dev/null 2>&1 || { echo "Installing air..."; go install github.com/air-verse/air@latest; }
	@echo "Watching..."
	@air

# Database migrations
DSN ?= postgres://$(DB_USERNAME):$(DB_PASSWORD)@$(DB_HOST):$(DB_PORT)/$(DB_DATABASE)?sslmode=$(DB_SSLMODE)&search_path=$(DB_SCHEMA)

migrate-up:
	@migrate -path migrations -database "$(DSN)" up

migrate-down:
	@migrate -path migrations -database "$(DSN)" down

migrate-create:
	@migrate create -ext sql -dir migrations -seq $(name)

# SQLC
sqlc-generate:
	@sqlc generate

# Lint
lint:
	@golangci-lint run ./...

# Seed database (schema must already exist; migrate-run is a dependency)
seed: migrate-run
	@echo "Seeding database..."
	@go run ./cmd/seed

# Swagger
swagger:
	@swag init -g cmd/api/main.go -o docs

# Rename module path (usage: make rename-module mod=github.com/yourname/yourproject)
rename-module:
	@bash scripts/rename-module.sh $(mod)

.PHONY: all build run migrate-run check vet arch test test-integration clean watch docker-run docker-down migrate-up migrate-down migrate-create sqlc-generate lint swagger seed rename-module
