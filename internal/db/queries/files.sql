-- name: CreateFolder :one
INSERT INTO folders (id, user_id, parent_id, name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetFolder :one
SELECT * FROM folders
 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: ListFolders :many
SELECT * FROM folders
 WHERE user_id = $1 AND deleted_at IS NULL
 ORDER BY name;

-- name: ListChildFolders :many
SELECT * FROM folders
 WHERE user_id = $1
   AND deleted_at IS NULL
   AND parent_id IS NOT DISTINCT FROM sqlc.narg('parent_id')::uuid
 ORDER BY name;

-- name: RenameFolder :one
UPDATE folders
   SET name = $3, parent_id = $4, updated_at = now()
 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteFolder moves a folder to trash. Its children go with it, in the
-- same statement, so a partially trashed tree is not a state that can exist.
--
-- name: SoftDeleteFolder :execrows
WITH RECURSIVE subtree AS (
    SELECT f0.id FROM folders f0
     WHERE f0.id = $1 AND f0.user_id = $2 AND f0.deleted_at IS NULL
    UNION ALL
    SELECT f.id FROM folders f
     JOIN subtree s ON f.parent_id = s.id
     WHERE f.deleted_at IS NULL
)
UPDATE folders
   SET deleted_at = now(), updated_at = now()
 WHERE folders.id IN (SELECT subtree.id FROM subtree);

-- name: SoftDeleteFilesInFolderTree :execrows
WITH RECURSIVE subtree AS (
    SELECT f0.id FROM folders f0
     WHERE f0.id = $1 AND f0.user_id = $2
    UNION ALL
    SELECT f.id FROM folders f
     JOIN subtree s ON f.parent_id = s.id
)
UPDATE files
   SET deleted_at = now(), updated_at = now()
 WHERE files.user_id = $2
   AND files.deleted_at IS NULL
   AND files.folder_id IN (SELECT subtree.id FROM subtree);

-- FolderDescendants is used to reject a move that would put a folder inside
-- its own subtree, which would detach the whole branch from the root.
--
-- name: FolderDescendants :many
WITH RECURSIVE subtree AS (
    SELECT f0.id FROM folders f0
     WHERE f0.id = $1 AND f0.user_id = $2
    UNION ALL
    SELECT f.id FROM folders f
     JOIN subtree s ON f.parent_id = s.id
)
SELECT subtree.id FROM subtree;

-- name: CreateFile :one
INSERT INTO files (id, user_id, folder_id, name, size_bytes, declared_mime,
                   is_striped, is_encrypted, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')
RETURNING *;

-- name: MarkFileReady :one
UPDATE files
   SET status         = 'ready',
       size_bytes     = $3,
       content_sha256 = $4,
       updated_at     = now()
 WHERE id = $1 AND user_id = $2 AND status = 'pending'
RETURNING *;

-- name: MarkFileFailed :exec
UPDATE files
   SET status = 'failed', updated_at = now()
 WHERE id = $1 AND status = 'pending';

-- name: GetFile :one
SELECT * FROM files
 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- ListFiles is keyset-paginated on (created_at, id). OFFSET would re-scan the
-- skipped rows on every page and would silently shift entries as new files
-- arrive.
--
-- name: ListFiles :many
SELECT * FROM files
 WHERE user_id = $1
   AND deleted_at IS NULL
   AND status = 'ready'
   AND folder_id IS NOT DISTINCT FROM sqlc.narg('folder_id')::uuid
   AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
        OR (created_at, id) < (sqlc.narg('cursor_created_at')::timestamptz,
                               sqlc.narg('cursor_id')::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT $2;

-- name: ListTrashedFiles :many
SELECT * FROM files
 WHERE user_id = $1 AND deleted_at IS NOT NULL
 ORDER BY deleted_at DESC
 LIMIT $2;

-- name: UpdateFile :one
UPDATE files
   SET name = $3, folder_id = $4, updated_at = now()
 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteFile :execrows
UPDATE files
   SET deleted_at = now(), updated_at = now()
 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: RestoreFile :execrows
UPDATE files
   SET deleted_at = NULL, updated_at = now()
 WHERE id = $1 AND user_id = $2 AND deleted_at IS NOT NULL;

-- name: HardDeleteFile :execrows
DELETE FROM files WHERE id = $1 AND user_id = $2;

-- name: CreateFileShard :one
INSERT INTO file_shards (id, file_id, idx, connected_account_id,
                         provider_object_id, size_bytes, plain_size_bytes,
                         plain_offset, sha256)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListFileShards :many
SELECT * FROM file_shards WHERE file_id = $1 ORDER BY idx;

-- ListShardsForFiles fetches manifests for a page of files in one query,
-- rather than one query per row. Rules.md §2.12.
--
-- name: ListShardsForFiles :many
SELECT * FROM file_shards
 WHERE file_id = ANY($1::uuid[])
 ORDER BY file_id, idx;

-- name: DeleteFileShards :execrows
DELETE FROM file_shards WHERE file_id = $1;

-- name: CountFilesInFolder :one
SELECT COUNT(*) FROM files
 WHERE user_id = $1 AND folder_id = $2 AND deleted_at IS NULL;
