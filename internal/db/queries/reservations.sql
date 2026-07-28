-- ReserveBytes is the whole reservation scheme. Architecture.md §5.
--
-- One statement, one decision. The WHERE clause is the check and the UPDATE is
-- the commit, so there is no window between them for a second uploader to make
-- the same decision. Zero rows returned means somebody else got there first
-- and the caller should try the next candidate — it is not an error.
--
-- Never replace this with a SELECT followed by an UPDATE.
--
-- name: ReserveBytes :one
UPDATE storage_accounts
   SET reserved_bytes = reserved_bytes + @bytes::bigint
 WHERE connected_account_id = @connected_account_id
   AND (total_bytes - used_bytes - reserved_bytes) >= @bytes::bigint
RETURNING connected_account_id, total_bytes, used_bytes, reserved_bytes;

-- name: RecordReservation :exec
INSERT INTO quota_reservations (id, storage_account_id, bytes, upload_id, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- ReleaseReservation gives bytes back. GREATEST(0, ...) is not decoration:
-- without it a double release would drive reserved_bytes negative and quietly
-- inflate free space for every subsequent upload.
--
-- name: ReleaseReservation :exec
UPDATE storage_accounts
   SET reserved_bytes = GREATEST(0, reserved_bytes - @bytes::bigint)
 WHERE connected_account_id = @connected_account_id;

-- name: DeleteReservationsForUpload :many
DELETE FROM quota_reservations
 WHERE upload_id = $1
RETURNING storage_account_id, bytes;

-- ExpiredReservations feeds the janitor. Rows are returned and deleted in one
-- statement so two janitor runs cannot release the same reservation twice.
--
-- name: ExpiredReservations :many
DELETE FROM quota_reservations
 WHERE expires_at < now()
RETURNING id, storage_account_id, bytes, upload_id;

-- name: CreateUpload :one
INSERT INTO uploads (id, user_id, file_id, size_bytes, plan, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: SetUploadStatus :exec
UPDATE uploads
   SET status = $2, bytes_received = $3, updated_at = now()
 WHERE id = $1;

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = $1 AND user_id = $2;

-- name: AbandonExpiredUploads :many
UPDATE uploads
   SET status = 'abandoned', updated_at = now()
 WHERE status = 'in_progress'
   AND expires_at < now()
RETURNING id, file_id;

-- ListAccountCapacityForPlanning returns candidates in most-free-first order,
-- which is what the greedy planner walks. It is one query rather than a lookup
-- per account.
--
-- name: ListAccountCapacityForPlanning :many
SELECT ca.id,
       ca.ordinal,
       ca.email,
       COALESCE(sa.total_bytes, 0)    AS total_bytes,
       COALESCE(sa.used_bytes, 0)     AS used_bytes,
       COALESCE(sa.reserved_bytes, 0) AS reserved_bytes
  FROM connected_accounts ca
  JOIN storage_accounts sa ON sa.connected_account_id = ca.id
 WHERE ca.user_id = $1
   AND ca.status = 'active'
 ORDER BY (sa.total_bytes - sa.used_bytes - sa.reserved_bytes) DESC, ca.ordinal;
