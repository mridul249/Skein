# Skein

Your free cloud accounts, pooled, striped and encrypted.

Five Google accounts at 15 GB each is 75 GB of real capacity that behaves like
five separate 15 GB drives. You have to remember which account holds what, and
a 30 GB archive cannot be stored at all — even though 75 GB is free — because
no single account can hold it.

Skein pools them, splits files across them when no one account fits, and
encrypts everything before it leaves your machine. One static binary, frontend
included.

```
skein status
──────────────────────────────────────────────────────
 ▓▓▓▓▓▓▓▓▒▒▒▒▒░░░░░  38.2 / 75 GB          4 drives

 archive.tar.zst   28.4 GB   ●●●    3 shards, 3 drives
 video.mkv          4.1 GB   ●      1 drive
 notes.md            12 KB   ●      1 drive
```

## Why not rclone

`rclone union` is the honest answer to "combine my drives" and this does not
try to beat it at that. The differences that matter:

| | Pooling | Large-file striping | Encryption at rest | Web UI | Deploy |
|---|---|---|---|---|---|
| rclone `union` | Yes | No | Yes (`crypt`) | No | Binary + config |
| 9drive | Yes | No | No | Yes | MySQL + 2 npm + Compose |
| **Skein** | Yes | **Yes** | **Yes, default** | Yes | **One binary** |

Striping is the thing neither alternative does. Encryption is on by default
rather than an opt-in overlay. And deployment is `scp` plus a Postgres URL.

## The memory claim

File size never influences memory. A 2 GB upload on a 512 MB VPS is a
supported case, not a caveat, and it is enforced by a test rather than
asserted in a README:

```
$ go test -run TestUploadHoldsConstantMemory -v ./internal/files/
    uploaded 2147483648 bytes; peak HeapAlloc 677368 bytes (0.6 MiB), ceiling 150 MiB
--- PASS: TestUploadHoldsConstantMemory (1.08s)
```

2 GiB streamed through the real upload path, 0.6 MiB of heap. The whole path
is `io.Reader` composition over one fixed 256 KiB buffer:

```
request body ─► TeeReader (SHA-256) ─► StreamEncrypter ─► ShardWriter ─► provider
```

`Phases.md` says that test may never be relaxed. If a change breaks it, the
change reintroduced buffering; raising the number is not a fix.

## How it works

**Striping.** When no single drive can hold a file, the planner fills greedily
in 256 MiB shards, reserving each shard's capacity before a byte is written. A
manifest row records where each shard is and which plaintext byte range it
covers. A file whose manifest has a gap is unreadable and says so — it never
returns the shards that do exist as though they were the whole file.

**Quota reservation.** Free space is `total - used - reserved`, and a
reservation is one conditional `UPDATE`:

```sql
UPDATE storage_accounts
   SET reserved_bytes = reserved_bytes + $1
 WHERE id = $2
   AND (total_bytes - used_bytes - reserved_bytes) >= $1
RETURNING id;
```

The `WHERE` is the check and the `SET` is the commit, so two concurrent
uploads cannot both be told the same bytes are free. Zero rows means somebody
else got there first, and the planner tries the next drive. A janitor releases
reservations left behind by a killed process.

**Encryption.** AES-256-GCM in 64 KiB frames, one AEAD message per frame. A
whole-file GCM message would mean buffering gigabytes before a single byte
could be trusted. Framing keeps it streaming, localises corruption, and leaves
frame *i* at a computable offset — which is what makes a range request over an
encrypted striped file fetch 64 KiB rather than 256 MiB, and therefore what
makes scrubbing a striped video work.

Keys derive from one master key via HKDF-SHA256, salted per file. Every
ciphertext carries a version byte and a key id, so the format can change and
the key can rotate.

**`drive.file` scope only.** Skein can see only the files it created. It
cannot index, read or touch anything already in your Drive. That is a
deliberate privacy decision, and it also keeps the project out of Google's
restricted-scope verification review.

