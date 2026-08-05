package files

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mridul249/Skein/internal/db/gen"
	"github.com/mridul249/Skein/internal/skerr"
)

const pgUniqueViolation = "23505"

// PGStore is the Postgres implementation of Store.
type PGStore struct {
	q  *gen.Queries
	db gen.DBTX
}

// NewPGStore wraps a pgx connection pool.
func NewPGStore(db gen.DBTX) *PGStore { return &PGStore{q: gen.New(db), db: db} }

// CreateFolder inserts a folder.
func (s *PGStore) CreateFolder(ctx context.Context, id, userID uuid.UUID, parentID *uuid.UUID, name string) (Folder, error) {
	row, err := s.q.CreateFolder(ctx, gen.CreateFolderParams{
		ID: id, UserID: userID, ParentID: parentID, Name: name,
	})
	if err != nil {
		return Folder{}, mapWriteError(err, "insert folder")
	}
	return toFolder(row), nil
}

// GetFolder loads one folder the user owns.
func (s *PGStore) GetFolder(ctx context.Context, userID, id uuid.UUID) (Folder, error) {
	row, err := s.q.GetFolder(ctx, gen.GetFolderParams{ID: id, UserID: userID})
	if err != nil {
		return Folder{}, mapNoRows(err, "select folder")
	}
	return toFolder(row), nil
}

