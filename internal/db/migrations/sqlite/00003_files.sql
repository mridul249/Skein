-- +goose Up

CREATE TABLE folders (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id  TEXT REFERENCES folders (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT,

    CONSTRAINT folders_name_not_blank CHECK (length(trim(name)) > 0)
) STRICT;

CREATE UNIQUE INDEX folders_unique_child ON folders (user_id, parent_id, name)
    WHERE deleted_at IS NULL AND parent_id IS NOT NULL;
CREATE UNIQUE INDEX folders_unique_root ON folders (user_id, name)
    WHERE deleted_at IS NULL AND parent_id IS NULL;
CREATE INDEX folders_user_parent_idx ON folders (user_id, parent_id) WHERE deleted_at IS NULL;

CREATE TABLE files (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder_id      TEXT REFERENCES folders (id) ON DELETE SET NULL,
    name           TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,
    declared_mime  TEXT NOT NULL DEFAULT '',
    content_sha256 BLOB,
    is_striped     INTEGER NOT NULL DEFAULT 0,
    is_encrypted   INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'pending',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    deleted_at     TEXT,

    CONSTRAINT files_size_non_negative CHECK (size_bytes >= 0),
    CONSTRAINT files_status_check CHECK (status IN ('pending', 'ready', 'failed')),
    CONSTRAINT files_name_not_blank CHECK (length(trim(name)) > 0),
    CONSTRAINT files_bool_check CHECK (is_striped IN (0, 1) AND is_encrypted IN (0, 1))
) STRICT;

CREATE INDEX files_listing_idx ON files (user_id, folder_id, deleted_at);
CREATE INDEX files_user_deleted_idx ON files (user_id, deleted_at);
CREATE INDEX files_sha256_idx ON files (user_id, content_sha256) WHERE content_sha256 IS NOT NULL;

CREATE TABLE file_shards (
    id                   TEXT PRIMARY KEY,
    file_id              TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    idx                  INTEGER NOT NULL,
    connected_account_id TEXT REFERENCES connected_accounts (id) ON DELETE SET NULL,
    provider_object_id   TEXT NOT NULL,
    size_bytes           INTEGER NOT NULL,
    plain_size_bytes     INTEGER NOT NULL,
    plain_offset         INTEGER NOT NULL,
    sha256               BLOB,
    created_at           TEXT NOT NULL,

    CONSTRAINT file_shards_unique_index UNIQUE (file_id, idx),
    CONSTRAINT file_shards_sizes_non_negative
        CHECK (size_bytes >= 0 AND plain_size_bytes >= 0 AND plain_offset >= 0)
) STRICT;

CREATE INDEX file_shards_file_idx ON file_shards (file_id, idx);
CREATE INDEX file_shards_account_idx ON file_shards (connected_account_id);

-- +goose Down
DROP TABLE IF EXISTS file_shards;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS folders;
