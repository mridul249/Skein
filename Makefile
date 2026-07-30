SHELL       := /bin/bash
BIN         := bin/skein
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
GOBIN       := $(shell go env GOPATH)/bin
WEB_DIST    := internal/web/dist

export CGO_ENABLED := 0

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## run: run the server with .env loaded
run:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/skein

## build: build the single binary (frontend must be built first)
build: web
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/skein
	@ls -lh $(BIN)

## build-go: build the binary without rebuilding the frontend
build-go:
	@mkdir -p bin $(WEB_DIST)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/skein

## test: run the full Go test suite with the race detector
test:
	go test -race -count=1 $(PKG)

## test-short: skip the slow memory-ceiling and integration tests
test-short:
	go test -short -count=1 $(PKG)

## bench: run the streaming memory benchmark
bench:
	go test -run '^$$' -bench . -benchmem ./internal/uploads/... ./internal/shard/...

## lint: gofmt check, go vet, golangci-lint
lint:
	@gofmt -l . | grep -v '^web/' | (! grep .) || (echo "gofmt needed on the files above"; exit 1)
	go vet $(PKG)
	$(GOBIN)/golangci-lint run

## sqlc: regenerate internal/db/gen from queries (never hand-edit the output)
sqlc:
	$(GOBIN)/sqlc generate

## migrate: apply migrations against SKEIN_DATABASE_URL
migrate:
	@set -a; [ -f .env ] && . ./.env; set +a; \
	$(GOBIN)/goose -dir internal/db/migrations postgres "$$SKEIN_DATABASE_URL" up

## migrate-down: roll back one migration
migrate-down:
	@set -a; [ -f .env ] && . ./.env; set +a; \
	$(GOBIN)/goose -dir internal/db/migrations postgres "$$SKEIN_DATABASE_URL" down

## migrate-status: show migration state
migrate-status:
	@set -a; [ -f .env ] && . ./.env; set +a; \
	$(GOBIN)/goose -dir internal/db/migrations postgres "$$SKEIN_DATABASE_URL" status

## web: build the frontend into internal/web/dist for embedding
web:
	cd web && npm ci && npm run build
	@# Vite empties the output directory, which takes the .gitkeep with it.
	@# //go:embed needs internal/web/dist to exist on a fresh clone, so the
	@# marker is restored here rather than left to whoever notices the
	@# build breaking.
	@touch $(WEB_DIST)/.gitkeep

## web-dev: run the Vite dev server against a local API
web-dev:
	cd web && npm run dev

## dev-db: start the development Postgres (dev only, never for deployment)
dev-db:
	docker compose up -d postgres

## dev-db-down: stop and remove the development Postgres
dev-db-down:
	docker compose down -v

## tools: install the pinned developer tooling
tools:
	go install github.com/pressly/goose/v3/cmd/goose@latest
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

## clean: remove build output
clean:
	rm -rf bin $(WEB_DIST)/* web/node_modules

.PHONY: help run build build-go test test-short bench lint sqlc migrate \
        migrate-down migrate-status web web-dev dev-db dev-db-down tools clean
