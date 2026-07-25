-- +goose Up

CREATE TABLE folders (
    id         UUID PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id  UUID        REFERENCES folders (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,

    CONSTRAINT folders_name_not_blank CHECK (length(btrim(name)) > 0)
);

-- Two partial indexes rather than one: a NULL parent_id is the root, and NULL
-- never equals NULL, so a single UNIQUE (user_id, parent_id, name) would let
-- unlimited duplicates accumulate at the top level.
CREATE UNIQUE INDEX folders_unique_child ON folders (user_id, parent_id, name)
    WHERE deleted_at IS NULL AND parent_id IS NOT NULL;
CREATE UNIQUE INDEX folders_unique_root ON folders (user_id, name)
    WHERE deleted_at IS NULL AND parent_id IS NULL;

CREATE INDEX folders_user_parent_idx ON folders (user_id, parent_id) WHERE deleted_at IS NULL;

CREATE TABLE files (
    id             UUID PRIMARY KEY,
    user_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder_id      UUID        REFERENCES folders (id) ON DELETE SET NULL,
    name           TEXT        NOT NULL,
    size_bytes     BIGINT      NOT NULL,
    -- The client's declared MIME type, kept as metadata only. It is never
    -- echoed into a response Content-Type: Rules.md §2.3 sniffs and applies
    -- an allowlist at serve time instead.
    declared_mime  TEXT        NOT NULL DEFAULT '',
    content_sha256 BYTEA,
    is_striped     BOOLEAN     NOT NULL DEFAULT false,
    is_encrypted   BOOLEAN     NOT NULL DEFAULT false,
    -- pending until every shard is committed. A reader refuses anything that
    -- is not 'ready', so a crashed upload is never served as a whole file.
    status         TEXT        NOT NULL DEFAULT 'pending',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ,

    CONSTRAINT files_size_non_negative CHECK (size_bytes >= 0),
    CONSTRAINT files_status_check CHECK (status IN ('pending', 'ready', 'failed')),
    CONSTRAINT files_name_not_blank CHECK (length(btrim(name)) > 0)
);

CREATE INDEX files_listing_idx ON files (user_id, folder_id, deleted_at);
CREATE INDEX files_user_deleted_idx ON files (user_id, deleted_at);
CREATE INDEX files_sha256_idx ON files (user_id, content_sha256) WHERE content_sha256 IS NOT NULL;

-- The manifest. One row per shard, and a file with a single shard is simply a
-- manifest of length one.
--
-- This table arrives in Phase 3 rather than Phase 5 on purpose: the one-shard
-- and many-shard cases are the same shape, and introducing it later would mean
-- migrating live rows into a schema the striping reader depends on.
CREATE TABLE file_shards (
    id                   UUID PRIMARY KEY,
    file_id              UUID   NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    idx                  INT    NOT NULL,
    connected_account_id UUID   REFERENCES connected_accounts (id) ON DELETE SET NULL,
    provider_object_id   TEXT   NOT NULL,
    -- Bytes as stored: ciphertext is longer than plaintext, so this is not
    -- the same number as the shard's share of files.size_bytes.
    size_bytes           BIGINT NOT NULL,
    plain_size_bytes     BIGINT NOT NULL,
    -- Offset of this shard's first plaintext byte within the whole file. It
    -- is what lets a Range request find the shards it needs without reading
    -- the ones it does not.
    plain_offset         BIGINT NOT NULL,
    sha256               BYTEA,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT file_shards_unique_index UNIQUE (file_id, idx),
    CONSTRAINT file_shards_sizes_non_negative
        CHECK (size_bytes >= 0 AND plain_size_bytes >= 0 AND plain_offset >= 0)
);

CREATE INDEX file_shards_file_idx ON file_shards (file_id, idx);
CREATE INDEX file_shards_account_idx ON file_shards (connected_account_id);

-- +goose Down
DROP TABLE IF EXISTS file_shards;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS folders;
