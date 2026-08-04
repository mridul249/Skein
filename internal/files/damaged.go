package files

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// DamagedFileError reports a file whose shards are no longer all reachable.
//
// It names WHICH shards are gone. Without that the client can only say
// something vague — the UI previously offered "this file cannot be shown here,
// download it instead", which was wrong twice over: the download fails the
// same way, and on the a.click() path the error response is saved as the file.
//
// A damaged file is NOT a server error. The server behaved correctly: it
// refused to hand back a partial file as if it were whole. It maps to 409,
// because retrying can never succeed and the user has a real decision to make
// (purge the record, or reconnect a drive if that is why the shard is
// unreachable).
type DamagedFileError struct {
	FileID uuid.UUID
	// MissingShards are the shard indexes that could not be reached, in order.
	MissingShards []int32
	// TotalShards is how many the manifest expects, so the client can say
	// "2 of 5" rather than only naming the casualties.
	TotalShards int
}

func (e *DamagedFileError) Error() string {
	return fmt.Sprintf("file %s is missing %d of %d shards (%s)",
		e.FileID, len(e.MissingShards), e.TotalShards, e.shardList())
}

func (e *DamagedFileError) shardList() string {
	parts := make([]string, 0, len(e.MissingShards))
	for _, i := range e.MissingShards {
		parts = append(parts, fmt.Sprint(i))
	}
	return strings.Join(parts, ", ")
}

// Unwrap exposes the integrity sentinel so the existing status mapping and
// every errors.Is(err, skerr.ErrIntegrity) check keep working.
func (e *DamagedFileError) Unwrap() error { return skerr.ErrIntegrity }

// PublicMessage is what the user is told. It names the damage and does not
// suggest a download, which cannot work.
func (e *DamagedFileError) PublicMessage() string {
	if len(e.MissingShards) == 1 {
		return fmt.Sprintf(
			"This file is damaged: shard %s of %d is missing from its drive. "+
				"It cannot be downloaded or previewed.",
			e.shardList(), e.TotalShards)
	}
	return fmt.Sprintf(
		"This file is damaged: %d of %d shards are missing from their drives "+
			"(%s). It cannot be downloaded or previewed.",
		len(e.MissingShards), e.TotalShards, e.shardList())
}

// newDamagedFileError builds the public error the API returns.
func newDamagedFileError(fileID uuid.UUID, missing []int32, total int) error {
	d := &DamagedFileError{FileID: fileID, MissingShards: missing, TotalShards: total}
	return &skerr.PublicError{
		Sentinel: d,
		Message:  d.PublicMessage(),
		Fields:   map[string]string{"file_id": fileID.String()},
	}
}

// CheckReadable reports whether every shard of a file is still present at its
// provider.
//
// Called before minting a download capability. `Get` proves ownership and that
// the manifest is internally consistent, but it does not ask the drives
// anything — which is why POST /content-url happily returned 200 for a file
// whose shard had been deleted, and the download then saved an error response
// as the file.
//
// This costs one metadata call per shard, run through the shared pool. It is
// deliberately NOT on the read path: the reader already fails correctly when a
// shard is missing, and paying for a pre-flight on every range request of a
// video would be absurd.
func (s *Service) CheckReadable(ctx context.Context, userID, fileID uuid.UUID) error {
	file, err := s.Get(ctx, userID, fileID)
	if err != nil {
		return err
	}
	if verr := verifyManifest(file); verr != nil {
		return verr
	}

	var (
		mu      sync.Mutex
		missing []int32
		wg      sync.WaitGroup
	)

	for _, sh := range file.Shards {
		wg.Add(1)
		go func() {
			defer wg.Done()

			backend, berr := s.backends.For(ctx, userID, sh.AccountID)
			if berr != nil {
				// The drive is unreachable rather than the shard absent. That
				// is a different condition — a disconnected or dead account —
				// and it is reported by the read path with its own message.
				// Not counted as missing here.
				return
			}

			ok, serr := s.runPooledStat(ctx, backend, sh)
			if serr != nil || ok {
				return
			}
			mu.Lock()
			missing = append(missing, sh.Index)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(missing) == 0 {
		return nil
	}
	sortInt32(missing)
	return newDamagedFileError(file.ID, missing, len(file.Shards))
}

// runPooledStat reports whether one shard's object is still present.
func (s *Service) runPooledStat(ctx context.Context, backend storage.Backend, sh Shard) (bool, error) {
	present := true
	err := s.runPooled(ctx, func(pctx context.Context) error {
		_, _, gerr := backend.Get(pctx, storage.ObjectRef{
			ProviderID: sh.ProviderID, Size: sh.SizeBytes,
		}, &storage.ByteRange{Start: 0, Length: 1})
		if errors.Is(gerr, storage.ErrObjectNotFound) {
			present = false
			return nil
		}
		return gerr
	})
	return present, err
}

func sortInt32(xs []int32) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
