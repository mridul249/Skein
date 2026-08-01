-- SQLite dialect of internal/db/queries/users.sql.
--
-- The one systematic difference from the Postgres originals: there is no
-- now(). Every timestamp is a bound parameter supplied by the store, which
-- also makes these queries testable against a fake clock rather than the
-- wall clock. See internal/db/migrations/sqlite/00001_auth.sql.

-- name: CreateUser :one
INSERT INTO users (id, email, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- Case-insensitivity comes from the column's NOCASE collation, so this is a
-- plain equality test exactly as the Postgres citext version is.
--
-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: UpdateUserPassword :exec
UPDATE users
   SET password_hash = ?,
       updated_at    = ?
 WHERE id = ?;

-- name: MarkEmailVerified :exec
UPDATE users
   SET email_verified_at = ?,
       updated_at        = ?
 WHERE id = ?
   AND email_verified_at IS NULL;
