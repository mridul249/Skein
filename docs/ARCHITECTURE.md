# Architecture

A structural reference: what exists, where, and how the pieces fit. For the
*why* behind specific decisions, see `docs/architecture-notes.md` — this
file covers the shape, that one covers the reasoning that produced it.

## Stack

| Layer | Choice |
|---|---|
| Language | Go 1.25 |
| Router | `chi/v5` |
| Database | PostgreSQL (`pgx/v5`) |
| Queries | `sqlc` — generated, no ORM, no hidden N+1 |
| Migrations | `goose`, embedded in the binary, run automatically on boot |
| Frontend | React 18 + Vite + TypeScript + Tailwind, embedded via `//go:embed` |
| Desktop shell | Wails v2, a small local fork — see below |

**SQLite is deferred, not rejected.** It would remove the last external
dependency for a self-hoster, but the quota reservation scheme relies on
row-level locking semantics (`SELECT … FOR UPDATE` / atomic conditional
`UPDATE`) that need re-verification under SQLite's single-writer model. It
is gated on an owner-written rewrite of `internal/router/reserve.go` and
`plan.go`.

## Two binaries, one server

```
cmd/skein         → skein serve, headless, CGO_ENABLED=0, static binary
cmd/skein-desktop → native window, requires cgo, same server in-process
```

`internal/app.Build` wires config, the database pool, every domain service,
and the HTTP handler once; both binaries call it, differing only in how
they open a listener and what triggers shutdown. `cmd/skein` reacts to
`SIGTERM`/`SIGINT`; `cmd/skein-desktop` reacts to the window closing.

**Both talk to PostgreSQL.** The desktop build does not embed SQLite or any
other local database — see the SQLite note above for why, and
[INSTALL.md](INSTALL.md) for what that means for setup.

### Desktop: a real HTTP origin, not a custom scheme

`skein-desktop` binds `127.0.0.1:0`, lets the OS assign a port, and points
the window **directly at that real `http://` address** — not at Wails'
default `wails://` custom-scheme bridge. This needed a small vendored fork
of Wails (`third_party/wails-v2.13.0`, `replace`d in `go.mod`; see
`third_party/wails-v2.13.0/PATCH.md` for the full diff and reasoning)
because Wails has no public API to redirect the window's start URL.

The reason it's worth a fork: Wails' custom-scheme bridge builds every
request with `context.Background()`, with nothing downstream ever
replacing it with a context tied to the request's real lifecycle. Practical
consequence, found from a real bug report — a cancelled upload kept writing
shards and holding quota reservations until the whole process died, because
`r.Context()` inside the handler never actually cancelled. The bridge also
has no working download handling on Linux (confirmed by grepping Wails'
entire Linux frontend package for `download`: zero hits), so a
`<a download>` click had nowhere to report progress or completion. Pointing
the window at a real HTTP origin puts every request through Go's ordinary
`net/http` server and WebKit's ordinary HTTP loader instead, and both
problems disappear because they were never problems with Skein's own code.

### Desktop shutdown

`OnShutdown` (fired when the window closes) must return in well under a
second — it runs on the UI thread, and Wails does not finish closing the
window until it returns. The actual drain (HTTP server shutdown, background
workers, database pool close) runs in a detached goroutine with its own
fixed budget, independent of `SKEIN_SHUTDOWN_TIMEOUT` (which governs the
headless server's SIGTERM path, where nothing is watching the process
visibly hang). If the drain exceeds its budget, the process exits directly
rather than lingering as an orphan after its window is already gone.

### Desktop OAuth

