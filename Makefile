SHELL       := /bin/bash
BIN         := bin/skein
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
GOBIN       := $(shell go env GOPATH)/bin
WEB_DIST    := internal/web/dist

export CGO_ENABLED := 0

LOADENV = pre_db="$$SKEIN_DATABASE_URL"; \
	set -a; [ -f .env ] && . ./.env; set +a; \
	if [ -n "$$pre_db" ]; then \
		SKEIN_DATABASE_URL="$$pre_db"; src="environment"; \
	else \
		src=".env"; \
	fi

# SHOWDB names the database a target is about to touch, host and name only.
SHOWDB = printf '\033[36m--> %s (from %s)\033[0m\n' \
	"$$(printf '%s' "$$SKEIN_DATABASE_URL" | sed -E 's|^[a-z+]+://||; s|^[^@/]*@||; s|\?.*$$||')" \
	"$$src"

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## run: run the server with .env loaded
run:
	@$(LOADENV); $(SHOWDB); go run ./cmd/skein

## build: build the single binary (frontend must be built first)
build: web
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/skein
	@ls -lh $(BIN)

## build-go: build the binary without rebuilding the frontend
build-go:
	@mkdir -p bin $(WEB_DIST)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/skein

## desktop: build the native desktop binary via wails build (requires cgo;
## make web first, or ErrNoUI surfaces at launch instead of at build time)
DESKTOP_CLIENT_ID ?=
DESKTOP_CLIENT_SECRET ?=
DESKTOP_LDFLAGS = -X main.version=$(VERSION) \
	-X main.desktopClientID=$(DESKTOP_CLIENT_ID) \
	-X main.desktopClientSecret=$(DESKTOP_CLIENT_SECRET)

desktop: web
	@mkdir -p bin
	cd cmd/skein-desktop && $(GOBIN)/wails build -tags webkit2_41,desktop -skipbindings \
		-trimpath -ldflags '$(DESKTOP_LDFLAGS)' -clean -f
	cp cmd/skein-desktop/build/bin/skein-desktop bin/skein-desktop
	@ls -lh bin/skein-desktop

## test: run the full Go test suite with the race detector
test:
	go test -race -count=1 $(PKG)

## test-short: skip the slow memory-ceiling and integration tests
test-short:
	go test -short -count=1 $(PKG)

## bench: run the streaming memory benchmark
bench:
	go test -run '^$$' -bench . -benchmem ./internal/uploads/... ./internal/shard/...

## lint: gofmt check, go vet and golangci-lint, in BOTH build configurations
lint:
	@gofmt -l . | grep -v '^web/' | grep -v '^third_party/' | (! grep .) || (echo "gofmt needed on the files above"; exit 1)
	go vet $(PKG)
	go vet -tags desktop $(PKG)
	$(GOBIN)/golangci-lint run
	$(GOBIN)/golangci-lint run --build-tags desktop

## sqlc: regenerate internal/db/gen from queries (never hand-edit the output)
sqlc:
	$(GOBIN)/sqlc generate

## migrate: apply migrations against SKEIN_DATABASE_URL
migrate:
	@$(LOADENV); $(SHOWDB); \
	$(GOBIN)/goose -dir internal/db/migrations postgres "$$SKEIN_DATABASE_URL" up

## migrate-down: roll back one migration
migrate-down:
	@$(LOADENV); $(SHOWDB); \
	$(GOBIN)/goose -dir internal/db/migrations postgres "$$SKEIN_DATABASE_URL" down

## migrate-status: show migration state
migrate-status:
	@$(LOADENV); $(SHOWDB); \
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

## backup: dump the database (the master key alone cannot restore your files)
backup:
	@$(LOADENV); $(SHOWDB); \
	mkdir -p backups; \
	ver=$$(psql "$$SKEIN_DATABASE_URL" -qAt \
	        -c 'SELECT max(version_id) FROM goose_db_version' 2>/dev/null); \
	[ -n "$$ver" ] || ver=unknown; \
	out="backups/skein-$$(date +%Y%m%d-%H%M%S)-v$$ver.sql.gz"; \
	tmp="$$out.partial"; \
	: 'pg_dump | gzip hides pg_dump failure: sh reports the exit status of'; \
	: 'the LAST command, so a dump that never connected still left a valid'; \
	: 'empty .gz and printed "wrote ...". A backup that fails silently is'; \
	: 'worse than one that fails loudly. Capture pg_dump own status, and'; \
	: 'write to .partial so a failed run cannot leave a file that looks'; \
	: 'like a backup. Reproduced 2026-08-01.'; \
	if pg_dump --no-owner --no-privileges "$$SKEIN_DATABASE_URL" > "$$tmp.sql"; then \
	  gzip -c "$$tmp.sql" > "$$out" && rm -f "$$tmp.sql"; \
	else \
	  rm -f "$$tmp.sql" "$$out"; \
	  echo "backup FAILED: pg_dump could not dump $$SKEIN_DATABASE_URL; no file written" >&2; \
	  exit 1; \
	fi; \
	if [ ! -s "$$out" ]; then \
	  rm -f "$$out"; echo "backup FAILED: dump was empty; no file written" >&2; exit 1; \
	fi; \
	echo "wrote $$out ($$(du -h "$$out" | cut -f1))"; \
	echo "schema version: $$ver — restoring this dump lands at v$$ver, then run 'make migrate'"; \
	echo; \
	echo "This dump is only half a backup. It records which shard belongs to"; \
	echo "which file; SKEIN_MASTER_KEY decrypts their contents. Restoring needs"; \
	echo "both. Keep the key somewhere this dump is not."; \
	echo "Restore is a three-step procedure, not one pipe. See README \"Backups\"."

## backup-memory: snapshot PLAN/Memory.md (gitignored, so git will not save it)
backup-memory:
	@scripts/backup-memory.sh

## clean: remove build output
clean:
	rm -rf bin $(WEB_DIST)/* web/node_modules

.PHONY: help run build build-go desktop test test-short bench lint sqlc migrate \
        migrate-down migrate-status web web-dev dev-db dev-db-down tools backup clean
