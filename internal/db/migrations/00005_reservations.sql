-- +goose Up

-- Quota reservations. Architecture.md §5.
--
-- The naive approach — read available bytes, decide, write — races: two
-- concurrent uploads both see the same free space and both commit. The
-- reference project used an in-memory Map for this, which does not even
-- survive a restart, let alone a second process.
--
-- Instead a reservation is a row, taken with one conditional UPDATE against
-- storage_accounts. Free space is total - used - reserved, so a reservation
-- makes bytes unavailable to everyone else the moment it commits.
CREATE TABLE quota_reservations (
    id                 UUID PRIMARY KEY,
    storage_account_id UUID   NOT NULL
        REFERENCES storage_accounts (connected_account_id) ON DELETE CASCADE,
    bytes              BIGINT NOT NULL,
    -- The upload this reservation belongs to, so committing or abandoning
    -- one upload releases exactly its own reservations.
    upload_id          UUID   NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A crashed process must not strand capacity forever. The janitor
    -- releases anything past this.
    expires_at         TIMESTAMPTZ NOT NULL,

    CONSTRAINT quota_reservations_bytes_positive CHECK (bytes > 0)
);

CREATE INDEX quota_reservations_upload_idx  ON quota_reservations (upload_id);
CREATE INDEX quota_reservations_expires_idx ON quota_reservations (expires_at);
CREATE INDEX quota_reservations_account_idx ON quota_reservations (storage_account_id);

-- In-flight uploads, so a resumable upload survives a browser refresh and so
-- the janitor knows which reservations belong to something still running.
CREATE TABLE uploads (
    id             UUID PRIMARY KEY,
    user_id        UUID   NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id        UUID   REFERENCES files (id) ON DELETE SET NULL,
    status         TEXT   NOT NULL DEFAULT 'in_progress',
    size_bytes     BIGINT NOT NULL,
    bytes_received BIGINT NOT NULL DEFAULT 0,
    plan           JSONB  NOT NULL DEFAULT '[]'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT uploads_status_check
        CHECK (status IN ('in_progress', 'completed', 'failed', 'abandoned')),
    CONSTRAINT uploads_sizes_non_negative
        CHECK (size_bytes >= 0 AND bytes_received >= 0)
);

CREATE INDEX uploads_user_idx    ON uploads (user_id, status);
CREATE INDEX uploads_expires_idx ON uploads (expires_at) WHERE status = 'in_progress';

-- +goose Down
DROP TABLE IF EXISTS uploads;
DROP TABLE IF EXISTS quota_reservations;