RFC 8252: a Desktop app OAuth client (no client secret — the client ID is
not a secret for this client type), PKCE mandatory
(`oauth2.S256ChallengeOption`/`VerifierOption`), and the authorization
request opens the system's default browser rather than the embedded
webview. `internal/desktopoauth` opens an ephemeral loopback listener,
builds the auth URL, launches the browser, and blocks on exactly one
callback — bounded by `accounts.OAuthStateTTL` (10 minutes). See
[SECURITY.md](SECURITY.md#pkce-desktop-oauth) for the threat-model reasoning and
[INSTALL.md](INSTALL.md#4-connecting-a-google-drive) for the setup steps
this removes.

## Package layout

```
cmd/
  skein/            entrypoint: signals, config, listen
  skein-desktop/     entrypoint: window, OAuth wiring, shutdown
internal/
  app/               shared server wiring (Build), used by both cmd/ entrypoints
  config/            env parsing, fail-fast validation
  db/                pool, embedded goose migrations, sqlc output (gen/ — never hand-edit)
  auth/              argon2id, JWT access tokens, opaque refresh + family revocation
  crypto/            HKDF keyring, versioned envelope, framed streaming AEAD
  storage/           the Backend interface; gdrive and local implementations
  router/            atomic quota reservation, the shard planner, routing policies
  files/             upload/download services, folders, the shard manifest
  accounts/          OAuth linking (web flow), token sealing, quota sync
  desktopoauth/      desktop-only: loopback listener, PKCE connector — never imported by cmd/skein
  worker/            periodic background jobs with per-run panic recovery
  httpapi/           chi router, middleware chain, handlers
  web/               go:embed of the built frontend
web/                 frontend source (React/Vite/TypeScript)
third_party/         the vendored Wails fork
```

`internal/desktopoauth` is deliberately excluded from `cmd/skein`'s
dependency graph — it imports `github.com/pkg/browser` and runs its own
HTTP listener, neither of which the headless server has any use for.
Verify with `go list -deps ./cmd/skein | grep desktopoauth` (should be
empty) whenever touching this boundary.

## The `Backend` interface

Everything above this line is provider-agnostic:

```go
type Backend interface {
    Put(ctx context.Context, r io.Reader, o ObjectSpec) (ObjectRef, error)
    Get(ctx context.Context, ref ObjectRef, rng *ByteRange) (io.ReadCloser, int64, error)
    Delete(ctx context.Context, ref ObjectRef) error
    Quota(ctx context.Context) (Quota, error)
    Kind() BackendKind
}
```

`storage/gdrive` implements it over Drive's resumable upload protocol.
`storage/local` implements it over the filesystem and is what the test
suite runs against — no network required for `go test ./...`.

## Upload path

```
POST /api/uploads (multipart)
  → parse headers only
  → plan shards (below)
  → reserve quota atomically
  → open resumable session(s) with the provider
  → stream body: TeeReader (SHA-256) → StreamEncrypter (AEAD) → ShardWriter → io.Copy
  → verify bytes received == declared size
  → commit the shard manifest
  → release the reservation
```

Every byte crosses a fixed 256 KiB buffer via `io.Copy`; nothing grows with
file size. `files.TestUploadHoldsConstantMemory` asserts this directly: a
2 GiB upload measured at 0.6 MiB peak heap against a 150 MB ceiling — a
build-breaking test, not a design aspiration.

## Quota reservation

Reserved atomically in one statement, never read-then-write:

```sql
UPDATE storage_accounts
   SET reserved_bytes = reserved_bytes + $1
 WHERE id = $2
   AND (total_bytes - used_bytes - reserved_bytes) >= $1
RETURNING id;
```

Zero rows returned means another concurrent reservation already claimed the
space; the planner tries the next candidate account. Reservations carry
`expires_at`; a background janitor (`SKEIN_RECLAIM_EVERY`) releases stale
ones so a crashed or abandoned upload doesn't strand capacity.

## Striping

A file too large for any single connected account splits across several.
The planner produces an ordered `[{account, index, size}]` given the file
size and each candidate account's free space; the routing policy
(`SKEIN_ROUTING_POLICY`) decides which accounts are candidates when more
than one has room. The writer rolls over to the next shard's session at
each boundary transparently — nothing upstream of it knows striping
happened. The reader does the inverse: given a byte range, it computes
which shards intersect it and returns a reader that seeks into the first
and concatenates in order, which is what makes `Range` requests — and
therefore video scrubbing — work on a file spread across three drives.

`file_shards` is the manifest and the source of truth. A file with a
missing or checksum-mismatched shard reports itself unreadable explicitly;
it never returns partial data silently.

## Encryption

- One master key from `SKEIN_MASTER_KEY` (32 bytes).
- Per-file key: `HKDF-SHA256(master, salt=file_id, info="skein-file-v1")`.
- Content: AES-256-GCM in fixed 64 KiB frames, each with a nonce derived
  from `(file_id, frame_index)` — never a whole multi-gigabyte file as one
  GCM message, which would mean buffering the entire ciphertext before a
  single byte could be verified.
- Every ciphertext carries a version byte and key id, so key rotation is
  possible without breaking existing data.
- OAuth tokens use the same envelope format, salted per user.

## Data model — verified against the current schema

`size_bytes` is not one unit across tables. `files.size_bytes` is
**plaintext** (what the user uploaded). `file_shards.size_bytes` is
**ciphertext** (what the provider actually stores — `5 + plain + frames×16`
bytes larger). `file_shards.plain_size_bytes` and `plain_offset` carry the
plaintext-side numbers a range reader needs. Conflating these is a real,
previously-made mistake (known issue #9 in the project's session log) —
whole-file plaintext sums and per-shard provider-object sums are not
interchangeable.

`file_shards.sha256` is over the **plaintext** slice, not the ciphertext —
which is correct for localising corruption to one drive on read, but means
no ciphertext digest exists yet, so nothing can currently verify a provider
object's integrity without downloading and decrypting it first.

### Recovery

**Status: database-dependent today, self-describing drives planned.** An
encrypted sidecar manifest written alongside every file's shards — making
each drive independently reconstructable without the database — is
specified for Phase 7 Task 5.1 and **not implemented**. Until it lands, the
database is the only record of which shard belongs to which file; see
[BACKUP.md](BACKUP.md) for what that means for your backup strategy.

### Current tables

`users`, `sessions`, `security_events` · `connected_accounts`,
`storage_accounts`, `oauth_states` (carries `pkce_verifier`, nullable, for
the desktop flow) · `folders`, `files`, `file_shards` · `quota_reservations`,
`uploads` (schema exists, not yet read by any code path — resumable upload
is not built).

**Not built:** `share_links` (no public share-link feature exists yet,
despite it appearing in early design sketches).

## HTTP surface — verified current, not aspirational

```
POST   /api/auth/register            POST   /api/auth/login
POST   /api/auth/refresh             POST   /api/auth/logout
GET    /api/auth/me

GET    /api/accounts                 POST   /api/accounts/google/connect
GET    /api/accounts/google/callback (server build only — see below)
POST   /api/accounts/{id}/sync       DELETE /api/accounts/{id}
GET    /api/quota

POST   /api/uploads                  # single-shot streaming, multipart
GET    /api/files                    GET    /api/files/{id}
PATCH  /api/files/{id}                DELETE /api/files/{id}?permanent=true
POST   /api/files/{id}/restore       GET    /api/trash
POST   /api/files/{id}/content-url   # mints a short-lived capability URL
GET,HEAD /api/files/{id}/content     # Range-capable; bearer OR capability URL

GET,POST     /api/folders
PATCH,DELETE /api/folders/{id}

GET    /healthz                      GET    /readyz
```

`POST /api/accounts/google/connect` behaves differently by build.
**Server:** returns `{authorize_url}` for the frontend to navigate to; the
browser comes back to `GET /api/accounts/google/callback`, an unauthenticated
route (identity comes from the single-use state row, not a bearer token a
redirect can't carry). **Desktop:** the request itself runs the entire OAuth
round trip server-side — opens the system browser, waits on the loopback
listener, completes the exchange — and blocks until it finishes or fails,
returning the connected drive directly. There is no server-hosted callback
route on the desktop path; the loopback listener *is* the callback route,
for the duration of one attempt.

**Not built:** `/api/uploads/resumable` (chunked/resumable upload — a
single `POST /api/uploads` is the only upload path today) and `/s/:token`
public share links.

## Content delivery: capability URLs, not bearer auth

`POST /api/files/{id}/content-url` mints a stateless, HMAC-signed URL
scoped to one file and one user, valid ~15 minutes. `GET/HEAD
/api/files/{id}/content` accepts either that URL or a normal bearer token.
This exists because a browser-managed download (`<a download>`, so the
transfer streams to disk without ever touching JS-controlled memory) cannot
set an `Authorization` header — see [SECURITY.md](SECURITY.md) for the
threat model this closes and the one it deliberately leaves open.

## Concurrency

- `errgroup.Group` with a bounded `SetLimit` for multi-account quota sync —
  never one unbounded goroutine per account.
- `singleflight.Group` keyed by account id collapses concurrent quota
  refreshes into one provider call.
- Quota sync is never on the upload hot path; a background ticker refreshes
  the cached figure, and the atomic reservation (above) is what actually
  keeps concurrent uploads correct regardless of how stale that cache is.
