// Package files owns the file and folder domain: the streaming upload path,
// the range-aware download path, and the metadata operations around them.
//
// The upload and download paths are the product thesis. Neither of them ever
// holds more than a fixed buffer of user content, regardless of file size, and
// there is a test that fails if that stops being true.
package files

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// File status values.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusFailed  = "failed"
)

// Folder is a virtual folder. Folders exist only in Skein's database; nothing
// resembling this tree is created at the provider.
type Folder struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ParentID  *uuid.UUID
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// File is one stored file.
type File struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	FolderID     *uuid.UUID
	Name         string
	SizeBytes    int64
	DeclaredMime string
	ContentSHA   []byte
	IsStriped    bool
	IsEncrypted  bool
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time

	// Shards is the manifest, ordered by index. It is the source of truth
	// for where the bytes are; a file whose manifest is incomplete is
	// unreadable and says so rather than returning partial data.
	Shards []Shard
}

// Shard is one contiguous piece of a file at one provider account.
type Shard struct {
	ID          uuid.UUID
	FileID      uuid.UUID
	Index       int32
	AccountID   *uuid.UUID
	ProviderID  string
	SizeBytes   int64
	PlainSize   int64
	PlainOffset int64
	SHA256      []byte
	CreatedAt   time.Time
}

// NewFile is the input to Store.CreateFile.
type NewFile struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	FolderID     *uuid.UUID
	Name         string
	SizeBytes    int64
	DeclaredMime string
	IsStriped    bool
	IsEncrypted  bool
}

// NewShard is the input to Store.CreateShard.
type NewShard struct {
	ID          uuid.UUID
	FileID      uuid.UUID
	Index       int32
	AccountID   *uuid.UUID
	ProviderID  string
	SizeBytes   int64
	PlainSize   int64
	PlainOffset int64
	SHA256      []byte
}

// ListParams is a keyset-paginated listing request.
type ListParams struct {
	FolderID *uuid.UUID
	Limit    int32
	// Cursor is the last item of the previous page; nil starts at the top.
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
}

// Store is the persistence the files service needs. Every method that reads a
// user-owned row takes a user id and filters on it in SQL, per Rules.md §2.7 —
// there is deliberately no "fetch then check ownership" shape anywhere here.
type Store interface {
	CreateFolder(ctx context.Context, id, userID uuid.UUID, parentID *uuid.UUID, name string) (Folder, error)
	GetFolder(ctx context.Context, userID, id uuid.UUID) (Folder, error)
	ListFolders(ctx context.Context, userID uuid.UUID) ([]Folder, error)
	UpdateFolder(ctx context.Context, userID, id uuid.UUID, name string, parentID *uuid.UUID) (Folder, error)
	FolderDescendants(ctx context.Context, userID, id uuid.UUID) ([]uuid.UUID, error)
	SoftDeleteFolderTree(ctx context.Context, userID, id uuid.UUID) (int64, error)

	CreateFile(ctx context.Context, n NewFile) (File, error)
	MarkFileReady(ctx context.Context, userID, id uuid.UUID, size int64, sha []byte) (File, error)
	MarkFileFailed(ctx context.Context, id uuid.UUID) error
	GetFile(ctx context.Context, userID, id uuid.UUID) (File, error)
	ListFiles(ctx context.Context, userID uuid.UUID, p ListParams) ([]File, error)
	ListTrashed(ctx context.Context, userID uuid.UUID, limit int32) ([]File, error)
	UpdateFile(ctx context.Context, userID, id uuid.UUID, name string, folderID *uuid.UUID) (File, error)
	SoftDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error)
	RestoreFile(ctx context.Context, userID, id uuid.UUID) (int64, error)
	HardDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error)

	CreateShard(ctx context.Context, n NewShard) (Shard, error)
	ListShards(ctx context.Context, fileID uuid.UUID) ([]Shard, error)
	DeleteShards(ctx context.Context, fileID uuid.UUID) (int64, error)
}
