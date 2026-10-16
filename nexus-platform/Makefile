# Nexus Platform — common developer commands.
# Usage: make <target>. DB_URL is the golang-migrate DSN for local dev.

DB_URL ?= mysql://root:@tcp(127.0.0.1:3306)/nexus_platform

.PHONY: help build run test test-integration vet fmt check migrate-up migrate-down docker-up docker-down tidy

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

tidy: ## Sync go.mod/go.sum
	go mod tidy

build: ## Compile the server binary
	go build -o bin/server cmd/server/main.go

run: ## Run the server (needs MySQL up — see docker-up)
	go run cmd/server/main.go

test: ## Run all tests
	go test ./... -v

# Prereq: a reachable MySQL 8 (run `make docker-up` then `make migrate-up`), the
# golang-migrate CLI on PATH (used by the a4_rbac harness), and TEST_DATABASE_URL set
# to the go-sql-driver DSN (the wrapper defaults it to root:@tcp(127.0.0.1:3306)/nexus_platform
# when unset and appends clientFoundRows=true, the matched-rows semantics production runs
# under — specs/057-found-rows-dsn). Suites SKIP, not fail, when MySQL or the migrate CLI
# is missing, so a bare `go test` printed `ok` on a dead database. Run through the wrapper: any `--- SKIP` or
# zero `--- PASS` exits non-zero (#160). -timeout 25m matches CI: under parallel-worktree load
# handlers (919s) and services (836s) both ran past Go's 10m per-package default (2026-09-30).
# CI (ci.yml, job integration) runs the same wrapper (#244), so a dead MySQL service or a missing
# migrate CLI turns the job red instead of an all-SKIP green.
test-integration: ## Run integration tests (-tags=integration -race; SKIP = fail; needs MySQL up + migrated)
	bash scripts/run-integration.sh -race -timeout 25m ./...

vet: ## Static analysis
	go vet ./...

fmt: ## Format code
	gofmt -w .

check: vet test build ## Full pre-commit gate: vet -> test -> build

migrate-up: ## Apply DB migrations (requires golang-migrate)
	migrate -path migrations -database "$(DB_URL)" up

migrate-down: ## Roll back the last migration
	migrate -path migrations -database "$(DB_URL)" down 1

docker-up: ## Start MySQL 8 + Redis (from repo root docker-compose.yml)
	cd .. && docker compose up -d

docker-down: ## Stop MySQL 8 + Redis
	cd .. && docker compose down
