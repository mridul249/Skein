-- name: CreateUser :one
INSERT INTO users (id, email, password_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: UpdateUserPassword :exec
UPDATE users
   SET password_hash = $2,
       updated_at    = now()
 WHERE id = $1;

-- BumpUserSessionEpoch invalidates every session the user currently has,
-- including any that a concurrent refresh is in the middle of creating.
--
-- This is the enforcing write for user-level revocation (known issue #18).
-- Unlike RevokeAllUserSessions it does not enumerate rows, so there is no
-- instant whose membership it could miss: a successor inserted after this
-- statement still carries the epoch it inherited from its parent, which is now
-- stale, and ClaimSession refuses it.
--
-- Returns the new epoch so the caller can mint a replacement session under it
-- — a password change should not sign the device that performed it out.
--
-- name: BumpUserSessionEpoch :one
UPDATE users
   SET session_epoch = session_epoch + 1,
       updated_at    = now()
 WHERE id = $1
RETURNING session_epoch;

-- name: MarkEmailVerified :exec
UPDATE users
   SET email_verified_at = now(),
       updated_at        = now()
 WHERE id = $1
   AND email_verified_at IS NULL;