// ListFolders returns the caller's whole tree.
func (s *PGStore) ListFolders(ctx context.Context, userID uuid.UUID) ([]Folder, error) {
	rows, err := s.q.ListFolders(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	out := make([]Folder, 0, len(rows))
	for _, r := range rows {
		out = append(out, toFolder(r))
	}
	return out, nil
}

// UpdateFolder renames and/or moves a folder.
func (s *PGStore) UpdateFolder(ctx context.Context, userID, id uuid.UUID, name string, parentID *uuid.UUID) (Folder, error) {
	row, err := s.q.RenameFolder(ctx, gen.RenameFolderParams{
		ID: id, UserID: userID, Name: name, ParentID: parentID,
	})
	if err != nil {
		return Folder{}, mapWriteError(err, "update folder")
	}
	return toFolder(row), nil
}

// FolderDescendants returns every folder id in a subtree, including the root.
func (s *PGStore) FolderDescendants(ctx context.Context, userID, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.FolderDescendants(ctx, gen.FolderDescendantsParams{ID: id, UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("select folder subtree: %w", err)
	}
	return rows, nil
}

// SoftDeleteFolderTree trashes a folder, its subfolders and their files.
//
// The two statements run in one transaction so a crash between them cannot
// leave files visible inside a folder that is already gone.
func (s *PGStore) SoftDeleteFolderTree(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	pool, ok := s.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		// No transaction support on this handle: run them sequentially.
		return s.softDeleteFolderTreeSeq(ctx, s.q, userID, id)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin folder delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	n, err := s.softDeleteFolderTreeSeq(ctx, s.q.WithTx(tx), userID, id)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit folder delete: %w", err)
	}
	return n, nil
}

func (s *PGStore) softDeleteFolderTreeSeq(ctx context.Context, q *gen.Queries, userID, id uuid.UUID) (int64, error) {
	// Files first: the folder query flips deleted_at on the folders, and the
	// file query walks the same tree. Doing folders first would still work
	// because the recursive term does not filter on deleted_at, but ordering
	// it this way keeps the intent obvious.
	if _, err := q.SoftDeleteFilesInFolderTree(ctx, gen.SoftDeleteFilesInFolderTreeParams{
		ID: id, UserID: userID,
	}); err != nil {
		return 0, fmt.Errorf("trash files in folder: %w", err)
	}
	n, err := q.SoftDeleteFolder(ctx, gen.SoftDeleteFolderParams{ID: id, UserID: userID})
	if err != nil {
		return 0, fmt.Errorf("trash folder: %w", err)
	}
	return n, nil
}

// CreateFile inserts a pending file row.
func (s *PGStore) CreateFile(ctx context.Context, n NewFile) (File, error) {
	row, err := s.q.CreateFile(ctx, gen.CreateFileParams{
		ID:           n.ID,
		UserID:       n.UserID,
		FolderID:     n.FolderID,
		Name:         n.Name,
		SizeBytes:    n.SizeBytes,
		DeclaredMime: n.DeclaredMime,
		IsStriped:    n.IsStriped,
		IsEncrypted:  n.IsEncrypted,
	})
	if err != nil {
		return File{}, mapWriteError(err, "insert file")
	}
	return toFile(row), nil
}

// MarkFileReady commits a file once every shard is stored.
func (s *PGStore) MarkFileReady(ctx context.Context, userID, id uuid.UUID, size int64, sha []byte) (File, error) {
	row, err := s.q.MarkFileReady(ctx, gen.MarkFileReadyParams{
		ID: id, UserID: userID, SizeBytes: size, ContentSha256: sha,
	})
	if err != nil {
		return File{}, mapNoRows(err, "mark file ready")
	}
	return toFile(row), nil
}

// RecordReconciledHealth writes a COMPLETE reconcile run's finding for one
// file. The status predicates live in SQL; see the query's comment.
func (s *PGStore) RecordReconciledHealth(ctx context.Context, userID, id uuid.UUID, status string, at time.Time) error {
	if !IsListable(status) {
		return fmt.Errorf("refusing to record non-reconciled status %q", status)
	}
	if _, err := s.q.RecordReconciledHealth(ctx, gen.RecordReconciledHealthParams{
		ID:           id,
		UserID:       userID,
		Status:       status,
		ReconciledAt: nullTS(&at),
	}); err != nil {
		return fmt.Errorf("record reconciled health: %w", err)
	}
	return nil
}

// MarkFileFailed records that an upload did not finish.
func (s *PGStore) MarkFileFailed(ctx context.Context, id uuid.UUID) error {
	if err := s.q.MarkFileFailed(ctx, id); err != nil {
		return fmt.Errorf("mark file failed: %w", err)
	}
	return nil
}

// GetFile loads one file the user owns.
func (s *PGStore) GetFile(ctx context.Context, userID, id uuid.UUID) (File, error) {
	row, err := s.q.GetFile(ctx, gen.GetFileParams{ID: id, UserID: userID})
	if err != nil {
		return File{}, mapNoRows(err, "select file")
	}
	return toFile(row), nil
}

// ListFiles returns a keyset-paginated page, with every manifest fetched in
// one extra query rather than one per row.
func (s *PGStore) ListFiles(ctx context.Context, userID uuid.UUID, p ListParams) ([]File, error) {
	rows, err := s.q.ListFiles(ctx, gen.ListFilesParams{
		UserID:          userID,
		FolderID:        p.FolderID,
		Limit:           p.Limit,
		CursorCreatedAt: nullTS(p.CursorCreatedAt),
		CursorID:        p.CursorID,
	})
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	return s.attachShards(ctx, mapFiles(rows))
}

// ListTrashed returns soft-deleted files.
func (s *PGStore) ListTrashed(ctx context.Context, userID uuid.UUID, limit int32) ([]File, error) {
	rows, err := s.q.ListTrashedFiles(ctx, gen.ListTrashedFilesParams{UserID: userID, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list trashed files: %w", err)
	}
	return s.attachShards(ctx, mapFiles(rows))
}

// attachShards loads manifests for a page of files with one query.
// Rules.md §2.12: no query in a loop.
func (s *PGStore) attachShards(ctx context.Context, list []File) ([]File, error) {
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, f := range list {
		ids = append(ids, f.ID)
	}

	rows, err := s.q.ListShardsForFiles(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list shards for files: %w", err)
	}

	byFile := make(map[uuid.UUID][]Shard, len(list))
	for _, r := range rows {
		byFile[r.FileID] = append(byFile[r.FileID], toShard(r))
	}
	for i := range list {
		list[i].Shards = byFile[list[i].ID]
	}
	return list, nil
}

// UpdateFile renames and/or moves a file.
func (s *PGStore) UpdateFile(ctx context.Context, userID, id uuid.UUID, name string, folderID *uuid.UUID) (File, error) {
	row, err := s.q.UpdateFile(ctx, gen.UpdateFileParams{
		ID: id, UserID: userID, Name: name, FolderID: folderID,
	})
	if err != nil {
		return File{}, mapWriteError(err, "update file")
	}
	return toFile(row), nil
}

// SoftDeleteFile moves a file to trash.
func (s *PGStore) SoftDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	n, err := s.q.SoftDeleteFile(ctx, gen.SoftDeleteFileParams{ID: id, UserID: userID})
	if err != nil {
		return 0, fmt.Errorf("trash file: %w", err)
	}
	return n, nil
}

// RestoreFile brings a file back from trash.
func (s *PGStore) RestoreFile(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	n, err := s.q.RestoreFile(ctx, gen.RestoreFileParams{ID: id, UserID: userID})
	if err != nil {
		return 0, fmt.Errorf("restore file: %w", err)
	}
	return n, nil
}

// HardDeleteFile removes the row permanently.
func (s *PGStore) HardDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	n, err := s.q.HardDeleteFile(ctx, gen.HardDeleteFileParams{ID: id, UserID: userID})
	if err != nil {
		return 0, fmt.Errorf("delete file: %w", err)
	}
	return n, nil
}

