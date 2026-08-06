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
//
// pending/ready/failed are the UPLOAD lifecycle. partially_missing and
// corrupted are RECONCILED states: they say something about the shards at the
// provider, not about whether the upload finished, and only a complete
// reconcile run may ever write them (see Service.Reconcile).
//
// The distinction matters to every listing query. A file in a reconciled state
// is a committed file that has been damaged since — it must stay visible and
// marked, never filtered out, or a damaged file silently disappears instead of
// warning its owner. IsListable is that predicate; do not spell it as
// `status == StatusReady` anywhere.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusFailed  = "failed"

	// StatusPartiallyMissing — some shards confirmed gone, some present. The
	// file cannot be read whole, but the surviving shards are still there.
	StatusPartiallyMissing = "partially_missing"

	// StatusCorrupted — every shard confirmed gone. Only the database row is
	// left.
	StatusCorrupted = "corrupted"
)

// IsListable reports whether a status belongs in a user's file listing.
//
// Committed files are listable whatever their health; pending and failed
// uploads are not. Written as a predicate rather than repeated inline because
// there are three listing sites across two dialects plus the memory store, and
// a fourth that disagreed would hide damaged files from exactly one backend.
func IsListable(status string) bool {
	switch status {
	case StatusReady, StatusPartiallyMissing, StatusCorrupted:
		return true
	default:
		return false
	}
}

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

	// ReconciledAt is when this row's health was last established by a
	// COMPLETE reconcile run. Nil means never reconciled, which is different
	// from "reconciled and found healthy" — the latter carries a timestamp
	// alongside StatusReady. An incomplete run must never stamp it: the
	// timestamp is an assertion that the evidence was gathered, and a badge
	// claiming freshness for a scan that never happened is the failure mode
	// persistence introduces.
	ReconciledAt *time.Time

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

// ReconstructedFile is the input to Store.InsertReconstructedFile.
//
// Separate from NewFile because the two describe opposite situations. NewFile
// begins an upload: the store assigns `pending`, and the row becomes `ready`
// only once every byte has arrived. A reconstructed file has ALREADY been
// uploaded — its shards are sitting at the provider right now — so it is
// inserted committed, and its timestamps come from the manifest rather than
// from the clock, or a recovered library would claim every file was created
// the moment it was recovered.
type ReconstructedFile struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	FolderID     *uuid.UUID
	Name         string
	SizeBytes    int64
	DeclaredMime string
	IsStriped    bool
	IsEncrypted  bool
	CreatedAt    time.Time
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

// ListAllPageSize is how many rows ListAllFiles fetches per round trip.
//
// Exported so a test can build a library that provably spans more than one
// page. Silent truncation at a page boundary is the same failure shape as
// issue #50 one level up — an operation reporting itself complete over a
// subset — and it fails at exactly the library size where it matters most.
const ListAllPageSize = 500

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

	// RecordReconciledHealth writes the outcome of a COMPLETE reconcile run:
	// the derived status and the moment it was established.
	//
	// Only ever called for a file every one of whose shards produced a
	// definite answer. An incomplete run must call this for nothing — see
	// Service.Reconcile — because reconciled_at asserts that the evidence was
	// gathered, and a status derived from a partial scan is a guess wearing a
	// fact's clothes.
	//
	// It refuses to move a file out of an upload state: a pending or failed
	// row is mid-upload or dead, and reconcile has no business commenting on
	// either.
	RecordReconciledHealth(ctx context.Context, userID, id uuid.UUID, status string, at time.Time) error
	GetFile(ctx context.Context, userID, id uuid.UUID) (File, error)
	// ListFiles returns ONE PAGE of ONE FOLDER. A nil ListParams.FolderID
	// means the ROOT folder, not "everywhere" — it is the browse query behind
	// GET /api/files.
	//
	// Do not use it to mean "the whole library". See ListAllFiles and known
	// issue #50.
	ListFiles(ctx context.Context, userID uuid.UUID, p ListParams) ([]File, error)

	// ListAllFiles returns every live file the user owns, in every folder, at
	// every depth, across every page.
	//
	// A SEPARATE METHOD RATHER THAN A ListParams FLAG, and the distinction is
	// the whole reason issue #50 happened. With a flag, nil FolderID would
	// have to mean "the root" in one mode and "everywhere" in another — the
	// same field carrying two opposite meanings, distinguished by a second
	// field somebody has to remember to set. Reconcile did not set it, and got
	// the root-only reading while believing it had the library; it reported
	// `complete: true` over 2 of the owner's 20 files.
	//
	// An explicit method cannot be got wrong by omission. Whole-library
	// operations — reconcile, manifest backfill, anything that sweeps — call
	// this; folder browsing calls ListFiles.
	//
	// Implementations page internally and return the whole set: a caller that
	// had to remember to loop would be the same defect one layer along.
	ListAllFiles(ctx context.Context, userID uuid.UUID) ([]File, error)
	ListTrashed(ctx context.Context, userID uuid.UUID, limit int32) ([]File, error)
	UpdateFile(ctx context.Context, userID, id uuid.UUID, name string, folderID *uuid.UUID) (File, error)
	SoftDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error)
	RestoreFile(ctx context.Context, userID, id uuid.UUID) (int64, error)
	HardDeleteFile(ctx context.Context, userID, id uuid.UUID) (int64, error)

	CreateShard(ctx context.Context, n NewShard) (Shard, error)

	// InsertReconstructedFile inserts a file row recovered from a sidecar
	// manifest, and does NOTHING if a row with that id already exists.
	//
	// ADDITIVE ONLY. Reconstruction adds what is missing; it never overwrites
	// what the database has. The database is authoritative where the two
	// disagree, because it holds state a manifest cannot know — a file the
	// user has since renamed, trashed, or that reconcile has marked damaged.
	// Overwriting on a partial Drive scan is how a recovery destroys good
	// data, which is a strictly worse outcome than recovering nothing.
	//
	// It returns whether it inserted, so a run can report what it actually
	// added rather than how many rows it looked at.
	InsertReconstructedFile(ctx context.Context, n ReconstructedFile) (inserted bool, err error)

	// InsertReconstructedShard inserts one shard row, doing nothing if the
	// (file_id, idx) pair already exists. Same additive-only rule.
	InsertReconstructedShard(ctx context.Context, n NewShard) (inserted bool, err error)

	// EnsureFolder returns the id of the folder with this name under this
	// parent, creating it only if no live one exists. Reconstruction rebuilds
	// a folder path segment by segment through this.
	EnsureFolder(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (uuid.UUID, error)
	ListShards(ctx context.Context, fileID uuid.UUID) ([]Shard, error)
	DeleteShards(ctx context.Context, fileID uuid.UUID) (int64, error)
}
