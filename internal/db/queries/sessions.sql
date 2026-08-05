-- CreateSession inserts one issued refresh token.
--
-- epoch is supplied by the caller and is NEVER read from users here. On
-- rotation it is copied from the parent row that was just claimed; on a fresh
-- login it is the user's current epoch. Reading it in this statement instead
-- would reintroduce known issue #18 one scope up: a successor whose INSERT
-- races a revocation would read the NEW epoch and be born valid, which is
-- precisely the race the epoch exists to close.
--
-- name: CreateSession :one
INSERT INTO sessions (id, user_id, family_id, prev_id, refresh_hash,
                      user_agent, ip, expires_at, epoch)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
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
-- The NOT EXISTS clause is the authoritative half of the two-part validity
-- condition, and this statement is the only place it is enforced. Known issue
-- #11: a successor inserted after its family was revoked carries revoked_at
-- NULL, so its own column says nothing. Reading the family here, fresh, at
-- claim time, is what makes such a row unusable.
--
-- Enforcement deliberately does NOT live on the successor's INSERT. Both
-- INSERT ... WHERE NOT EXISTS and INSERT ... SELECT ... WHERE revoked_at IS
-- NULL look airtight and are not: under READ COMMITTED each statement takes its
-- snapshot at statement start, so the insert cannot see the concurrent
-- revocation and the revocation cannot see the new row. Both commit and the bug
-- is unchanged. A single statement is atomic; it is not serialisable.
--
-- The epoch predicate is the second half of the same idea, for user-level
-- revocation (known issue #18). A session is valid only while the epoch it was
-- born under still matches its user's current one, and this is the only place
-- that is enforced. Reading users here, fresh, at claim time is what makes a
-- session created before a password change unusable after it — including one
-- inserted moments after the revocation by a refresh that was already in
-- flight, since that successor inherited its parent's stale epoch.
--
-- name: MarkSessionUsed :one
UPDATE sessions
   SET used_at = now()
 WHERE sessions.id = $1
   AND sessions.used_at IS NULL
   AND sessions.revoked_at IS NULL
   AND sessions.expires_at > now()
   AND NOT EXISTS (
       SELECT 1 FROM token_families f
        WHERE f.id = sessions.family_id
          AND f.revoked_at IS NOT NULL)
   AND EXISTS (
       SELECT 1 FROM users u
        WHERE u.id = sessions.user_id
          AND u.session_epoch = sessions.epoch)
RETURNING *;

-- name: RevokeSessionFamily :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE family_id = $1
   AND revoked_at IS NULL;

-- RevokeSession revokes a single session.
--
-- UNSOUND FOR "SIGN OUT THIS DEVICE". DO NOT USE YET. Known issue #18.
--
-- It revokes one row and cannot bind that session's future successors, so a
-- concurrent refresh rotates the revoked session into a live one. Note also
-- that a session's successors are all in its family by construction, so
-- "revoke this device" and "revoke this family" are the same operation —
-- RevokeTokenFamily plus RevokeSessionFamily is the correct primitive for that
-- job, and this one is the wrong shape regardless of the race.
--
-- name: RevokeSession :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE id = $1
   AND revoked_at IS NULL;

-- RevokeAllUserSessions signs a user out everywhere.
--
-- UNSOUND AGAINST A CONCURRENT REFRESH. DO NOT USE YET. Known issue #18.
--
-- This enumerates the sessions that exist at this instant and cannot bind one
-- inserted afterwards, which is exactly the defect token_families fixed for the
-- family case. A refresh that has claimed its token but not yet inserted its
-- successor will produce a live session that outlives this sweep. It has no
-- production caller today, which is the only reason it is not a live bug.
--
-- Before wiring this to sign-out-everywhere, password change or account
-- deletion, it needs validity state that a racing insert inherits — a per-user
-- epoch on users, INHERITED FROM THE CLAIMED PARENT on rotation the way
-- expires_at already is. An epoch re-read at insert time reproduces this
-- identical race one scope up: the successor picks up the new epoch and is born
-- valid. token_families does not cover this case; a family is one login, and a
-- user has one family per device.
--
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

-- CreateTokenFamily records a new login's family. Written before the session
-- row, because sessions.family_id has a foreign key to it. Reversing the order
-- gives an FK violation; this order risks at worst an orphan family row, which
-- is harmless.
--
-- name: CreateTokenFamily :exec
INSERT INTO token_families (id, user_id) VALUES ($1, $2);

-- RevokeTokenFamily kills every token descended from one login, including
-- successors that do not exist yet. This is the enforcing write.
--
-- Idempotent, and it preserves the first revocation: revoked_at IS NULL means a
-- second call reports zero rows rather than moving the timestamp forward, so
-- the audit trail keeps saying when the family actually died.
--
-- name: RevokeTokenFamily :execrows
UPDATE token_families
   SET revoked_at = now()
 WHERE id = $1
   AND revoked_at IS NULL;
