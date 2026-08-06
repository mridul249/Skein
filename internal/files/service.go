package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
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
	// UploadID identifies the capacity reservations this plan holds. It is
	// uuid.Nil for a planner that does not reserve.
	UploadID uuid.UUID
	Shards   []PlannedShard
}

// Planner decides how an upload is laid out across accounts. It is an
// interface so Phase 3 can ship a single-account planner and Phase 5 can
// replace it with the striping one without touching the upload path.
type Planner interface {
	Plan(ctx context.Context, userID uuid.UUID, size int64) (Plan, error)
}

// ReleasingPlanner is a Planner that holds capacity reservations and needs to
// be told when an upload is over. The upload path checks for it rather than
// widening Planner, so the single-shard planner stays a two-line type.
type ReleasingPlanner interface {
	Release(ctx context.Context, uploadID uuid.UUID) error
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

	// pool bounds and retries provider calls made in bulk. Nil means run
	// inline, which is what the single-file paths did before bulk existed and
	// what tests without a pool get.
	pool WorkPool

	// accounts names the drives a user has connected, for reconstruction.
	accounts AccountLister

	// users resolves the durable identity a manifest records.
	users UserDirectory
}

// WorkPool bounds concurrency and retries rate-limited provider calls.
//
// An interface rather than *gdrive.Pool so this package does not import a
// provider: files is provider-agnostic, and a concrete Drive type here would
// make it not so. accounts.Service.Pool() satisfies it.
type WorkPool interface {
	Do(ctx context.Context, fn func(ctx context.Context) error) error
}

// SetWorkPool installs the shared provider pool. Called during wiring.
func (s *Service) SetWorkPool(p WorkPool) { s.pool = p }

// AccountLister names the accounts a user has connected.
//
// A one-method interface for the same reason WorkPool is one: reconstruction
// has to scan every drive the user owns, and this package must not import
// internal/accounts to find out which those are. accounts.Service satisfies it.
type AccountLister interface {
	AccountIDsForUser(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// UserDirectory resolves a user's durable identity — their email address.
//
// A one-method interface for the same reason WorkPool and AccountLister are:
// this package must not import internal/auth. auth.Service satisfies it.
//
// Needed because a manifest has to carry something that survives losing the
// database. User ids are random UUIDs minted at registration; email is what
// the user re-enters when rebuilding, and is UNIQUE per instance.
type UserDirectory interface {
	EmailForUser(ctx context.Context, userID uuid.UUID) (string, error)
	UserIDForEmail(ctx context.Context, email string) (uuid.UUID, error)
}

// SetUserDirectory installs the identity lookup manifests and reconstruction
// need. Nil means manifests carry no email, which makes them unrecoverable
// after a database rebuild — so it is wired in app.go and only tests omit it.
func (s *Service) SetUserDirectory(d UserDirectory) { s.users = d }

// SetAccountLister installs the account source reconstruction scans. Called
// during wiring. Nil means Reconstruct must be given account ids explicitly,
// which is what the tests do.
func (s *Service) SetAccountLister(a AccountLister) { s.accounts = a }

// runPooled runs fn through the pool when one is installed, inline otherwise.
func (s *Service) runPooled(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.pool == nil {
		return fn(ctx)
	}
	return s.pool.Do(ctx, fn)
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
// With encryption on — the default — the reader is the framed AEAD stream and
// the stored size is larger than the plaintext by a header plus one tag per
// 64 KiB frame. The provider needs that number before the first byte, because
// a resumable session commits to its length up front, which is why this
// returns a size rather than discovering one as it goes.
func (s *Service) wrapForStorage(fileID uuid.UUID, shardIndex int32, r io.Reader, plainSize int64) (io.Reader, int64, error) {
	if !s.encrypt {
		return r, plainSize, nil
	}
	if s.keyring == nil {
		return nil, 0, errors.New("files: encryption is enabled but no keyring was wired")
	}

	// The stored size has to be exact and known now: a resumable session
	// commits to its length before the first byte is sent, so it cannot be
	// discovered as the ciphertext is produced.
	enc, err := s.keyring.NewEncryptReader(fileID[:], uint32(shardIndex), r)
	if err != nil {
		return nil, 0, fmt.Errorf("open encrypt reader for shard %d: %w", shardIndex, err)
	}
	return enc, skcrypto.StreamOverhead(plainSize), nil
}

// StoredSizeFor reports how many provider bytes a plaintext shard takes. The
// planner needs it so a reservation covers the ciphertext rather than the
// plaintext; reserving the smaller number runs a drive out of space at the
// final frame.
func (s *Service) StoredSizeFor(plainSize int64) int64 {
	if !s.encrypt {
		return plainSize
	}
	return skcrypto.StreamOverhead(plainSize)
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
		// Through the shared pool: a bulk delete of fifty files is fifty-plus
		// provider calls, and unbounded they are exactly the burst that earns
		// a 429.
		if err := s.runPooled(ctx, func(pctx context.Context) error {
			return backend.Delete(pctx, ref)
		}); err != nil {
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
