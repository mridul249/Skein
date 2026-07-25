package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
)

// PlannedShard is one entry in an upload plan.
type PlannedShard struct {
	// AccountID is the connected account this shard goes to. nil means the
	// deployment has no connected drives and the local backend is in use.
	AccountID *uuid.UUID
	// PlainSize is how many plaintext bytes this shard takes. The tail
	// shard is short.
	PlainSize int64
	// PlainOffset is where this shard's first byte sits in the whole file.
	PlainOffset int64
}

// Plan is an ordered shard layout for one upload.
type Plan struct {
	UserID uuid.UUID
	Shards []PlannedShard
}

// Planner decides how an upload is laid out across accounts. It is an
// interface so Phase 3 can ship a single-account planner and Phase 5 can
// replace it with the striping one without touching the upload path.
type Planner interface {
	Plan(ctx context.Context, userID uuid.UUID, size int64) (Plan, error)
}

// BackendResolver hands out a storage.Backend for an account.
type BackendResolver interface {
	For(ctx context.Context, userID uuid.UUID, accountID *uuid.UUID) (storage.Backend, error)
}

// Service implements the file and folder use cases.
type Service struct {
	store    Store
	planner  Planner
	backends BackendResolver
	keyring  *skcrypto.Keyring
	log      *slog.Logger

	encrypt        bool
	maxUploadBytes int64
}

// Config configures the files service.
type Config struct {
	Encrypt        bool
	MaxUploadBytes int64
}

// NewService wires the files service.
func NewService(
	store Store,
	planner Planner,
	backends BackendResolver,
	keyring *skcrypto.Keyring,
	cfg Config,
	log *slog.Logger,
) *Service {
	return &Service{
		store:          store,
		planner:        planner,
		backends:       backends,
		keyring:        keyring,
		log:            log,
		encrypt:        cfg.Encrypt,
		maxUploadBytes: cfg.MaxUploadBytes,
	}
}

// wrapForStorage wraps one shard's plaintext stream in whatever transform is
// applied before it reaches the provider, and reports how many bytes the
// provider will receive.
//
// Encryption is not wired in this phase, so the two numbers are equal and the
// reader is passed through untouched. The seam exists now because the provider
// needs an authoritative byte count up front — a resumable session declares its
// length before the first byte — and retrofitting a size transform into a path
// that has already assumed plainSize == storedSize is exactly the kind of
// change that gets a byte-count check quietly deleted.
func (s *Service) wrapForStorage(_ uuid.UUID, _ int32, r io.Reader, plainSize int64) (io.Reader, int64, error) {
	return r, plainSize, nil
}

// Get returns one file with its manifest.
func (s *Service) Get(ctx context.Context, userID, fileID uuid.UUID) (File, error) {
	file, err := s.store.GetFile(ctx, userID, fileID)
	if err != nil {
		return File{}, err
	}
	shards, err := s.store.ListShards(ctx, file.ID)
	if err != nil {
		return File{}, fmt.Errorf("load manifest: %w", err)
	}
	file.Shards = shards
	return file, nil
}

// List returns a page of files in a folder.
func (s *Service) List(ctx context.Context, userID uuid.UUID, p ListParams) ([]File, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	out, err := s.store.ListFiles(ctx, userID, p)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	return out, nil
}

// ListTrashed returns soft-deleted files.
func (s *Service) ListTrashed(ctx context.Context, userID uuid.UUID, limit int32) ([]File, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out, err := s.store.ListTrashed(ctx, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list trash: %w", err)
	}
	return out, nil
}

// Rename changes a file's name and/or folder.
func (s *Service) Rename(ctx context.Context, userID, fileID uuid.UUID, newName *string, newFolder **uuid.UUID) (File, error) {
	current, err := s.store.GetFile(ctx, userID, fileID)
	if err != nil {
		return File{}, err
	}

	name := current.Name
	if newName != nil {
		cleaned, cerr := CleanName(*newName)
		if cerr != nil {
			return File{}, cerr
		}
		name = cleaned
	}

	folder := current.FolderID
	if newFolder != nil {
		folder = *newFolder
		if folder != nil {
			// The destination must exist and belong to the caller.
			if _, ferr := s.store.GetFolder(ctx, userID, *folder); ferr != nil {
				if errors.Is(ferr, skerr.ErrNotFound) {
					return File{}, skerr.Public(skerr.ErrNotFound, "That folder does not exist.")
				}
				return File{}, fmt.Errorf("check destination folder: %w", ferr)
			}
		}
	}

	updated, err := s.store.UpdateFile(ctx, userID, fileID, name, folder)
	if err != nil {
		if errors.Is(err, skerr.ErrConflict) {
			return File{}, skerr.Public(skerr.ErrConflict, "A file called %q is already there.", name)
		}
		return File{}, fmt.Errorf("update file: %w", err)
	}
	return updated, nil
}

