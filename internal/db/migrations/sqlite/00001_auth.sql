-- +goose Up

-- SQLite mirror of the auth half of the Postgres schema (00002_auth.sql and
-- 00007_token_families.sql, collapsed: there is no deployed SQLite database to
-- migrate, so the token_families table is created directly instead of being
-- added and backfilled).
--
-- The type mapping, and why each one is what it is:
--
--   UUID        -> TEXT      SQLite has no UUID type. Stored as the canonical
--                            36-char hyphenated string, which is what
--                            uuid.UUID.String() produces and what sqlc's
--                            override parses back. Not BLOB: a text id stays
--                            greppable in a database users can open themselves,
--                            and 16 bytes per row is not the constraint here.
--   CITEXT      -> TEXT COLLATE NOCASE
--                            Postgres gets case-insensitive email uniqueness
--                            from the citext type. SQLite gets it from the
--                            column collation, which applies to the UNIQUE
--                            index as well — so 'A@b.com' and 'a@b.com' still
--                            collide. NOCASE is ASCII-only, which is correct
--                            for the local part of an address and adequate for
--                            the domain; Postgres citext is likewise not doing
--                            full Unicode case folding.
--   TIMESTAMPTZ -> TEXT      RFC 3339 in UTC. SQLite has no date type at all;
--                            the alternatives are INTEGER unix seconds (loses
--                            sub-second precision and readability) or REAL
--                            julian days (loses precision outright). Text
--                            sorts correctly for a fixed-width UTC format,
--                            which is what every comparison here relies on.
--   BYTEA       -> BLOB      Direct equivalent.
--   JSONB       -> TEXT      SQLite's JSON support is functions over text.
--                            Stored as the same JSON the Postgres side stores.
--   INET        -> TEXT      No native type. netip.Addr.String() round trips.
--   BIGSERIAL   -> INTEGER PRIMARY KEY AUTOINCREMENT
--                            INTEGER PRIMARY KEY is SQLite's rowid alias and
--                            already autoincrements; AUTOINCREMENT additionally
--                            guarantees ids are never reused after a delete,
--                            which is what an append-only audit trail wants.
--
-- now() has no SQLite equivalent either. Rather than translate it per query to
-- strftime('%Y-%m-%dT%H:%M:%fZ','now') — easy to get subtly wrong in 30 places,
-- and untestable with a fake clock — every timestamp is passed in by the
-- caller. See internal/db/queries/sqlite/*.sql.

CREATE TABLE users (
    id                TEXT PRIMARY KEY,
    email             TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash     TEXT NOT NULL,
    email_verified_at TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;

-- One row per login. See 00007_token_families.sql for why this table exists:
-- revoking a family has to bind sessions that do not exist yet, which a sweep
-- over the sessions table structurally cannot do.
CREATE TABLE token_families (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    revoked_at TEXT
) STRICT;

CREATE INDEX token_families_user_idx ON token_families (user_id);

-- One row per refresh token ever issued. Rotation inserts a new row and marks
-- the old one used; it never updates a token in place, because that history is
-- what makes reuse detection possible.
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id    TEXT NOT NULL REFERENCES token_families (id),
    prev_id      TEXT REFERENCES sessions (id) ON DELETE SET NULL,
    -- SHA-256 of the opaque refresh token. The token itself is never stored.
    refresh_hash BLOB NOT NULL UNIQUE,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    used_at      TEXT,
    revoked_at   TEXT
) STRICT;

CREATE INDEX sessions_user_id_idx    ON sessions (user_id);
CREATE INDEX sessions_family_id_idx  ON sessions (family_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- Append-only audit trail. Read during incident review; never updated or
-- deleted by application code.
CREATE TABLE security_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    TEXT REFERENCES users (id) ON DELETE SET NULL,
    kind       TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '{}',
    ip         TEXT,
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
) STRICT;

CREATE INDEX security_events_user_id_idx    ON security_events (user_id);
CREATE INDEX security_events_created_at_idx ON security_events (created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS security_events;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS token_families;
DROP TABLE IF EXISTS users;
