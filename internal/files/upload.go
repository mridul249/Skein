package files

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
)

// UploadRequest describes one incoming file.
//
// Size is what the client declared. It is a claim: the planner uses it to
// choose shards, and the committed size is what actually arrived. A mismatch
// fails the upload and deletes everything it wrote.
type UploadRequest struct {
	UserID       uuid.UUID
	FolderID     *uuid.UUID
	Name         string
	Size         int64
	DeclaredMime string
}

// Upload streams r into storage and commits a manifest.
//
// This is the path the whole product is an argument about, so the shape is
// deliberate:
//
//	r  ──►  TeeReader (SHA-256)  ──►  StreamEncrypter  ──►  ShardWriter  ──►  provider
//
// Every stage is an io.Reader or io.Writer over a fixed buffer. Nothing in it
// grows with file size, nothing calls ReadAll, and no []byte is allocated per
// megabyte. Backpressure is free because io.Copy blocks on whichever side is
// slower; cancellation is free because ctx reaches the outbound request, so a
// client disconnect tears down the provider upload rather than leaking it.
//
// The order matters. The hash is taken over plaintext, before encryption, so
// content_sha256 identifies the file the user uploaded rather than a
// particular encryption of it — which is what makes dedup possible later and
// what makes a round-trip check meaningful.
func (s *Service) Upload(ctx context.Context, req UploadRequest, r io.Reader) (File, error) {
	if err := s.validateUpload(ctx, req); err != nil {
		return File{}, err
	}

	plan, err := s.planner.Plan(ctx, req.UserID, req.Size)
	if err != nil {
		return File{}, err
	}

	fileID := uuid.New()
	file, err := s.store.CreateFile(ctx, NewFile{
		ID:           fileID,
		UserID:       req.UserID,
		FolderID:     req.FolderID,
		Name:         req.Name,
		SizeBytes:    req.Size,
		DeclaredMime: req.DeclaredMime,
		IsStriped:    len(plan.Shards) > 1,
		IsEncrypted:  s.encrypt,
	})
	if err != nil {
		if errors.Is(err, skerr.ErrConflict) {
			return File{}, skerr.Public(skerr.ErrConflict,
				"A file called %q is already here.", req.Name)
		}
		return File{}, fmt.Errorf("create file row: %w", err)
	}

	// Everything written from here is provisional. cleanup runs on every
	// failure path and removes both the provider objects and the rows, so a
	// failed upload leaves no orphan shards behind.
	committed := false
	writtenShards := make([]committedShard, 0, len(plan.Shards))
	defer func() {
		// Reservations are released either way. On success the bytes are
		// used rather than reserved; on failure they were never used at
		// all. Holding them past this point is wrong in both cases, and
		// leaving it to the janitor would strand capacity for half an
		// hour after every single upload.
		s.releasePlan(ctx, plan)

		if committed {
			return
		}
		s.cleanupFailedUpload(ctx, file.ID, writtenShards)
	}()

	digest := sha256.New()
	// TeeReader hashes as the bytes go past. It holds one hash state, not
	// one copy of the file.
	hashed := io.TeeReader(r, digest)

	total, shards, err := s.writeShards(ctx, fileID, plan, hashed, digest)
	writtenShards = shards
	if err != nil {
		return File{}, err
	}

	// Rules.md §2.7: the declared size is verified against what arrived,
	// before anything is marked ready.
	if total != req.Size {
		return File{}, skerr.Public(skerr.ErrValidation,
			"Upload was %s but %s was declared. Nothing was saved.",
			humanBytes(total), humanBytes(req.Size))
	}

	sum := digest.Sum(nil)
	ready, err := s.store.MarkFileReady(ctx, req.UserID, fileID, total, sum)
	if err != nil {
		return File{}, fmt.Errorf("commit file: %w", err)
	}

	committed = true
	ready.Shards = make([]Shard, 0, len(writtenShards))
	for _, c := range writtenShards {
		ready.Shards = append(ready.Shards, c.shard)
	}

	s.log.InfoContext(ctx, "upload committed",
		slog.String("file_id", fileID.String()),
		slog.Int64("bytes", total),
		slog.Int("shards", len(writtenShards)),
		slog.Bool("encrypted", s.encrypt))

	return ready, nil
}