// CreateShard records one manifest entry.
func (s *PGStore) CreateShard(ctx context.Context, n NewShard) (Shard, error) {
	row, err := s.q.CreateFileShard(ctx, gen.CreateFileShardParams{
		ID:                 n.ID,
		FileID:             n.FileID,
		Idx:                n.Index,
		ConnectedAccountID: n.AccountID,
		ProviderObjectID:   n.ProviderID,
		SizeBytes:          n.SizeBytes,
		PlainSizeBytes:     n.PlainSize,
		PlainOffset:        n.PlainOffset,
		Sha256:             n.SHA256,
	})
	if err != nil {
		return Shard{}, mapWriteError(err, "insert shard")
	}
	return toShard(row), nil
}

// ListShards returns a file's manifest in index order.
func (s *PGStore) ListShards(ctx context.Context, fileID uuid.UUID) ([]Shard, error) {
	rows, err := s.q.ListFileShards(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	out := make([]Shard, 0, len(rows))
	for _, r := range rows {
		out = append(out, toShard(r))
	}
	return out, nil
}

// DeleteShards removes a file's manifest.
func (s *PGStore) DeleteShards(ctx context.Context, fileID uuid.UUID) (int64, error) {
	n, err := s.q.DeleteFileShards(ctx, fileID)
	if err != nil {
		return 0, fmt.Errorf("delete shards: %w", err)
	}
	return n, nil
}

func mapFiles(rows []gen.File) []File {
	out := make([]File, 0, len(rows))
	for _, r := range rows {
		out = append(out, toFile(r))
	}
	return out
}

func toFolder(r gen.Folder) Folder {
	return Folder{
		ID:        r.ID,
		UserID:    r.UserID,
		ParentID:  r.ParentID,
		Name:      r.Name,
		CreatedAt: r.CreatedAt.Time,
		UpdatedAt: r.UpdatedAt.Time,
		DeletedAt: nullableTime(r.DeletedAt),
	}
}

func toFile(r gen.File) File {
	return File{
		ID:           r.ID,
		UserID:       r.UserID,
		FolderID:     r.FolderID,
		Name:         r.Name,
		SizeBytes:    r.SizeBytes,
		DeclaredMime: r.DeclaredMime,
		ContentSHA:   r.ContentSha256,
		IsStriped:    r.IsStriped,
		IsEncrypted:  r.IsEncrypted,
		Status:       r.Status,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
		DeletedAt:    nullableTime(r.DeletedAt),
		ReconciledAt: nullableTime(r.ReconciledAt),
	}
}

func toShard(r gen.FileShard) Shard {
	return Shard{
		ID:          r.ID,
		FileID:      r.FileID,
		Index:       r.Idx,
		AccountID:   r.ConnectedAccountID,
		ProviderID:  r.ProviderObjectID,
		SizeBytes:   r.SizeBytes,
		PlainSize:   r.PlainSizeBytes,
		PlainOffset: r.PlainOffset,
		SHA256:      r.Sha256,
		CreatedAt:   r.CreatedAt.Time,
	}
}

func mapNoRows(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return skerr.ErrNotFound
	}
	return fmt.Errorf("%s: %w", what, err)
}

func mapWriteError(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return skerr.ErrConflict
	}
	return mapNoRows(err, what)
}

func nullableTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func nullTS(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// Compile-time check.
var _ Store = (*PGStore)(nil)