// Trash soft-deletes a file. The provider objects stay put: trash has to be
// undoable, and deleting the bytes would make it a one-way operation wearing
// the wrong name.
func (s *Service) Trash(ctx context.Context, userID, fileID uuid.UUID) error {
	n, err := s.store.SoftDeleteFile(ctx, userID, fileID)
	if err != nil {
		return fmt.Errorf("trash file: %w", err)
	}
	if n == 0 {
		return skerr.ErrNotFound
	}
	return nil
}

// Restore brings a file back from trash.
func (s *Service) Restore(ctx context.Context, userID, fileID uuid.UUID) error {
	n, err := s.store.RestoreFile(ctx, userID, fileID)
	if err != nil {
		return fmt.Errorf("restore file: %w", err)
	}
	if n == 0 {
		return skerr.ErrNotFound
	}
	return nil
}

// Delete permanently removes a file and every object behind it.
//
// Provider deletions happen before the row is removed. The other order would
// leave objects that nothing in the database points at — invisible, permanent
// quota consumption with no way to find them again.
func (s *Service) Delete(ctx context.Context, userID, fileID uuid.UUID) error {
	file, err := s.Get(ctx, userID, fileID)
	if err != nil {
		// A trashed file is still deletable, so fall back to a read that
		// includes it.
		return s.deleteTrashed(ctx, userID, fileID, err)
	}
	return s.deleteWithShards(ctx, userID, file)
}

func (s *Service) deleteTrashed(ctx context.Context, userID, fileID uuid.UUID, cause error) error {
	if !errors.Is(cause, skerr.ErrNotFound) {
		return cause
	}
	// The row may exist but be trashed; ListShards does not filter on
	// deleted_at, so the manifest is still reachable for cleanup.
	shards, err := s.store.ListShards(ctx, fileID)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if len(shards) == 0 {
		n, derr := s.store.HardDeleteFile(ctx, userID, fileID)
		if derr != nil {
			return fmt.Errorf("delete file row: %w", derr)
		}
		if n == 0 {
			return skerr.ErrNotFound
		}
		return nil
	}
	return s.deleteWithShards(ctx, userID, File{ID: fileID, UserID: userID, Shards: shards})
}

func (s *Service) deleteWithShards(ctx context.Context, userID uuid.UUID, file File) error {
	var failed int
	for _, sh := range file.Shards {
		backend, err := s.backends.For(ctx, userID, sh.AccountID)
		if err != nil {
			// The account is gone. There is nothing left to call, and
			// refusing to delete the row would strand the file forever.
			s.log.WarnContext(ctx, "shard's drive is unavailable; deleting the record anyway",
				slog.String("file_id", file.ID.String()),
				slog.Int("shard", int(sh.Index)))
			failed++
			continue
		}
		ref := storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}
		if err := backend.Delete(ctx, ref); err != nil {
			s.log.ErrorContext(ctx, "could not delete shard from its drive",
				slog.String("file_id", file.ID.String()),
				slog.Int("shard", int(sh.Index)),
				slog.String("error", err.Error()))
			failed++
		}
	}

	if failed > 0 && failed == len(file.Shards) {
		// Nothing was removed at the provider. Keeping the row means the
		// user can retry rather than losing track of the objects.
		return skerr.Public(skerr.ErrUnavailable,
			"Could not reach the drives holding this file. Nothing was deleted.")
	}

	if _, err := s.store.DeleteShards(ctx, file.ID); err != nil {
		return fmt.Errorf("delete manifest: %w", err)
	}
	n, err := s.store.HardDeleteFile(ctx, userID, file.ID)
	if err != nil {
		return fmt.Errorf("delete file row: %w", err)
	}
	if n == 0 {
		return skerr.ErrNotFound
	}
	return nil
}
