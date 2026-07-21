-- name: CreateSession :one
INSERT INTO sessions (id, user_id, family_id, prev_id, refresh_hash,
                      user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- GetSessionByRefreshHash looks a session up by the hash of the presented
-- token. It deliberately returns revoked, used and expired rows too: the
-- caller must be able to tell "already used" from "unknown", because those
-- mean very different things.
--
-- name: GetSessionByRefreshHash :one
SELECT * FROM sessions WHERE refresh_hash = $1;

-- name: GetSessionByID :one
SELECT * FROM sessions WHERE id = $1;

-- MarkSessionUsed claims a refresh token. The used_at IS NULL predicate makes
-- the claim atomic: two concurrent refreshes with the same token produce one
-- winner and one zero-row result, and the loser is treated as reuse.
--
-- name: MarkSessionUsed :one
UPDATE sessions
   SET used_at = now()
 WHERE id = $1
   AND used_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now()
RETURNING *;

-- name: RevokeSessionFamily :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE family_id = $1
   AND revoked_at IS NULL;

-- name: RevokeSession :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE id = $1
   AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE user_id = $1
   AND revoked_at IS NULL;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
 WHERE expires_at < now() - INTERVAL '30 days';

-- name: RecordSecurityEvent :exec
INSERT INTO security_events (user_id, kind, detail, ip, user_agent)
VALUES ($1, $2, $3, $4, $5);
