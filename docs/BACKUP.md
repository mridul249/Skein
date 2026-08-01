# Backup and disaster recovery

**Two things have to survive, not one.** `SKEIN_MASTER_KEY` decrypts shard
contents. The PostgreSQL database records which shard belongs to which
file, on which drive, in what order. Either alone is useless: the key
without the database is a key with nothing to open; the database without
the key is a map to files you cannot read.

**The manifest architecture that would make a drive self-describing — and
would let you rebuild the database from the drives alone — is planned but
not built yet** (Phase 7 Task 5.1; see [ARCHITECTURE.md](ARCHITECTURE.md)).
Until it lands, the database is a genuine single point of failure for the
shard-to-file mapping, even though it is not one for the file contents
themselves (those are encrypted end to end regardless of what happens to
the database).

## What to back up

1. **`SKEIN_MASTER_KEY`.** A password manager or a secrets vault, not a
   file next to the database dump. If an attacker gets both, they can
   decrypt everything; keeping them apart is the whole point.
2. **The database**, via `make backup`.

## `make backup`

```bash
make backup
```

Writes `backups/skein-<timestamp>-v<schema-version>.sql.gz` — a plain
`pg_dump --no-owner --no-privileges`, gzipped. The schema version in the
filename is the migration version the dump was taken at (`goose_db_version`),
not the current live version, so you know what you're restoring into before
you start.

This is a **logical** dump: portable across PostgreSQL installs, restorable
into a cluster that has never heard of the `skein` role. It is not a
point-in-time / WAL-based backup — if you need continuous recovery rather
than periodic snapshots, put PostgreSQL's own `pg_basebackup`/WAL archiving
in front of this, not instead of it.

Run it on a schedule (cron, systemd timer) pointed at your production
`SKEIN_DATABASE_URL`. `make backup` reads `.env` the same way `make run`
does — see [CONFIGURATION.md](CONFIGURATION.md) if you're driving it from a
different environment than your shell's.

## Restoring

This is a three-step procedure, not one pipe — verified end to end against
a throwaway cluster with no `skein` role present, so these steps are what
actually worked, not what should work in theory.

### 1. Prerequisites on the target cluster

The schema uses the `citext` extension (case-insensitive email uniqueness).
It ships in PostgreSQL's `contrib` package — on Debian/Ubuntu,
`postgresql-contrib` — and the dump's `CREATE EXTENSION IF NOT EXISTS
citext` will fail if the extension files aren't installed on the server,
even with superuser rights on the restore role.

```bash
createdb skein_restore   # or whatever name you're restoring into
```

### 2. Restore the dump

```bash
gunzip -c backups/skein-<timestamp>-v<N>.sql.gz | \
  psql -v ON_ERROR_STOP=1 -d skein_restore
```

`ON_ERROR_STOP=1` is not optional — without it, `psql` keeps going past a
failed statement and you get a database that looks restored but is
silently missing whatever failed. Check the exit code.

This lands the database at schema version `<N>` — the version in the
filename — with `goose_db_version` restored **including its rows**, so the
next step does not re-run every migration from scratch.

### 3. Migrate forward

```bash
SKEIN_DATABASE_URL=postgres://.../skein_restore make migrate
```

Brings the restored database from version `<N>` to whatever `internal/db/migrations`
currently defines. If you're restoring onto the exact version Skein is
currently running, this is a no-op.

### 4. Point Skein at it

Update `SKEIN_DATABASE_URL` (and confirm `SKEIN_MASTER_KEY` matches the key
that encrypted this data — a mismatched key doesn't error at boot, it fails
key-by-key at decrypt time) and start the server.

## What restoring gives you back

Every row: users, sessions (already expired if enough time passed —
everyone signs in again, which is correct), connected accounts, folders,
files, and the shard manifest. What it does **not** restore is anything
that changed on your Google Drive accounts after the dump was taken and
before you restored — the database and the drives can drift apart in that
window, in either direction. There is currently no reconciliation pass that
audits the two against each other (`doctor`, Phase 7 Task 6, not built).

## What backup does not cover

- **Provider-side loss.** If a connected Google account is deleted or its
  files are removed outside Skein, no local backup gets those bytes back —
  Skein stripes and encrypts, it does not replicate. Google's own account
  deletion and file-recovery windows are the only recourse there.
- **A lost master key with no backup.** There is no recovery path. This is
  by design — a recoverable master key is a master key an attacker with
  database access can also recover.
