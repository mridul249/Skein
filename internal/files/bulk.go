package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
)

// maxBulkFiles caps one bulk request. Without a bound, a single call can pin
// the Drive pool for an unbounded time and hold a request open with it.
const maxBulkFiles = 200

// BulkResult is the outcome for ONE file in a bulk operation.
//
// Per-file rather than one aggregate status because the aggregate is unusable:
// "207, some of it worked" leaves the client unable to say which rows to
// remove from the view, and a retry of the whole set re-deletes what already
// succeeded.
type BulkResult struct {
	FileID uuid.UUID `json:"file_id"`
	OK     bool      `json:"ok"`
	// Error is a user-safe message, empty when OK.
	//
	// It never contains the file NAME. Shard object names key on the file
	// UUID and the user's filename never reaches the provider (see
	// storage.NameForShard); echoing a name into a bulk result — or a log
	// line — would undo that for the sake of a nicer error string.
	Error string `json:"error,omitempty"`
	// Code is the machine-readable reason, matching the API error codes.
	Code string `json:"code,omitempty"`
}

// BulkDelete moves many files to the trash, returning one result per file.
//
// TRASHES BY DEFAULT, exactly as DELETE /api/files/{id} does. Permanent
// destruction requires BulkPurge, which is the analogue of ?permanent=true.
// The first version of this method called Delete directly and destroyed shards
// with no way back — a multi-select in the file list is not a request to
// permanently erase anything, and the single-file route has always made that
// distinction.
//
// Ownership is checked per file. Block 3c established that scoping is sound —
// every store read takes userID and returns ErrNotFound otherwise — so this is
// applying an existing property rather than establishing one: each file goes
// through the same path a single-file request would.
func (s *Service) BulkDelete(ctx context.Context, userID uuid.UUID, fileIDs []uuid.UUID) ([]BulkResult, error) {
	return s.bulk(ctx, userID, fileIDs, s.Trash)
}

// BulkPurge permanently deletes many files, removing their shards from the
// drives. This is the trash view's "delete permanently" and empty-trash path.
//
// Shard deletions run through the shared Drive pool, so a fifty-file purge is
// bounded and retries 429s rather than stampeding.
func (s *Service) BulkPurge(ctx context.Context, userID uuid.UUID, fileIDs []uuid.UUID) ([]BulkResult, error) {
	return s.bulk(ctx, userID, fileIDs, s.Delete)
}

func (s *Service) bulk(
	ctx context.Context,
	userID uuid.UUID,
	fileIDs []uuid.UUID,
	op func(ctx context.Context, userID, fileID uuid.UUID) error,
) ([]BulkResult, error) {
	if len(fileIDs) == 0 {
		return nil, skerr.Invalid(map[string]string{
			"file_ids": "Select at least one file.",
		})
	}
	if len(fileIDs) > maxBulkFiles {
		return nil, skerr.Invalid(map[string]string{
			"file_ids": fmt.Sprintf("Select at most %d files at a time.", maxBulkFiles),
		})
	}

	// De-duplicate. A repeated id would otherwise produce a second result
	// reporting "not found" for a file this very call just deleted.
	seen := make(map[uuid.UUID]bool, len(fileIDs))
	unique := make([]uuid.UUID, 0, len(fileIDs))
	for _, id := range fileIDs {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}

	results := make([]BulkResult, len(unique))
	var wg sync.WaitGroup

	for i, id := range unique {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.applyOne(ctx, userID, id, op)
		}()
	}
	wg.Wait()

	return results, nil
}

// applyOne runs one file's operation and converts the outcome to a result.
//
// Errors are classified rather than stringified: the client needs to tell "you
// do not own that" from "a drive is rate limiting, try again" from "the grant
// died", and only the last two are worth retrying.
func (s *Service) applyOne(
	ctx context.Context,
	userID, fileID uuid.UUID,
	op func(ctx context.Context, userID, fileID uuid.UUID) error,
) BulkResult {
	res := BulkResult{FileID: fileID}

	err := op(ctx, userID, fileID)
	if err == nil {
		res.OK = true
		return res
	}

	// Deliberately logs the file ID and never the name.
	s.log.WarnContext(ctx, "bulk operation: file failed",
		slog.String("file_id", fileID.String()),
		slog.String("error", err.Error()))

	switch {
	case errors.Is(err, skerr.ErrNotFound):
		res.Code, res.Error = "not_found", "That file no longer exists."
	case errors.Is(err, skerr.ErrDriveNeedsReconnect):
		res.Code, res.Error = "drive_needs_reauth", "A drive holding this file needs reconnecting."
	case errors.Is(err, skerr.ErrRateLimited):
		res.Code, res.Error = "rate_limited", "A drive is rate limiting. Try this file again shortly."
	default:
		res.Code, res.Error = "failed", "Could not delete this file."
	}
	return res
}

// EmptyTrash permanently deletes every trashed file the user owns.
//
// Same machinery as BulkDelete: same pool, same per-file results, same
// ownership scoping. ListTrashed is already user-scoped, so the ids fed in
// cannot belong to anybody else.
func (s *Service) EmptyTrash(ctx context.Context, userID uuid.UUID) ([]BulkResult, error) {
	trashed, err := s.store.ListTrashed(ctx, userID, maxBulkFiles)
	if err != nil {
		return nil, fmt.Errorf("list trashed: %w", err)
	}
	if len(trashed) == 0 {
		return []BulkResult{}, nil
	}

	ids := make([]uuid.UUID, 0, len(trashed))
	for _, f := range trashed {
		ids = append(ids, f.ID)
	}
	// Purge, not trash: these files are already trashed.
	return s.BulkPurge(ctx, userID, ids)
}
