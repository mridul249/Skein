-- +goose Up

CREATE TABLE quota_reservations (
    id                 TEXT PRIMARY KEY,
    storage_account_id TEXT NOT NULL
        REFERENCES storage_accounts (connected_account_id) ON DELETE CASCADE,
    bytes              INTEGER NOT NULL,
    upload_id          TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    expires_at         TEXT NOT NULL,

    CONSTRAINT quota_reservations_bytes_positive CHECK (bytes > 0)
) STRICT;

CREATE INDEX quota_reservations_upload_idx  ON quota_reservations (upload_id);
CREATE INDEX quota_reservations_expires_idx ON quota_reservations (expires_at);
CREATE INDEX quota_reservations_account_idx ON quota_reservations (storage_account_id);

CREATE TABLE uploads (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id        TEXT REFERENCES files (id) ON DELETE SET NULL,
    status         TEXT NOT NULL DEFAULT 'in_progress',
    size_bytes     INTEGER NOT NULL,
    bytes_received INTEGER NOT NULL DEFAULT 0,
    plan           TEXT NOT NULL DEFAULT '[]',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,

    CONSTRAINT uploads_status_check
        CHECK (status IN ('in_progress', 'completed', 'failed', 'abandoned')),
    CONSTRAINT uploads_sizes_non_negative
        CHECK (size_bytes >= 0 AND bytes_received >= 0)
) STRICT;

CREATE INDEX uploads_user_idx    ON uploads (user_id, status);
CREATE INDEX uploads_expires_idx ON uploads (expires_at) WHERE status = 'in_progress';

-- +goose Down
DROP TABLE IF EXISTS uploads;
DROP TABLE IF EXISTS quota_reservations;
