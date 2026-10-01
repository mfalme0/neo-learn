# Neo Learn -- developer entry points.
#
# On Windows (PowerShell) run `make` via `mingw32-make` or Git Bash. The
# `ENV` variable differs per shell; see the note in the README.

SHELL := /bin/bash

DATABASE_URL ?= postgres://neolearn:neolearn@localhost:5433/neolearn?sslmode=disable
MIGRATIONS  ?= db/migrations

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Infrastructure
# ---------------------------------------------------------------------------

.PHONY: up
up: ## Start Postgres and Redis
	docker compose up -d
	docker compose ps

.PHONY: down
down: ## Stop containers, keep data
	docker compose down

.PHONY: clean-data
clean-data: ## Stop containers and delete the database volume
	docker compose down -v

.PHONY: wait-db
wait-db: ## Block until Postgres reports healthy
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' neo-learn-postgres)" = "healthy" ]; do \
		sleep 1; \
	done

# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply pending migrations
	migrate -path $(MIGRATIONS) -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	migrate -path $(MIGRATIONS) -database "$(DATABASE_URL)" down 1

.PHONY: migrate-force
migrate-force: ## Force migration to VERSION=n (e.g. make migrate-force VERSION=1)
	migrate -path $(MIGRATIONS) -database "$(DATABASE_URL)" force $(VERSION)

.PHONY: migrate-version
migrate-version: ## Print current schema version
	migrate -path $(MIGRATIONS) -database "$(DATABASE_URL)" version

.PHONY: sqlc
sqlc: ## Regenerate Go code from SQL queries
	sqlc generate

# ---------------------------------------------------------------------------
# Build and run
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Compile the API
	go build ./...

.PHONY: run
run: ## Run the API with hot reload disabled
	go run ./cmd/api

.PHONY: test
test: ## Run all tests
	go test ./... -count=1

.PHONY: test-race
test-race: ## Run all tests with the race detector
	go test ./... -count=1 -race

.PHONY: cover
cover: ## Run tests and report coverage
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -n 20

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format Go sources
	gofmt -w -s .

.PHONY: lint
lint: vet ## Vet plus gofmt drift check
	@test -z "$$(gofmt -l -s . | tee /dev/stderr)" || (echo "gofmt needed" && exit 1)

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: check
check: lint test ## Everything CI runs

# ---------------------------------------------------------------------------
# Web
# ---------------------------------------------------------------------------

.PHONY: web-install
web-install: ## Install web dependencies
	cd web && npm install

.PHONY: web-dev
web-dev: ## Run the Next.js dev server
	cd web && npm run dev