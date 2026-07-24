-- name: CreateConnectedAccount :one
INSERT INTO connected_accounts (
    id, user_id, kind, provider_account_id, email, display_name,
    access_token_enc, refresh_token_enc, token_expires_at, ordinal
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- UpdateAccountTokens refreshes the stored credentials for an account that is
-- already linked. The user_id predicate is not decoration: it is what stops a
-- callback from writing tokens into somebody else's row.
--
-- name: UpdateAccountTokens :one
UPDATE connected_accounts
   SET access_token_enc  = $3,
       refresh_token_enc = COALESCE($4, refresh_token_enc),
       token_expires_at  = $5,
       email             = $6,
       display_name      = $7,
       status            = 'active',
       last_error        = '',
       updated_at        = now()
 WHERE id = $1
   AND user_id = $2
RETURNING *;

-- name: GetConnectedAccount :one
SELECT * FROM connected_accounts WHERE id = $1 AND user_id = $2;

-- name: GetConnectedAccountByProviderID :one
SELECT * FROM connected_accounts
 WHERE user_id = $1 AND kind = $2 AND provider_account_id = $3;

-- name: ListConnectedAccounts :many
SELECT * FROM connected_accounts
 WHERE user_id = $1
 ORDER BY ordinal, created_at;

-- ListActiveAccountsForSync feeds the background quota ticker, which is not
-- acting on behalf of a request and so has no user to scope by.
--
-- name: ListActiveAccountsForSync :many
SELECT * FROM connected_accounts
 WHERE status = 'active'
 ORDER BY created_at;

-- name: NextAccountOrdinal :one
SELECT COALESCE(MAX(ordinal), 0) + 1 FROM connected_accounts WHERE user_id = $1;

-- name: SetAccountStatus :exec
UPDATE connected_accounts
   SET status     = $2,
       last_error = $3,
       updated_at = now()
 WHERE id = $1;

-- name: DeleteConnectedAccount :execrows
DELETE FROM connected_accounts WHERE id = $1 AND user_id = $2;

-- name: UpsertStorageAccount :exec
INSERT INTO storage_accounts (connected_account_id, total_bytes, used_bytes, last_synced_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (connected_account_id) DO UPDATE
   SET total_bytes    = EXCLUDED.total_bytes,
       used_bytes     = EXCLUDED.used_bytes,
       last_synced_at = now(),
       last_error     = '';

-- name: SetStorageAccountError :exec
INSERT INTO storage_accounts (connected_account_id, last_error)
VALUES ($1, $2)
ON CONFLICT (connected_account_id) DO UPDATE
   SET last_error = EXCLUDED.last_error;

-- ListStorageAccounts returns capacity joined to the owning account. It is one
-- query rather than a list plus a lookup per row, per Rules.md §2.12.
--
-- name: ListStorageAccounts :many
SELECT ca.id,
       ca.kind,
       ca.email,
       ca.display_name,
       ca.status,
       ca.ordinal,
       ca.last_error,
       COALESCE(sa.total_bytes, 0)    AS total_bytes,
       COALESCE(sa.used_bytes, 0)     AS used_bytes,
       COALESCE(sa.reserved_bytes, 0) AS reserved_bytes,
       sa.last_synced_at
  FROM connected_accounts ca
  LEFT JOIN storage_accounts sa ON sa.connected_account_id = ca.id
 WHERE ca.user_id = $1
 ORDER BY ca.ordinal, ca.created_at;

-- name: CreateOAuthState :exec
INSERT INTO oauth_states (state_hash, user_id, kind, redirect_to, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- ConsumeOAuthState is single use by construction: the row is deleted as it is
-- read, so a replayed callback finds nothing. The expiry predicate is in SQL so
-- a stale state cannot be accepted by a clock-skewed application check.
--
-- name: ConsumeOAuthState :one
DELETE FROM oauth_states
 WHERE state_hash = $1
   AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredOAuthStates :execrows
DELETE FROM oauth_states WHERE expires_at < now();