// committedShard pairs a stored manifest row with the reference needed to
// delete the provider object if the upload later fails.
type committedShard struct {
	shard   Shard
	backend storage.Backend
	ref     storage.ObjectRef
}

// writeShards streams r across the planned shards in order.
//
// Each shard reads exactly its planned number of plaintext bytes from the
// shared reader via io.LimitReader, so the boundary between shards is a
// position in the stream rather than a buffer that has to be held anywhere.
func (s *Service) writeShards(
	ctx context.Context,
	fileID uuid.UUID,
	plan Plan,
	r io.Reader,
	_ hash.Hash,
) (int64, []committedShard, error) {
	written := make([]committedShard, 0, len(plan.Shards))
	var total int64

	for i, ps := range plan.Shards {
		backend, err := s.backends.For(ctx, plan.UserID, ps.AccountID)
		if err != nil {
			return total, written, fmt.Errorf("open backend for shard %d: %w", i, err)
		}

		// This shard's slice of the plaintext stream. The tail shard is
		// short, so the count is a maximum rather than a promise.
		limited := &countingReader{r: io.LimitReader(r, ps.PlainSize)}

		// Per-shard checksum over plaintext, taken as the bytes go past.
		// It is what makes an integrity check on read possible without
		// re-reading the whole file, and it localises a corrupt shard to
		// one drive rather than to "somewhere in a 30 GB file".
		shardDigest := sha256.New()
		hashedShard := io.TeeReader(limited, shardDigest)

		body, storedSize, err := s.wrapForStorage(fileID, int32(i), hashedShard, ps.PlainSize)
		if err != nil {
			return total, written, err
		}

		ref, err := backend.Put(ctx, body, storage.ObjectSpec{
			Name:        storage.NameForShard(fileID.String(), i),
			Size:        storedSize,
			ContentType: "application/octet-stream",
		})
		if err != nil {
			total += limited.n
			return total, written, mapStorageError(err, i)
		}

		plainWritten := limited.n
		total += plainWritten

		shard, err := s.store.CreateShard(ctx, NewShard{
			ID:          uuid.New(),
			FileID:      fileID,
			Index:       int32(i),
			AccountID:   ps.AccountID,
			ProviderID:  ref.ProviderID,
			SizeBytes:   ref.Size,
			PlainSize:   plainWritten,
			PlainOffset: ps.PlainOffset,
			SHA256:      shardDigest.Sum(nil),
		})
		if err != nil {
			// The object exists at the provider but the row does not.
			// Record it as written so cleanup removes it.
			written = append(written, committedShard{
				backend: backend, ref: ref,
			})
			return total, written, fmt.Errorf("record shard %d: %w", i, err)
		}

		written = append(written, committedShard{shard: shard, backend: backend, ref: ref})

		// A short read before the last shard means the client sent less
		// than it declared. Stop rather than opening sessions for shards
		// that will never receive anything.
		if plainWritten < ps.PlainSize {
			break
		}
	}

	// Anything still unread means the client sent more than it declared.
	// One byte is enough to know; there is no reason to drain the rest.
	extra := make([]byte, 1)
	if n, _ := io.ReadFull(r, extra); n > 0 {
		total += int64(n)
	}

	return total, written, nil
}

// cleanupFailedUpload removes every object and row a failed upload created.
//
// It runs on a detached context: the usual reason an upload fails is that the
// client vanished, which cancels the request context, and cleanup that skips
// itself exactly when it is most needed is not cleanup.
func (s *Service) cleanupFailedUpload(ctx context.Context, fileID uuid.UUID, shards []committedShard) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	for _, c := range shards {
		if c.backend == nil || c.ref.ProviderID == "" {
			continue
		}
		if err := c.backend.Delete(cleanupCtx, c.ref); err != nil {
			// Logged at Error: this is a real orphan at the provider,
			// consuming quota that nothing will reclaim.
			s.log.ErrorContext(cleanupCtx, "could not delete orphaned shard",
				slog.String("file_id", fileID.String()),
				slog.String("provider_object_id", c.ref.ProviderID),
				slog.String("error", err.Error()))
		}
	}

	if _, err := s.store.DeleteShards(cleanupCtx, fileID); err != nil {
		s.log.ErrorContext(cleanupCtx, "could not delete shard rows",
			slog.String("file_id", fileID.String()),
			slog.String("error", err.Error()))
	}
	if err := s.store.MarkFileFailed(cleanupCtx, fileID); err != nil {
		s.log.ErrorContext(cleanupCtx, "could not mark file failed",
			slog.String("file_id", fileID.String()),
			slog.String("error", err.Error()))
	}
}

