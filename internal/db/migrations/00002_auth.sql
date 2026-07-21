-- +goose Up

CREATE TABLE users (
    id                UUID PRIMARY KEY,
    email             CITEXT      NOT NULL UNIQUE,
    password_hash     TEXT        NOT NULL,
    email_verified_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per refresh token ever issued. Rotation inserts a new row and marks
-- the old one used; it never updates a token in place, because that history is
-- what makes reuse detection possible.
--
-- family_id groups every rotation descended from a single login. Presenting a
-- refresh token that has already been used revokes the entire family: either
-- the token was stolen or the legitimate client replayed one, and there is no
-- way to tell those apart from the server side.
CREATE TABLE sessions (
    id           UUID PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id    UUID        NOT NULL,
    prev_id      UUID        REFERENCES sessions (id) ON DELETE SET NULL,
    -- SHA-256 of the opaque refresh token. The token itself is never stored.
    refresh_hash BYTEA       NOT NULL UNIQUE,
    user_agent   TEXT        NOT NULL DEFAULT '',
    ip           INET,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX sessions_user_id_idx    ON sessions (user_id);
CREATE INDEX sessions_family_id_idx  ON sessions (family_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- Append-only audit trail. Read during incident review; never updated or
-- deleted by application code.
CREATE TABLE security_events (
    id         BIGSERIAL PRIMARY KEY,
    user_id    UUID REFERENCES users (id) ON DELETE SET NULL,
    kind       TEXT        NOT NULL,
    detail     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip         INET,
    user_agent TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX security_events_user_id_idx    ON security_events (user_id);
CREATE INDEX security_events_created_at_idx ON security_events (created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS security_events;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
