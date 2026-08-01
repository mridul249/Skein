package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
)

const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteStore is the SQLite implementation of Store for skein-desktop.
type SQLiteStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLiteStore wraps an open migrated SQLite database.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db, now: time.Now}
}

func (s *SQLiteStore) CreateFolder(ctx context.Context, id, userID uuid.UUID, parentID *uuid.UUID, name string) (Folder, error) {
	now := s.fmt(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO folders (id, user_id, parent_id, name, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id, user_id, parent_id, name, created_at, updated_at, deleted_at`,
		id.String(), userID.String(), uuidPtrToString(parentID), name, now, now)
	return s.scanFolder(row, "insert folder")
}

func (s *SQLiteStore) GetFolder(ctx context.Context, userID, id uuid.UUID) (Folder, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, user_id, parent_id, name, created_at, updated_at, deleted_at
  FROM folders
 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, id.String(), userID.String())
	return s.scanFolder(row, "select folder")
}

func (s *SQLiteStore) ListFolders(ctx context.Context, userID uuid.UUID) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, parent_id, name, created_at, updated_at, deleted_at
  FROM folders
 WHERE user_id = ? AND deleted_at IS NULL
 ORDER BY name`, userID.String())
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()
	return s.scanFolders(rows)
}

func (s *SQLiteStore) UpdateFolder(ctx context.Context, userID, id uuid.UUID, name string, parentID *uuid.UUID) (Folder, error) {
	row := s.db.QueryRowContext(ctx, `
UPDATE folders
   SET name = ?3, parent_id = ?4, updated_at = ?5
 WHERE id = ?1 AND user_id = ?2 AND deleted_at IS NULL
RETURNING id, user_id, parent_id, name, created_at, updated_at, deleted_at`,
		id.String(), userID.String(), name, uuidPtrToString(parentID), s.fmt(s.now()))
	return s.scanFolder(row, "update folder")
}

func (s *SQLiteStore) FolderDescendants(ctx context.Context, userID, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE subtree AS (
    SELECT f0.id FROM folders f0
     WHERE f0.id = ?1 AND f0.user_id = ?2
    UNION ALL
    SELECT f.id FROM folders f
     JOIN subtree st ON f.parent_id = st.id
)
SELECT id FROM subtree`, id.String(), userID.String())
	if err != nil {
		return nil, fmt.Errorf("select folder subtree: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan folder subtree: %w", err)
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse folder subtree id %q: %w", raw, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan folder subtree: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) SoftDeleteFolderTree(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin folder delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.fmt(s.now())
	if _, err := tx.ExecContext(ctx, `
WITH RECURSIVE subtree AS (
    SELECT f0.id FROM folders f0
     WHERE f0.id = ?1 AND f0.user_id = ?2
    UNION ALL
    SELECT f.id FROM folders f
     JOIN subtree st ON f.parent_id = st.id
)
UPDATE files
   SET deleted_at = ?3, updated_at = ?3
 WHERE files.user_id = ?2
   AND files.deleted_at IS NULL
   AND files.folder_id IN (SELECT id FROM subtree)`, id.String(), userID.String(), now); err != nil {
		return 0, fmt.Errorf("trash files in folder: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
WITH RECURSIVE subtree AS (
    SELECT f0.id FROM folders f0
     WHERE f0.id = ?1 AND f0.user_id = ?2 AND f0.deleted_at IS NULL
    UNION ALL
    SELECT f.id FROM folders f
     JOIN subtree st ON f.parent_id = st.id
     WHERE f.deleted_at IS NULL
)
UPDATE folders
   SET deleted_at = ?3, updated_at = ?3
 WHERE id IN (SELECT id FROM subtree)`, id.String(), userID.String(), now)
	if err != nil {
		return 0, fmt.Errorf("trash folder: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit folder delete: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) CreateFile(ctx context.Context, n NewFile) (File, error) {
	now := s.fmt(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO files (id, user_id, folder_id, name, size_bytes, declared_mime,
                   is_striped, is_encrypted, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
RETURNING id, user_id, folder_id, name, size_bytes, declared_mime, content_sha256,
          is_striped, is_encrypted, status, created_at, updated_at, deleted_at`,
		n.ID.String(), n.UserID.String(), uuidPtrToString(n.FolderID), n.Name, n.SizeBytes,
		n.DeclaredMime, boolInt(n.IsStriped), boolInt(n.IsEncrypted), now, now)
	return s.scanFile(row, "insert file")
}

func (s *SQLiteStore) MarkFileReady(ctx context.Context, userID, id uuid.UUID, size int64, sha []byte) (File, error) {
	row := s.db.QueryRowContext(ctx, `
UPDATE files
   SET status = 'ready', size_bytes = ?3, content_sha256 = ?4, updated_at = ?5
 WHERE id = ?1 AND user_id = ?2 AND status = 'pending'
RETURNING id, user_id, folder_id, name, size_bytes, declared_mime, content_sha256,
          is_striped, is_encrypted, status, created_at, updated_at, deleted_at`,
		id.String(), userID.String(), size, sha, s.fmt(s.now()))
	return s.scanFile(row, "mark file ready")
}

func (s *SQLiteStore) MarkFileFailed(ctx context.Context, id uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE files
   SET status = 'failed', updated_at = ?
 WHERE id = ? AND status = 'pending'`, s.fmt(s.now()), id.String()); err != nil {
		return fmt.Errorf("mark file failed: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetFile(ctx context.Context, userID, id uuid.UUID) (File, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, user_id, folder_id, name, size_bytes, declared_mime, content_sha256,
       is_striped, is_encrypted, status, created_at, updated_at, deleted_at
  FROM files
 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, id.String(), userID.String())
	return s.scanFile(row, "select file")
}

func (s *SQLiteStore) ListFiles(ctx context.Context, userID uuid.UUID, p ListParams) ([]File, error) {
	args := []any{userID.String(), uuidPtrToString(p.FolderID)}
	cursorSQL := ""
	if p.CursorCreatedAt != nil && p.CursorID != nil {
		cursorSQL = "AND (created_at < ? OR (created_at = ? AND id < ?))"
		c := s.fmt(*p.CursorCreatedAt)
		args = append(args, c, c, p.CursorID.String())
	}
	args = append(args, p.Limit)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, folder_id, name, size_bytes, declared_mime, content_sha256,
       is_striped, is_encrypted, status, created_at, updated_at, deleted_at
  FROM files
 WHERE user_id = ?
   AND deleted_at IS NULL
   AND status = 'ready'
   AND ((? IS NULL AND folder_id IS NULL) OR folder_id = ?)
 `+cursorSQL+`
 ORDER BY created_at DESC, id DESC
 LIMIT ?`, normalizeListArgs(args)...)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	files, err := s.scanFiles(rows)
	if err != nil {
		return nil, err
	}
	return s.attachShards(ctx, files)
}

func (s *SQLiteStore) ListTrashed(ctx context.Context, userID uuid.UUID, limit int32) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, folder_id, name, size_bytes, declared_mime, content_sha256,
       is_striped, is_encrypted, status, created_at, updated_at, deleted_at
  FROM files
 WHERE user_id = ? AND deleted_at IS NOT NULL
 ORDER BY deleted_at DESC
 LIMIT ?`, userID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("list trashed files: %w", err)
	}
	defer rows.Close()
	return s.scanFiles(rows)
}

func (s *SQLiteStore) UpdateFile(ctx context.Context, userID, id uuid.UUID, name string, folderID *uuid.UUID) (File, error) {
	row := s.db.QueryRowContext(ctx, `
UPDATE files
   SET name = ?3, folder_id = ?4, updated_at = ?5
 WHERE id = ?1 AND user_id = ?2 AND deleted_at IS NULL
RETURNING id, user_id, folder_id, name, size_bytes, declared_mime, content_sha256,
          is_striped, is_encrypted, status, created_at, updated_at, deleted_at`,
		id.String(), userID.String(), name, uuidPtrToString(folderID), s.fmt(s.now()))
	return s.scanFile(row, "update file")
}

func (s *SQLiteStore) SoftDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE files
   SET deleted_at = ?3, updated_at = ?3
 WHERE id = ?1 AND user_id = ?2 AND deleted_at IS NULL`, id.String(), userID.String(), s.fmt(s.now()))
	if err != nil {
		return 0, fmt.Errorf("trash file: %w", err)
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) RestoreFile(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE files
   SET deleted_at = NULL, updated_at = ?3
 WHERE id = ?1 AND user_id = ?2 AND deleted_at IS NOT NULL`, id.String(), userID.String(), s.fmt(s.now()))
	if err != nil {
		return 0, fmt.Errorf("restore file: %w", err)
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) HardDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE id = ? AND user_id = ?`, id.String(), userID.String())
	if err != nil {
		return 0, fmt.Errorf("delete file: %w", err)
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) CreateShard(ctx context.Context, n NewShard) (Shard, error) {
	row := s.db.QueryRowContext(ctx, `
INSERT INTO file_shards (id, file_id, idx, connected_account_id, provider_object_id,
                         size_bytes, plain_size_bytes, plain_offset, sha256, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, file_id, idx, connected_account_id, provider_object_id, size_bytes,
          plain_size_bytes, plain_offset, sha256, created_at`,
		n.ID.String(), n.FileID.String(), n.Index, uuidPtrToString(n.AccountID), n.ProviderID,
		n.SizeBytes, n.PlainSize, n.PlainOffset, n.SHA256, s.fmt(s.now()))
	return s.scanShard(row, "insert shard")
}

func (s *SQLiteStore) ListShards(ctx context.Context, fileID uuid.UUID) ([]Shard, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, file_id, idx, connected_account_id, provider_object_id, size_bytes,
       plain_size_bytes, plain_offset, sha256, created_at
  FROM file_shards
 WHERE file_id = ?
 ORDER BY idx`, fileID.String())
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	defer rows.Close()
	return s.scanShards(rows)
}

func (s *SQLiteStore) DeleteShards(ctx context.Context, fileID uuid.UUID) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM file_shards WHERE file_id = ?`, fileID.String())
	if err != nil {
		return 0, fmt.Errorf("delete shards: %w", err)
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) attachShards(ctx context.Context, list []File) ([]File, error) {
	for i := range list {
		shards, err := s.ListShards(ctx, list[i].ID)
		if err != nil {
			return nil, err
		}
		list[i].Shards = shards
	}
	return list, nil
}

func (s *SQLiteStore) scanFolder(row interface{ Scan(...any) error }, what string) (Folder, error) {
	var raw folderRow
	if err := row.Scan(&raw.id, &raw.userID, &raw.parentID, &raw.name, &raw.createdAt, &raw.updatedAt, &raw.deletedAt); err != nil {
		return Folder{}, mapSQLiteWriteError(err, what)
	}
	return raw.toFolder()
}

func (s *SQLiteStore) scanFolders(rows *sql.Rows) ([]Folder, error) {
	var out []Folder
	for rows.Next() {
		f, err := s.scanFolder(rows, "scan folder")
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan folders: %w", err)
	}
	if out == nil {
		out = []Folder{}
	}
	return out, nil
}

func (s *SQLiteStore) scanFile(row interface{ Scan(...any) error }, what string) (File, error) {
	var raw fileRow
	if err := row.Scan(&raw.id, &raw.userID, &raw.folderID, &raw.name, &raw.sizeBytes, &raw.declaredMime,
		&raw.contentSHA, &raw.isStriped, &raw.isEncrypted, &raw.status, &raw.createdAt, &raw.updatedAt, &raw.deletedAt); err != nil {
		return File{}, mapSQLiteWriteError(err, what)
	}
	return raw.toFile()
}

func (s *SQLiteStore) scanFiles(rows *sql.Rows) ([]File, error) {
	var out []File
	for rows.Next() {
		f, err := s.scanFile(rows, "scan file")
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan files: %w", err)
	}
	if out == nil {
		out = []File{}
	}
	return out, nil
}

func (s *SQLiteStore) scanShard(row interface{ Scan(...any) error }, what string) (Shard, error) {
	var raw shardRow
	if err := row.Scan(&raw.id, &raw.fileID, &raw.idx, &raw.accountID, &raw.providerID, &raw.sizeBytes,
		&raw.plainSize, &raw.plainOffset, &raw.sha256, &raw.createdAt); err != nil {
		return Shard{}, mapSQLiteWriteError(err, what)
	}
	return raw.toShard()
}

func (s *SQLiteStore) scanShards(rows *sql.Rows) ([]Shard, error) {
	var out []Shard
	for rows.Next() {
		sh, err := s.scanShard(rows, "scan shard")
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan shards: %w", err)
	}
	if out == nil {
		out = []Shard{}
	}
	return out, nil
}

func (s *SQLiteStore) fmt(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

type folderRow struct {
	id, userID, name, createdAt, updatedAt string
	parentID, deletedAt                    *string
}

func (r folderRow) toFolder() (Folder, error) {
	id, err := uuid.Parse(r.id)
	if err != nil {
		return Folder{}, fmt.Errorf("parse folder id %q: %w", r.id, err)
	}
	userID, err := uuid.Parse(r.userID)
	if err != nil {
		return Folder{}, fmt.Errorf("parse folder user id: %w", err)
	}
	parentID, err := parseUUIDPtr(r.parentID)
	if err != nil {
		return Folder{}, fmt.Errorf("parse folder parent id: %w", err)
	}
	createdAt, err := parseSQLiteTime(r.createdAt)
	if err != nil {
		return Folder{}, fmt.Errorf("parse folder created_at: %w", err)
	}
	updatedAt, err := parseSQLiteTime(r.updatedAt)
	if err != nil {
		return Folder{}, fmt.Errorf("parse folder updated_at: %w", err)
	}
	deletedAt, err := parseSQLiteTimePtr(r.deletedAt)
	if err != nil {
		return Folder{}, fmt.Errorf("parse folder deleted_at: %w", err)
	}
	return Folder{
		ID: id, UserID: userID, ParentID: parentID, Name: r.name,
		CreatedAt: createdAt, UpdatedAt: updatedAt, DeletedAt: deletedAt,
	}, nil
}

type fileRow struct {
	id, userID, name, declaredMime, status, createdAt, updatedAt string
	folderID, deletedAt                                          *string
	sizeBytes                                                    int64
	contentSHA                                                   []byte
	isStriped, isEncrypted                                       int64
}

func (r fileRow) toFile() (File, error) {
	id, err := uuid.Parse(r.id)
	if err != nil {
		return File{}, fmt.Errorf("parse file id %q: %w", r.id, err)
	}
	userID, err := uuid.Parse(r.userID)
	if err != nil {
		return File{}, fmt.Errorf("parse file user id: %w", err)
	}
	folderID, err := parseUUIDPtr(r.folderID)
	if err != nil {
		return File{}, fmt.Errorf("parse file folder id: %w", err)
	}
	createdAt, err := parseSQLiteTime(r.createdAt)
	if err != nil {
		return File{}, fmt.Errorf("parse file created_at: %w", err)
	}
	updatedAt, err := parseSQLiteTime(r.updatedAt)
	if err != nil {
		return File{}, fmt.Errorf("parse file updated_at: %w", err)
	}
	deletedAt, err := parseSQLiteTimePtr(r.deletedAt)
	if err != nil {
		return File{}, fmt.Errorf("parse file deleted_at: %w", err)
	}
	return File{
		ID: id, UserID: userID, FolderID: folderID, Name: r.name,
		SizeBytes: r.sizeBytes, DeclaredMime: r.declaredMime, ContentSHA: r.contentSHA,
		IsStriped: r.isStriped != 0, IsEncrypted: r.isEncrypted != 0, Status: r.status,
		CreatedAt: createdAt, UpdatedAt: updatedAt, DeletedAt: deletedAt,
	}, nil
}

type shardRow struct {
	id, fileID, providerID, createdAt string
	accountID                         *string
	idx                               int32
	sizeBytes, plainSize, plainOffset int64
	sha256                            []byte
}

func (r shardRow) toShard() (Shard, error) {
	id, err := uuid.Parse(r.id)
	if err != nil {
		return Shard{}, fmt.Errorf("parse shard id %q: %w", r.id, err)
	}
	fileID, err := uuid.Parse(r.fileID)
	if err != nil {
		return Shard{}, fmt.Errorf("parse shard file id: %w", err)
	}
	accountID, err := parseUUIDPtr(r.accountID)
	if err != nil {
		return Shard{}, fmt.Errorf("parse shard account id: %w", err)
	}
	createdAt, err := parseSQLiteTime(r.createdAt)
	if err != nil {
		return Shard{}, fmt.Errorf("parse shard created_at: %w", err)
	}
	return Shard{
		ID: id, FileID: fileID, Index: r.idx, AccountID: accountID, ProviderID: r.providerID,
		SizeBytes: r.sizeBytes, PlainSize: r.plainSize, PlainOffset: r.plainOffset,
		SHA256: r.sha256, CreatedAt: createdAt,
	}, nil
}

func uuidPtrToString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	v := id.String()
	return &v
}

func parseUUIDPtr(s *string) (*uuid.UUID, error) {
	if s == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func parseSQLiteTime(s string) (time.Time, error) {
	return time.Parse(sqliteTimeLayout, s)
}

func parseSQLiteTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := parseSQLiteTime(*s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func normalizeListArgs(args []any) []any {
	if len(args) < 2 {
		return args
	}
	return append([]any{args[0], args[1], args[1]}, args[2:]...)
}

func mapSQLiteWriteError(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return skerr.ErrNotFound
	}
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return skerr.ErrConflict
	}
	return fmt.Errorf("%s: %w", what, err)
}

var _ Store = (*SQLiteStore)(nil)
