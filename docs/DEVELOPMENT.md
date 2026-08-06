<samp>

# Development

## Prerequisites

- Go 1.25+
- Node 18+ and npm
- PostgreSQL (via `make dev-db`, or your own instance)
- `golangci-lint`, `goose`, `sqlc` - `make tools` installs the pinned
  versions
- For desktop work: `libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev`,
  `wails` - see [INSTALL.md](docs/INSTALL.md#desktop) for the exact commands

## First-time setup

```bash
git clone <this repo> && cd skein
cp .env.example .env
openssl rand -base64 32   # -> SKEIN_MASTER_KEY in .env
openssl rand -base64 48   # -> SKEIN_JWT_SECRET in .env
make dev-db
make tools
```

## Running

```bash
make run          # server, .env loaded, no frontend rebuild
make web-dev       # Vite dev server, hot reload, proxies API calls
```

For frontend work, run both in separate terminals: `make run` for the API,
`make web-dev` for the UI with fast refresh. `web/vite.config.ts` proxies
`/api` to the Go server.

## Building

```bash
make build      # frontend + Go, -> bin/skein
make build-go   # Go only, skips the frontend rebuild
make desktop    # -> bin/skein-desktop, requires cgo (see INSTALL.md)
```

`make build` always builds the frontend first - if you're iterating on Go
code only and already have a fresh `internal/web/dist`, `make build-go` is
faster.

## Testing

### Go

```bash
make test         # full suite, -race
make test-short    # skips the slow memory-ceiling / integration tests
make bench         # the streaming memory benchmark
```

Every package should be green. `TestDisconnectThenReconnectRestoresAccess`
in `internal/files` used to be an expected failure - a committed
reproduction of issue #19, where disconnecting a drive orphaned its shards
permanently instead of making them temporarily unreachable. That issue is
fixed: `Disconnect` soft deletes (status `disabled`, credentials cleared)
so the account row id survives and reconnecting the same Google identity
re-links every shard. The test now asserts the fix.

### Frontend

```bash
cd web
npm run typecheck    # tsc -b --force
npm test              # logic tests: tsc + node --test, no framework
npm run test:browser  # component/layout tests: real headless Chrome over CDP
```

**No test framework** - not vitest, not testing-library, not jsdom. This
was a deliberate choice (project session log, 2026-07-31): jsdom computes
no layout, and every real defect found in that session was a layout
defect - a flex `min-width` floor overflowing a dialog, a scroll container
shifting a trigger 10px out from under the pointer, an `aria-describedby`
on the wrong element. A DOM approximation would have reported all of them
passing. `npm test` covers logic (`web/src/lib/*.ts`) with the Node built-in
test runner; `npm run test:browser` drives the real components against a
real Vite dev server over the Chrome DevTools Protocol and asserts on
computed values only - `getComputedStyle`, `getBoundingClientRect`, real
Tab keypresses - never markup shape.

`test:browser` skips with exit 0 and a clear message if no Chrome is found
on the machine; it never gates `make build`.

## Linting

```bash
make lint   # gofmt check, go vet, golangci-lint
```

`golangci-lint`'s config (`.golangci.yml`) excludes `third_party/` (the
vendored Wails fork - not ours to lint or reformat) and `internal/db/gen/`
(sqlc output - never hand-edited).

## Database

```bash
make migrate          # apply pending migrations against SKEIN_DATABASE_URL
make migrate-down      # roll back one
make migrate-status    # show current version
make sqlc               # regenerate internal/db/gen from internal/db/queries/*.sql
```

Migrations live in `internal/db/migrations/`, embedded in the binary and
applied automatically on every boot - the `make migrate*` targets are for
operating on a database the binary isn't currently running against (a
restore, a scratch environment, CI). Never hand-edit `internal/db/gen/` -
change the `.sql` under `internal/db/queries/` and run `make sqlc`.

All five `make` targets that touch a database (`run`, `migrate`,
`migrate-down`, `migrate-status`, `backup`) load `.env` and then let an
explicitly-set shell environment variable override it, printing which
source won and which host/database (never the password) they're about to
touch - see the `LOADENV`/`SHOWDB` helpers in the `Makefile` if you're
adding a new target that touches the database.

## Contributing

- **`gofmt`, `go vet`, and `golangci-lint run` must pass before any commit**
  (`make lint`). No exceptions carried by a build tag that silences rather
  than fixes.
- **Every bug fix ships with a regression test in the same change.**
- **No test depends on the network or on execution order.**
- **Conventional Commits**, subject line only, imperative mood, under 72
  characters: `feat|fix|refactor|test|docs|chore(scope): subject`. The
  commit body (if any) explains *why*, not *what* - the diff already shows
  what.
- **Never commit a failing build.** `make lint test` before every commit.
- Exported identifiers get doc comments starting with the identifier's own
  name. Context is always the first parameter, named `ctx`, never stored on
  a struct. Interfaces are declared by the consumer, not the implementer.


</samp>