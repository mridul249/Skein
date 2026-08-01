-- SQLite dialect of internal/db/queries/accounts.sql. Same contracts; the
-- differences are noted where they exist. now() becomes a bound parameter
-- throughout, as in sessions.sql.
--
-- KEEP THIS FILE ASCII-ONLY -- see the note at the top of sessions.sql. sqlc
-- v1.31.1 miscounts multi-byte characters in comments and truncates the query
-- that follows.

-- name: CreateConnectedAccount :one
INSERT INTO connected_accounts (
    id, user_id, kind, provider_account_id, email, display_name,
    access_token_enc, refresh_token_enc, token_expires_at, ordinal,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- UpdateAccountTokens refreshes the stored credentials for an account that is
-- already linked. The user_id predicate is not decoration: it is what stops a
-- callback from writing tokens into somebody else's row.
--
-- Setting status back to 'active' is what makes reconnecting a disconnected
-- drive work: Disconnect soft deletes by setting status = 'disabled' and
-- clearing the tokens (issue #19), and this is the write that undoes both
-- while keeping the row id -- and therefore every shard's link to it -- intact.
--
-- name: UpdateAccountTokens :one
UPDATE connected_accounts
   SET access_token_enc  = ?3,
       refresh_token_enc = COALESCE(?4, refresh_token_enc),
       token_expires_at  = ?5,
       email             = ?6,
       display_name      = ?7,
       status            = 'active',
       last_error        = '',
       updated_at        = ?8
 WHERE id = ?1
   AND user_id = ?2
RETURNING *;

-- name: GetConnectedAccount :one
SELECT * FROM connected_accounts WHERE id = ? AND user_id = ?;

-- name: GetConnectedAccountByProviderID :one
SELECT * FROM connected_accounts
 WHERE user_id = ? AND kind = ? AND provider_account_id = ?;

-- name: ListConnectedAccounts :many
SELECT * FROM connected_accounts
 WHERE user_id = ?
 ORDER BY ordinal, created_at;

-- ListActiveAccountsForSync feeds the background quota ticker, which is not
-- acting on behalf of a request and so has no user to scope by.
--
-- name: ListActiveAccountsForSync :many
SELECT * FROM connected_accounts
 WHERE status = 'active'
 ORDER BY created_at;

-- name: NextAccountOrdinal :one
SELECT COALESCE(MAX(ordinal), 0) + 1 FROM connected_accounts WHERE user_id = ?;

-- SetAppFolderID records where this account's shards live at the provider.
--
-- The app_folder_id IS NULL predicate makes the write single-shot: if another
-- process established a folder first, this one returns no row and the caller
-- re-reads rather than overwriting a good id with a duplicate folder.
--
-- name: SetAppFolderID :one
UPDATE connected_accounts
   SET app_folder_id = ?2,
       updated_at    = ?3
 WHERE id = ?1
   AND app_folder_id IS NULL
RETURNING app_folder_id;

-- name: GetAppFolderID :one
SELECT app_folder_id FROM connected_accounts WHERE id = ?;

-- name: SetAccountStatus :exec
UPDATE connected_accounts
   SET status     = ?2,
       last_error = ?3,
       updated_at = ?4
 WHERE id = ?1;

-- ClearAccountTokens wipes stored credentials without touching the row, so a
-- disconnected account stops being usable while its id -- and therefore every
-- file_shards.connected_account_id pointing at it -- survives. The Postgres
-- version casts an empty string to bytea; here the caller passes an empty
-- blob, since access_token_enc is NOT NULL.
--
-- name: ClearAccountTokens :exec
UPDATE connected_accounts
   SET access_token_enc  = ?2,
       refresh_token_enc = NULL,
       token_expires_at  = NULL,
       updated_at        = ?3
 WHERE id = ?1;

-- DeleteConnectedAccount is deliberately NOT used to disconnect a drive: the
-- ON DELETE SET NULL on file_shards.connected_account_id would orphan every
-- shard the drive held, unrecoverably (known issue #19). Disconnect soft
-- deletes via SetAccountStatus + ClearAccountTokens instead. This remains for
-- true row removal, e.g. hard-deleting a user's data.
--
-- name: DeleteConnectedAccount :execrows
DELETE FROM connected_accounts WHERE id = ? AND user_id = ?;

-- name: UpsertStorageAccount :exec
INSERT INTO storage_accounts (connected_account_id, total_bytes, used_bytes, last_synced_at)
VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (connected_account_id) DO UPDATE
   SET total_bytes    = excluded.total_bytes,
       used_bytes     = excluded.used_bytes,
       last_synced_at = excluded.last_synced_at,
       last_error     = '';

-- name: SetStorageAccountError :exec
INSERT INTO storage_accounts (connected_account_id, last_error)
VALUES (?1, ?2)
ON CONFLICT (connected_account_id) DO UPDATE
   SET last_error = excluded.last_error;

-- ListStorageAccounts returns capacity joined to the owning account. It is one
-- query rather than a list plus a lookup per row, per Rules.md section 2.12.
--
-- name: ListStorageAccounts :many
SELECT ca.id,
       ca.kind,
       ca.email,
       ca.display_name,
       ca.status,
       ca.ordinal,
       ca.last_error,
       COALESCE(sa.total_bytes, 0) AS total_bytes,
       COALESCE(sa.used_bytes, 0) AS used_bytes,
       COALESCE(sa.reserved_bytes, 0) AS reserved_bytes,
       sa.last_synced_at
  FROM connected_accounts ca
  LEFT JOIN storage_accounts sa ON sa.connected_account_id = ca.id
 WHERE ca.user_id = ?
 ORDER BY ca.ordinal, ca.created_at;

-- name: CreateOAuthState :exec
INSERT INTO oauth_states (state_hash, user_id, kind, redirect_to, pkce_verifier, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- ConsumeOAuthState is single use by construction: the row is deleted as it is
-- read, so a replayed callback finds nothing. The expiry predicate is in SQL so
-- a stale state cannot be accepted by a clock-skewed application check.
--
-- name: ConsumeOAuthState :one
DELETE FROM oauth_states
 WHERE state_hash = ?1
   AND expires_at > ?2
RETURNING *;

-- name: DeleteExpiredOAuthStates :execrows
DELETE FROM oauth_states WHERE expires_at < ?;

-- --- test support ---------------------------------------------------------
-- Back the inspection helpers the accounts test suite uses (memstore.go has
-- in-memory equivalents). Read-only apart from SetReservedBytes, and no
-- production caller.

-- name: PendingStateCount :one
SELECT COUNT(*) FROM oauth_states;

-- name: StateHashExists :one
SELECT COUNT(*) FROM oauth_states WHERE state_hash = ?;

-- name: PendingVerifiers :many
SELECT COALESCE(pkce_verifier, '') FROM oauth_states;

-- name: SetReservedBytes :exec
INSERT INTO storage_accounts (connected_account_id, reserved_bytes)
VALUES (?1, ?2)
ON CONFLICT (connected_account_id) DO UPDATE
   SET reserved_bytes = excluded.reserved_bytes;
