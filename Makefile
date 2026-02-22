# Simple Makefile for a Go project

# Build the application
all: build test

build:
	@echo "Building..."
	@go build -o main.exe ./cmd/api

# Run the application
run:
	@go run ./cmd/api

# Create DB container
docker-run:
	@docker compose up --build

# Shutdown DB container
docker-down:
	@docker compose down

# Test the application
test:
	@echo "Testing..."
	@go test ./... -v

# Integration tests (requires Docker)
test-integration:
	@echo "Running integration tests..."
	@go test ./... -v -tags=integration -count=1 -timeout=120s

# Clean the binary
clean:
	@echo "Cleaning..."
	@rm -f main.exe

# Live Reload
watch:
	@powershell -ExecutionPolicy Bypass -Command "if (Get-Command air -ErrorAction SilentlyContinue) { \
		air; \
		Write-Output 'Watching...'; \
	} else { \
		Write-Output 'Installing air...'; \
		go install github.com/air-verse/air@latest; \
		air; \
		Write-Output 'Watching...'; \
	}"

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

# Seed database
seed:
	@echo "Seeding database..."
	@go run ./cmd/seed

# Swagger
swagger:
	@swag init -g cmd/api/main.go -o docs

# Rename module path (usage: make rename-module mod=github.com/yourname/yourproject)
rename-module:
	@bash scripts/rename-module.sh $(mod)

.PHONY: all build run test test-integration clean watch docker-run docker-down migrate-up migrate-down migrate-create sqlc-generate lint swagger seed rename-module