> **The top FAQ, answered up front:** Skein cannot show you files already in
> your Google Drive. Not a bug, not a missing feature — it is the point of the
> narrow scope.

## Running it

You need Go 1.25+, Node 20+, and a Postgres 15+.

```bash
git clone <this repo> && cd skein
cp .env.example .env

# 32 random bytes. Losing this makes every stored file unreadable.
openssl rand -base64 32   # -> SKEIN_MASTER_KEY
openssl rand -base64 48   # -> SKEIN_JWT_SECRET

make dev-db      # Postgres on :5433, development only
make web         # build the frontend into internal/web/dist
make build       # one binary at bin/skein
./bin/skein
```

`make run` does the same without the build step.

### Connecting a Google account

Google requires you to supply your own OAuth client — there is no shared one,
by design.

1. [console.cloud.google.com](https://console.cloud.google.com) → new project
2. **APIs & Services → Library** → enable **Google Drive API**
3. **OAuth consent screen** → External → add yourself as a test user
4. **Credentials → Create credentials → OAuth client ID → Web application**
5. Authorised redirect URI: `http://localhost:8080/api/accounts/google/callback`
   (must match `SKEIN_GOOGLE_REDIRECT_URL` exactly)
6. Put the client ID and secret in `.env`, restart, and click **Connect
   Google Drive**

The consent screen stays in Testing mode, which caps at 100 test users. For a
single-tenant self-hosted tool that is not a limit you will reach.

## Configuration

Every variable is documented in [`.env.example`](.env.example). The ones that
matter:

| Variable | Notes |
|---|---|
| `SKEIN_MASTER_KEY` | 32 bytes, base64. **Back it up — and the database too, see [Backups](#backups).** |
| `SKEIN_JWT_SECRET` | Separate from the master key so either can rotate alone |
| `SKEIN_DATABASE_URL` | Postgres 15+ |
| `SKEIN_TRUSTED_PROXIES` | CIDRs. `X-Forwarded-For` is ignored from anywhere else |
| `SKEIN_SHARD_SIZE_BYTES` | 256 MiB default |
| `SKEIN_MAX_UPLOAD_BYTES` | 100 GiB default |
| `SKEIN_PREVIEW_ORIGIN` | Serve inline previews from a separate origin. Recommended |

Configuration is validated at boot and every problem is reported at once, so a
typo is a startup error rather than a surprise on someone's first upload.

## Security posture

- Passwords: argon2id, `t=3, m=64MB, p=4`
- Refresh tokens: 32 random bytes, single use, stored only as SHA-256. Every
  refresh rotates; presenting a spent token revokes the whole family
- Access tokens: 15 minutes, `sub` and `sid` only, held in a JS variable —
  never `localStorage`, and there is a test that greps the shipped bundle
- OAuth tokens at rest: versioned AES-256-GCM envelope, salted per user
- File content served with a twelve-entry inline allowlist, `nosniff`, and
  `default-src 'none'; sandbox`. `text/html`, `*+xml` and SVG are never inline
- No object is ever granted provider-side public permissions

## Backups

**Two things have to survive, not one.**

The master key decrypts shard contents. The database records which shard is
part of which file, in what order, on which drive. With the key but no
database, your shards are unlabelled encrypted blobs and nothing can
reassemble them. With the database but no key, the manifest is intact and the
contents are unreadable.

```bash
make backup            # -> backups/skein-<timestamp>-v<schema-version>.sql.gz
```

Keep the key somewhere else entirely — a password manager, not the same disk.

### Restoring

**Three steps, not one pipe.** This procedure has been tested end to end; the
one-line version that used to be here had not, and it does not work.

```bash
# 1. Create an EMPTY target database. This needs a superuser or a role holding
#    CREATEDB. The skein role has neither, deliberately — the application never
#    needs to create a database, so it should not be able to.
createdb -U postgres skein_restored

# 2. Restore. ON_ERROR_STOP=1 is not optional: psql continues past errors by
#    default, so without it a restore can report success while having silently
#    dropped objects.
gunzip -c backups/skein-20260730-172157-v7.sql.gz \
  | psql -v ON_ERROR_STOP=1 -U postgres skein_restored

# 3. Migrate. The dump lands at whatever schema version it was taken at — the
#    version in its filename. If the binary has moved on since, the restore is
#    behind until this runs.
#
#    Call goose directly, NOT `make migrate`. Every migrate target sources .env
#    after the shell's environment, so .env wins and a SKEIN_DATABASE_URL passed
#    on the command line is silently ignored — `make migrate` would migrate your
#    LIVE database, not the restored one.
goose -dir internal/db/migrations postgres \
  'postgres://postgres@localhost/skein_restored?sslmode=disable' up
```

**Restore into an empty database, never over a live one.** The dump contains
`CREATE TABLE`, so layering it over existing tables fails on every one of them —
and with `ON_ERROR_STOP=1` the restore aborts partway. There is no in-place
restore.

**Step 3 is the one people skip.** A restore that stops at the dump's schema
version looks complete and starts a binary expecting newer tables. The
`-v<version>` suffix on the filename exists so you can tell what you are holding
without decompressing it; a version recorded only in a log line gets separated
from the file it describes.

**The dump needs `citext` available on the target.** `CREATE EXTENSION IF NOT
EXISTS citext` is the first DDL in the file — everything before it is `SET`
boilerplate that cannot fail — so this is the first thing that breaks a restore,
and it breaks it at the very start. On Debian and Ubuntu the extension lives in
`postgresql-contrib`, which minimal container images often omit. It is a
*trusted* extension on PostgreSQL 13 and later, so a non-superuser database owner
can install it, but only if the files are on disk. (The SQLite path in Phase 7
Task 3.4 maps `CITEXT` to `TEXT COLLATE NOCASE` and sidesteps this entirely.)

**Restore with a `psql` at least as new as the `pg_dump` that wrote the file.**
These dumps begin with a `\restrict` meta-command, emitted by PostgreSQL 18's
`pg_dump`. An older `psql` does not know it and will fail on line one. Not tested
here — only PostgreSQL 18 is installed on the machine this was verified on — but
worth knowing before restoring onto an older host.

What has been verified, on a throwaway cluster with no `skein` role present: the
dump carries schema, data, extensions, sequences with correct `setval` values,
and the `goose_db_version` table with its rows — so a restore does **not** re-run
every migration from scratch. Restored object counts match the live database
exactly: 13 tables, 38 indexes, and every check, foreign-key, unique and
primary-key constraint. Because `pg_dump` runs with `--no-owner
--no-privileges`, the dump restores onto a machine that has never heard of the
`skein` role.

> Making the drives self-describing — so the database becomes a rebuildable
> cache rather than a single point of failure — is Phase 7 Task 5 and is not
> built yet. Until it lands, the database is load-bearing. Back it up.

## Development

```bash
make test        # full suite with -race
make test-short  # skips the 2 GiB ceiling test
make lint        # gofmt, go vet, golangci-lint
make sqlc        # regenerate internal/db/gen (never hand-edit it)
make bench       # streaming throughput with allocations
```

`internal/db/gen` is generated output. `internal/web/dist` is build output.
Neither is edited by hand.

## Status

v0.2.0. Single-tenant by design: no multi-tenancy, no team sharing, no SSO,
no mobile apps, no sync client. Those are not on a roadmap, they are
[non-goals](PLAN/PRD.md).

Not yet built: share links, resumable uploads surviving a refresh, S3/R2/B2
backends, WebDAV, content-addressed dedup.

## Credits

The idea of a web UI over pooled Drive accounts comes from
[9drive](https://github.com/topics/9drive). Skein is a different codebase with
different goals — striping, encryption by default, and one binary.

## Licence

MIT.