func (s *Service) validateUpload(ctx context.Context, req UploadRequest) error {
	name, err := CleanName(req.Name)
	if err != nil {
		return err
	}
	req.Name = name

	switch {
	case req.Size < 0:
		return skerr.Public(skerr.ErrValidation, "Size must not be negative.")
	case req.Size > s.maxUploadBytes:
		return skerr.Public(skerr.ErrTooLarge,
			"That file is %s. The limit is %s.",
			humanBytes(req.Size), humanBytes(s.maxUploadBytes))
	}

	// The destination folder must exist and belong to the caller. Checking
	// it here means a bad folder id fails before a single byte is read.
	if req.FolderID != nil {
		if _, err := s.store.GetFolder(ctx, req.UserID, *req.FolderID); err != nil {
			if errors.Is(err, skerr.ErrNotFound) {
				return skerr.Public(skerr.ErrNotFound, "That folder does not exist.")
			}
			return fmt.Errorf("check destination folder: %w", err)
		}
	}
	return nil
}

func mapStorageError(err error, shardIndex int) error {
	switch {
	case errors.Is(err, storage.ErrQuota):
		return skerr.Public(skerr.ErrQuotaExceeded,
			"A drive filled up at shard %d. Nothing was saved.", shardIndex)
	case errors.Is(err, storage.ErrUnauthorized):
		return skerr.Public(skerr.ErrUnavailable,
			"A drive rejected the upload. Reconnect it and try again.")
	case errors.Is(err, storage.ErrSizeMismatch):
		return skerr.Public(skerr.ErrValidation,
			"Upload failed at shard %d: the drive stored a different number of bytes.", shardIndex)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("upload shard %d: %w", shardIndex, err)
	default:
		return fmt.Errorf("upload shard %d: %w", shardIndex, err)
	}
}

// countingReader counts bytes as they pass. It holds none of them.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// humanBytes formats a size for a user-facing message. Design.md §7: errors
// say what happened in numbers a person can act on.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// CleanName validates a user-supplied file or folder name.
//
// Names never reach a filesystem or a provider path — the provider object name
// is generated — so this is about what is sane to display and store, not about
// traversal. Traversal is prevented structurally by never using this value as
// a path component.
func CleanName(raw string) (string, error) {
	name := strings.TrimSpace(raw)

	switch {
	case name == "":
		return "", skerr.Invalid(map[string]string{"name": "Give it a name."})
	case len(name) > 255:
		return "", skerr.Invalid(map[string]string{"name": "Use 255 characters or fewer."})
	case name == "." || name == "..":
		return "", skerr.Invalid(map[string]string{"name": "That name is not allowed."})
	case strings.ContainsAny(name, "/\\"):
		return "", skerr.Invalid(map[string]string{"name": "Names cannot contain slashes."})
	case strings.ContainsRune(name, 0):
		return "", skerr.Invalid(map[string]string{"name": "That name is not allowed."})
	}

	// Control characters would corrupt a terminal listing and can be used to
	// disguise an extension.
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", skerr.Invalid(map[string]string{"name": "Names cannot contain control characters."})
		}
	}
	return name, nil
}

// releasePlan gives back the capacity a plan reserved, if the planner holds
// any. It runs on a detached context for the same reason cleanup does: the
// usual failure is a cancelled request, and skipping the release exactly when
// an upload died is how a pool shrinks over time.
func (s *Service) releasePlan(ctx context.Context, plan Plan) {
	releaser, ok := s.planner.(ReleasingPlanner)
	if !ok || plan.UploadID == uuid.Nil {
		return
	}

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := releaser.Release(releaseCtx, plan.UploadID); err != nil {
		// Not fatal: the janitor reclaims it at expiry. Still worth
		// knowing, because capacity is briefly understated until then.
		s.log.WarnContext(releaseCtx, "could not release upload reservations",
			slog.String("upload_id", plan.UploadID.String()),
			slog.String("error", err.Error()))
	}
}
