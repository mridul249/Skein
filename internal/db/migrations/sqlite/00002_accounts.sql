-- +goose Up

-- SQLite mirror of the accounts half of the Postgres schema (00003_accounts.sql
-- plus 00006_app_folder.sql and 00008_oauth_pkce.sql, collapsed: there is no
-- deployed SQLite database to migrate, so the later ALTERs are folded into the
-- table definitions).
--
-- Type mapping rationale is in 00001_auth.sql and is not repeated here. The one
-- addition: INT -> INTEGER for ordinal.
--
-- The CHECK constraints are carried over verbatim, and they are real. Verified
-- 2026-08-01 against modernc.org/sqlite: an out-of-range status is rejected on
-- INSERT and on UPDATE, and the constraint survives in sqlite_master rather
-- than being silently dropped at creation. That matters specifically because
-- the issue #19 fix depends on status = 'disabled' being an enforced value --
-- Disconnect soft deletes by setting it, and a store that accepted any string
-- would let a typo silently disable nothing.
--
-- STRICT tables add a second, independent layer: they reject a value whose type
-- does not match the column, before the CHECK is consulted. Without STRICT,
-- SQLite's type affinity would coerce an integer 42 into the TEXT column as
-- '42' -- which this CHECK would then reject anyway, but only because the
-- allowed set is an explicit list. Do not drop STRICT on the assumption the
-- CHECK alone is enough.
--
-- If a future migration ever rebuilds one of these tables the SQLite way
-- (create new, copy, drop old, rename), the CHECK must be carried forward
-- explicitly. It lives in the CREATE TABLE statement and nothing else restores
-- it.

CREATE TABLE connected_accounts (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind                TEXT NOT NULL,
    -- The provider's own stable identifier for the account. Linking is keyed
    -- on this, never on the email address: an address can change hands, and
    -- linking by email alone is how account takeover happens.
    provider_account_id TEXT NOT NULL,
    email               TEXT NOT NULL COLLATE NOCASE,
    display_name        TEXT NOT NULL DEFAULT '',
    access_token_enc    BLOB NOT NULL,
    refresh_token_enc   BLOB,
    token_expires_at    TEXT,
    status              TEXT NOT NULL DEFAULT 'active',
    last_error          TEXT NOT NULL DEFAULT '',
    -- Position in the account colour ramp and the routing order. Assigned on
    -- connect and stable thereafter, because the colour is the account's
    -- identity across the whole interface.
    ordinal             INTEGER NOT NULL DEFAULT 0,
    -- Where this account's shards live at the provider. NULL means "not yet
    -- established", never "root": shards at Drive root look like junk with
    -- names nobody recognises, and deleting a shard destroys its file.
    app_folder_id       TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,

    CONSTRAINT connected_accounts_kind_check
        CHECK (kind IN ('gdrive', 'local', 's3')),
    CONSTRAINT connected_accounts_status_check
        CHECK (status IN ('active', 'needs_reauth', 'disabled')),
    CONSTRAINT connected_accounts_unique_provider
        UNIQUE (user_id, kind, provider_account_id)
) STRICT;

CREATE INDEX connected_accounts_user_id_idx ON connected_accounts (user_id);

-- Capacity, tracked separately from the account so the quota ticker updates a
-- narrow row rather than contending with token refreshes.
--
-- reserved_bytes is the in-flight commitment made by the atomic reservation in
-- Architecture.md section 5. Free space is total - used - reserved, so two
-- concurrent uploads cannot both be told the same bytes are available.
CREATE TABLE storage_accounts (
    connected_account_id TEXT PRIMARY KEY
        REFERENCES connected_accounts (id) ON DELETE CASCADE,
    total_bytes    INTEGER NOT NULL DEFAULT 0,
    used_bytes     INTEGER NOT NULL DEFAULT 0,
    reserved_bytes INTEGER NOT NULL DEFAULT 0,
    last_synced_at TEXT,
    last_error     TEXT NOT NULL DEFAULT '',

    CONSTRAINT storage_accounts_non_negative
        CHECK (total_bytes >= 0 AND used_bytes >= 0 AND reserved_bytes >= 0)
) STRICT;

-- Pending OAuth authorisations.
--
-- Only the SHA-256 of the state parameter is stored, for the same reason only
-- token hashes are: a database dump must not hand over anything replayable.
-- Rows are single use and short lived; consuming one deletes it.
CREATE TABLE oauth_states (
    state_hash    BLOB PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    redirect_to   TEXT NOT NULL DEFAULT '',
    -- Desktop OAuth uses PKCE: the verifier is read back from server-side
    -- state at the callback, never from the callback's own query string.
    -- NULL for the web flow, which does not use PKCE.
    pkce_verifier TEXT,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
) STRICT;

CREATE INDEX oauth_states_expires_at_idx ON oauth_states (expires_at);

-- +goose Down
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS storage_accounts;
DROP TABLE IF EXISTS connected_accounts;
