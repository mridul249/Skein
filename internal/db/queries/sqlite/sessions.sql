-- SQLite dialect of internal/db/queries/sessions.sql. Same contracts, same
-- comments where the reasoning is unchanged; the differences are noted where
-- they exist. now() becomes a bound parameter throughout (see users.sql).
--
-- KEEP THIS FILE ASCII-ONLY. sqlc v1.31.1's SQLite engine measures a comment's
-- length in bytes and then slices it by rune, so every multi-byte character
-- before a query shifts that query's start by (bytes-1) and truncates its tail.
-- The symptom is a parse error naming a chopped keyword -- one em-dash turns
-- RETURNING into RETURNIN, two into RETURNI, and so on. It is silent until the
-- damage reaches a keyword, so prose written in the house style (which uses
-- em-dashes freely) breaks codegen at a distance. Use "--" for an em-dash and
-- a plain apostrophe for a curly one. The Postgres files are unaffected.

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, family_id, prev_id, refresh_hash,
                      user_agent, ip, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- GetSessionByRefreshHash looks a session up by the hash of the presented
-- token. It deliberately returns revoked, used and expired rows too: the
-- caller must be able to tell "already used" from "unknown", because those
-- mean very different things.
--
-- name: GetSessionByRefreshHash :one
SELECT * FROM sessions WHERE refresh_hash = ?;

-- name: GetSessionByID :one
SELECT * FROM sessions WHERE id = ?;

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
-- The Postgres original carries a long warning that a single statement is
-- atomic but not serialisable, and that under READ COMMITTED an INSERT and a
-- concurrent revocation can each take a snapshot that misses the other. That
-- hazard does not exist here: SQLite serialises writers outright, so any two
-- write statements are ordered with respect to each other. This query is a
-- direct translation and is if anything more strongly protected. The
-- enforcement still belongs on the claim rather than the successor's INSERT --
-- keeping the two dialects behaviourally identical matters more than
-- exploiting the stronger guarantee.
--
-- Columns are qualified with `sessions.` for a reason beyond style: sqlc's
-- SQLite analyser treats the subquery's table as in scope for the outer
-- statement, so an unqualified `id` is rejected here as ambiguous. Qualifying
-- also keeps this a character-for-character match with the Postgres original
-- apart from the bound timestamps, which is the point -- the two dialects must
-- not drift on the one query that enforces issue #11.
--
-- name: MarkSessionUsed :one
UPDATE sessions
   SET used_at = ?
 WHERE sessions.id = ?
   AND sessions.used_at IS NULL
   AND sessions.revoked_at IS NULL
   AND sessions.expires_at > ?
   AND NOT EXISTS (
       SELECT 1 FROM token_families f
        WHERE f.id = sessions.family_id
          AND f.revoked_at IS NOT NULL)
RETURNING *;

-- name: RevokeSessionFamily :execrows
UPDATE sessions
   SET revoked_at = ?
 WHERE family_id = ?
   AND revoked_at IS NULL;

-- RevokeSession revokes a single session.
--
-- UNSOUND FOR "SIGN OUT THIS DEVICE". DO NOT USE YET. Known issue #18.
--
-- It revokes one row and cannot bind that session's future successors, so a
-- concurrent refresh rotates the revoked session into a live one. Note also
-- that a session's successors are all in its family by construction, so
-- "revoke this device" and "revoke this family" are the same operation --
-- RevokeTokenFamily plus RevokeSessionFamily is the correct primitive for that
-- job, and this one is the wrong shape regardless of the race.
--
-- name: RevokeSession :execrows
UPDATE sessions
   SET revoked_at = ?
 WHERE id = ?
   AND revoked_at IS NULL;

-- RevokeAllUserSessions signs a user out everywhere.
--
-- UNSOUND AGAINST A CONCURRENT REFRESH. DO NOT USE YET. Known issue #18.
--
-- This enumerates the sessions that exist at this instant and cannot bind one
-- inserted afterwards, which is exactly the defect token_families fixed for the
-- family case. Serialised writers do not save this one: the sweep and the
-- successor's INSERT are both well-ordered, and the successor simply lands
-- after the sweep. The fix is the same on both engines -- validity state a
-- racing insert inherits, i.e. a per-user epoch copied from the claimed parent
-- on rotation.
--
-- name: RevokeAllUserSessions :execrows
UPDATE sessions
   SET revoked_at = ?
 WHERE user_id = ?
   AND revoked_at IS NULL;

-- DeleteExpiredSessions drops long-dead rows. The Postgres original computes
-- the cutoff in SQL (now() - INTERVAL '30 days'); here the caller passes the
-- already-computed cutoff, so the retention window lives in one place in Go
-- rather than being duplicated in two dialects.
--
-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
 WHERE expires_at < ?;

-- name: RecordSecurityEvent :exec
INSERT INTO security_events (user_id, kind, detail, ip, user_agent, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- CreateTokenFamily records a new login's family. Written before the session
-- row, because sessions.family_id has a foreign key to it. Reversing the order
-- gives an FK violation; this order risks at worst an orphan family row, which
-- is harmless.
--
-- name: CreateTokenFamily :exec
INSERT INTO token_families (id, user_id, created_at) VALUES (?, ?, ?);

-- RevokeTokenFamily kills every token descended from one login, including
-- successors that do not exist yet. This is the enforcing write.
--
-- Idempotent, and it preserves the first revocation: revoked_at IS NULL means a
-- second call reports zero rows rather than moving the timestamp forward, so
-- the audit trail keeps saying when the family actually died.
--
-- name: RevokeTokenFamily :execrows
UPDATE token_families
   SET revoked_at = ?
 WHERE id = ?
   AND revoked_at IS NULL;

-- --- test support ---------------------------------------------------------
-- These back the inspection helpers the auth test suite uses to assert on
-- state (memstore.go has in-memory equivalents). They are read-only and have
-- no production caller.

-- name: SessionsInFamily :many
SELECT * FROM sessions WHERE family_id = ?;

-- name: FamilyRevokedAt :one
SELECT revoked_at FROM token_families WHERE id = ?;

-- name: EventsOfKind :many
SELECT * FROM security_events WHERE kind = ? ORDER BY id;

-- name: ExpireSession :execrows
UPDATE sessions SET expires_at = ? WHERE id = ?;
