-- +goose NO TRANSACTION
--
-- The counterpart of 00009_schema_bundle.sql. The SQLite migrations are an
-- independently-numbered series that collapses later Postgres migrations into
-- initial CREATE TABLEs; this one has a Postgres counterpart of its own
-- because it changes tables that already exist in both.
--
-- WHY "NO TRANSACTION", AND DO NOT REMOVE IT. Widening a CHECK constraint
-- under SQLite requires the 12-step table rebuild (there is no DROP
-- CONSTRAINT), and the rebuild's DROP TABLE files would CASCADE to
-- file_shards, destroying the shard mapping of every file in the database.
-- The guard against that is PRAGMA foreign_keys=off around the rebuild — and
-- a PRAGMA foreign_keys issued INSIDE a transaction is SILENTLY IGNORED.
-- Under goose's default (each migration in a transaction) the PRAGMA would be
-- a no-op, the cascade would fire, and the migration would report success
-- having deleted every shard row.
--
-- Verified by execution, not by reading: with the PRAGMA inside a transaction
-- the child rows were deleted; outside one they survived, and
-- PRAGMA foreign_key_check came back clean.
--
-- The cost of NO TRANSACTION is that a failure part-way leaves the database
-- between states. That is accepted here because the alternative is a
-- migration that succeeds and silently destroys data.

-- +goose Up

-- +goose StatementBegin
PRAGMA foreign_keys=off;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 1. Per-user session epoch (known issue #18). See the Postgres file for the
--    full reasoning: validity state a racing insert INHERITS from the parent
--    it claimed, rather than re-reads at insert time.
-- ---------------------------------------------------------------------------
ALTER TABLE users ADD COLUMN session_epoch INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN epoch INTEGER NOT NULL DEFAULT 1;

-- ---------------------------------------------------------------------------
-- 2 + 3. files.status widened (#42) and files.reconciled_at added.
-- ---------------------------------------------------------------------------
--
-- One rebuild carries both, since the rebuild is the expensive part.
--
-- THE CONSTRAINTS MUST BE RESTATED IN FULL. A rebuild does not carry anything
-- forward implicitly: every CHECK, every FK, every DEFAULT and every index
-- exists only if it is written again here. A rebuild that quietly dropped
-- files_status_check would also accept 'corrupted' and look identical to a
-- correct one from the outside -- which is why the test asserts a GARBAGE
-- value is still refused, not merely that the new values are accepted.

CREATE TABLE files_new (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder_id      TEXT REFERENCES folders (id) ON DELETE SET NULL,
    name           TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,
    declared_mime  TEXT NOT NULL DEFAULT '',
    content_sha256 BLOB,
    is_striped     INTEGER NOT NULL DEFAULT 0,
    is_encrypted   INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'pending',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    deleted_at     TEXT,
    reconciled_at  TEXT,

    CONSTRAINT files_size_non_negative CHECK (size_bytes >= 0),
    CONSTRAINT files_status_check CHECK (status IN
        ('pending', 'ready', 'failed', 'partially_missing', 'corrupted')),
    CONSTRAINT files_name_not_blank CHECK (length(trim(name)) > 0),
    CONSTRAINT files_bool_check CHECK (is_striped IN (0, 1) AND is_encrypted IN (0, 1))
) STRICT;

INSERT INTO files_new (id, user_id, folder_id, name, size_bytes, declared_mime,
                       content_sha256, is_striped, is_encrypted, status,
                       created_at, updated_at, deleted_at, reconciled_at)
SELECT id, user_id, folder_id, name, size_bytes, declared_mime,
       content_sha256, is_striped, is_encrypted, status,
       created_at, updated_at, deleted_at, NULL
  FROM files;

DROP TABLE files;

ALTER TABLE files_new RENAME TO files;

-- Indexes are dropped with the old table and must be recreated by hand.
CREATE INDEX files_listing_idx ON files (user_id, folder_id, deleted_at);
CREATE INDEX files_user_deleted_idx ON files (user_id, deleted_at);
CREATE INDEX files_sha256_idx ON files (user_id, content_sha256) WHERE content_sha256 IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 4. The instance's master key id (known issue #48).
-- ---------------------------------------------------------------------------
CREATE TABLE instance_metadata (
    id         INTEGER PRIMARY KEY,
    key_id     TEXT NOT NULL,
    created_at TEXT NOT NULL,

    CONSTRAINT instance_metadata_singleton CHECK (id = 1)
) STRICT;

-- +goose StatementBegin
PRAGMA foreign_keys=on;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
PRAGMA foreign_keys=off;
-- +goose StatementEnd

DROP TABLE IF EXISTS instance_metadata;

CREATE TABLE files_old (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder_id      TEXT REFERENCES folders (id) ON DELETE SET NULL,
    name           TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,
    declared_mime  TEXT NOT NULL DEFAULT '',
    content_sha256 BLOB,
    is_striped     INTEGER NOT NULL DEFAULT 0,
    is_encrypted   INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'pending',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    deleted_at     TEXT,

    CONSTRAINT files_size_non_negative CHECK (size_bytes >= 0),
    CONSTRAINT files_status_check CHECK (status IN ('pending', 'ready', 'failed')),
    CONSTRAINT files_name_not_blank CHECK (length(trim(name)) > 0),
    CONSTRAINT files_bool_check CHECK (is_striped IN (0, 1) AND is_encrypted IN (0, 1))
) STRICT;

-- Rows carrying a widened status would violate the narrow constraint. The
-- damage is in the drives, not in this column; a reconcile re-derives it.
INSERT INTO files_old (id, user_id, folder_id, name, size_bytes, declared_mime,
                       content_sha256, is_striped, is_encrypted, status,
                       created_at, updated_at, deleted_at)
SELECT id, user_id, folder_id, name, size_bytes, declared_mime,
       content_sha256, is_striped, is_encrypted,
       CASE WHEN status IN ('partially_missing', 'corrupted') THEN 'ready' ELSE status END,
       created_at, updated_at, deleted_at
  FROM files;

DROP TABLE files;

ALTER TABLE files_old RENAME TO files;

CREATE INDEX files_listing_idx ON files (user_id, folder_id, deleted_at);
CREATE INDEX files_user_deleted_idx ON files (user_id, deleted_at);
CREATE INDEX files_sha256_idx ON files (user_id, content_sha256) WHERE content_sha256 IS NOT NULL;

ALTER TABLE sessions DROP COLUMN epoch;
ALTER TABLE users DROP COLUMN session_epoch;

-- +goose StatementBegin
PRAGMA foreign_keys=on;
-- +goose StatementEnd
