package files

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/storage"
)

// AccountScan is what one account's manifest scan produced.
//
// THREE STATES, exactly as reconcile has, and for the same reason: "I looked
// and found nothing" and "I could not look" are different facts. An account
// that could not be enumerated is INDETERMINATE, never empty. A rate-limited
// scan reporting itself complete is how someone concludes their files are gone
// when they are not.
type AccountScan struct {
	AccountID uuid.UUID `json:"account_id"`
	// Scanned is true only when the account was successfully enumerated.
	Scanned bool `json:"scanned"`
	// ManifestsFound is how many sidecar manifests the listing contained.
	ManifestsFound int `json:"manifests_found"`
	// Reason explains a failed scan, for the operator.
	Reason string `json:"reason,omitempty"`
}

// ReconstructReport is one run's result.
type ReconstructReport struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Complete is false when ANY account could not be scanned, or any manifest
	// found could not be fetched or decrypted.
	//
	// A run that could not see everything must not present itself as a
	// complete picture of what exists. Reconstruction is additive so an
	// incomplete run is not dangerous the way an incomplete reconcile is —
	// but a user told "recovery complete" after a partial scan will stop
	// looking for the rest, which is its own kind of data loss.
	Complete          bool     `json:"complete"`
	IncompleteReasons []string `json:"incomplete_reasons,omitempty"`

	Accounts []AccountScan `json:"accounts"`

	// ManifestsFound is the total across every scanned account, counting each
	// file once however many copies were found.
	ManifestsFound int `json:"manifests_found"`
	// ManifestsUnreadable is how many were found but could not be fetched or
	// decrypted. Each one is a file that exists and was NOT recovered.
	ManifestsUnreadable int `json:"manifests_unreadable"`

	FilesRecovered   int `json:"files_recovered"`
	ShardsRecovered  int `json:"shards_recovered"`
	FoldersRecovered int `json:"folders_recovered"`
	// FilesAlreadyPresent is how many manifests described a file the database
	// already had. On a re-run this is everything, which is what idempotence
	// looks like from the outside.
	FilesAlreadyPresent int `json:"files_already_present"`
}

// Reconstruct rebuilds database rows from the sidecar manifests on a user's
// connected drives.
//
// THE POINT: without this, losing the database loses the library — the shards
// remain in Drive as opaque objects with no record of which file they belong
// to, what order they go in, or how to lay the bytes back out. Block 2 put
// that record on the drives; this reads it back.
//
// ADDITIVE ONLY, NEVER DESTRUCTIVE. It inserts what is missing and touches
// nothing that exists. It does NOT delete rows the database has and the
// manifests do not — that is reconcile's job, and conflating the two means a
// partial Drive scan wipes good data. Where the database and a manifest
// disagree about a file that already exists, the DATABASE WINS: it holds state
// a manifest cannot know, such as a rename, a trash, or a reconcile verdict.
//
// NO LAST-WRITE-WINS, AND NO TIMESTAMP VECTORS. One Skein instance per user
// means a single writer, so there is no concurrent-update conflict to resolve
// and nothing to merge; `updated_at` comparison would suffice even if there
// were. Do not reintroduce a merge algorithm here — the original spec called
// for one, and it solves a problem this system does not have.
//
// OWNERSHIP IS ENFORCED, NOT ASSUMED. Manifests carry user_id, and any whose
// user_id is not the caller's is skipped. Two Skein users can connect the same
// Google account and therefore see each other's manifest objects; multi-user
// isolation was established in Session 4 and must not regress here.
func (s *Service) Reconstruct(ctx context.Context, userID uuid.UUID, accountIDs []uuid.UUID) (ReconstructReport, error) {
	report := ReconstructReport{
		StartedAt: time.Now(),
		Complete:  true,
		Accounts:  make([]AccountScan, 0, len(accountIDs)),
	}
	if len(accountIDs) == 0 {
		report.FinishedAt = time.Now()
		return report, nil
	}

	var (
		mu      sync.Mutex
		reasons = map[string]struct{}{}
		wg      sync.WaitGroup
		scans   = make([]AccountScan, len(accountIDs))
		// found maps file id to one sealed manifest body. Each file has a copy
		// on every account holding one of its shards, so the same file is seen
		// repeatedly; one readable copy is all reconstruction needs.
		found = map[uuid.UUID][]byte{}
	)

	for i, acct := range accountIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scan, manifests, why := s.scanAccount(ctx, userID, acct)

			mu.Lock()
			defer mu.Unlock()
			scans[i] = scan
			for id, body := range manifests {
				if _, have := found[id]; !have {
					found[id] = body
				}
			}
			for _, r := range why {
				reasons[r] = struct{}{}
			}
		}()
	}
	wg.Wait()

	for _, scan := range scans {
		report.Accounts = append(report.Accounts, scan)
		if !scan.Scanned {
			report.Complete = false
		}
	}
	report.ManifestsFound = len(found)

	// Apply in a deterministic order so a run's log is readable and two runs
	// over the same data do the same things in the same sequence.
	ids := make([]uuid.UUID, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	for _, id := range ids {
		m, err := OpenManifest(s.keyring, id, found[id])
		if err != nil {
			// Found but unreadable: a file that exists and was not recovered.
			// Never silent — this is the difference between "nothing to
			// recover" and "something I could not read".
			report.ManifestsUnreadable++
			report.Complete = false
			reasons["a manifest could not be decrypted"] = struct{}{}
			s.log.WarnContext(ctx, "could not open a sidecar manifest",
				slog.String("file_id", id.String()),
				slog.String("error", err.Error()))
			continue
		}

		// OWNERSHIP. A manifest belonging to another user is skipped outright
		// and is not an error: on a shared Google account it is the expected
		// case, not a fault.
		if m.UserID != userID {
			continue
		}

		if err := s.applyManifest(ctx, userID, m, &report); err != nil {
			report.Complete = false
			reasons["a file could not be written to the database"] = struct{}{}
			s.log.WarnContext(ctx, "could not apply a manifest",
				slog.String("file_id", m.FileID.String()),
				slog.String("error", err.Error()))
		}
	}

	for r := range reasons {
		report.IncompleteReasons = append(report.IncompleteReasons, r)
	}
	sort.Strings(report.IncompleteReasons)

	report.FinishedAt = time.Now()
	s.log.InfoContext(ctx, "reconstruct finished",
		slog.Int("accounts", len(report.Accounts)),
		slog.Int("manifests", report.ManifestsFound),
		slog.Int("unreadable", report.ManifestsUnreadable),
		slog.Int("files_recovered", report.FilesRecovered),
		slog.Int("shards_recovered", report.ShardsRecovered),
		slog.Int("already_present", report.FilesAlreadyPresent),
		slog.Bool("complete", report.Complete))
	return report, nil
}

// ReconstructAll runs reconstruction across every drive the user has
// connected. It is the entry point the HTTP handler uses; Reconstruct takes
// explicit account ids so tests can name them.
func (s *Service) ReconstructAll(ctx context.Context, userID uuid.UUID) (ReconstructReport, error) {
	if s.accounts == nil {
		return ReconstructReport{}, fmt.Errorf("files: no account lister wired")
	}
	ids, err := s.accounts.AccountIDsForUser(ctx, userID)
	if err != nil {
		return ReconstructReport{}, fmt.Errorf("list connected drives: %w", err)
	}
	return s.Reconstruct(ctx, userID, ids)
}

// scanAccount lists one account and fetches every manifest it holds.
func (s *Service) scanAccount(ctx context.Context, userID, accountID uuid.UUID) (AccountScan, map[uuid.UUID][]byte, []string) {
	scan := AccountScan{AccountID: accountID}

	backend, err := s.backends.For(ctx, userID, &accountID)
	if err != nil {
		scan.Reason = "the drive could not be opened"
		return scan, nil, []string{scan.Reason}
	}

	// Lister is optional. A backend that cannot enumerate is INDETERMINATE,
	// never empty: concluding "no files here" from a backend that cannot
	// answer the question is exactly the collapse this design forbids.
	lister, ok := backend.(storage.Lister)
	if !ok {
		scan.Reason = "this drive does not support listing"
		return scan, nil, []string{scan.Reason}
	}

	var objects []storage.ListedObject
	if perr := s.runPooled(ctx, func(ctx context.Context) error {
		var lerr error
		objects, lerr = lister.List(ctx)
		return lerr
	}); perr != nil {
		scan.Reason = describeScanFailure(perr)
		return scan, nil, []string{scan.Reason}
	}

	scan.Scanned = true
	out := map[uuid.UUID][]byte{}
	var why []string

	for _, obj := range objects {
		if !IsManifestName(obj.Name) {
			continue
		}
		fileID, perr := manifestFileID(obj.Name)
		if perr != nil {
			continue // not one of ours, or a name we do not understand
		}
		scan.ManifestsFound++

		body, ferr := s.fetchManifest(ctx, backend, obj)
		if ferr != nil {
			why = append(why, "a manifest could not be downloaded")
			s.log.WarnContext(ctx, "could not fetch a sidecar manifest",
				slog.String("file_id", fileID.String()),
				slog.String("account_id", accountID.String()),
				slog.String("error", ferr.Error()))
			continue
		}
		out[fileID] = body
	}
	return scan, out, why
}

// fetchManifest downloads one manifest through the shared pool.
func (s *Service) fetchManifest(ctx context.Context, backend storage.Backend, obj storage.ListedObject) ([]byte, error) {
	var body []byte
	err := s.runPooled(ctx, func(ctx context.Context) error {
		rc, _, gerr := backend.Get(ctx, storage.ObjectRef{ProviderID: obj.ProviderID}, nil)
		if gerr != nil {
			return gerr
		}
		defer func() { _ = rc.Close() }()

		// Manifests are small by construction — a few hundred bytes per shard
		// — so a bounded ReadAll is appropriate here and nowhere else in this
		// package. The limit stops a corrupt or hostile object from being read
		// into memory without bound.
		b, rerr := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
		if rerr != nil {
			return rerr
		}
		body = b
		return nil
	})
	return body, err
}

// maxManifestBytes bounds a manifest read. A file with 10,000 shards would
// produce roughly 2 MB of JSON, so 8 MiB is far past any real manifest while
// still being a bound.
const maxManifestBytes = 8 << 20

// applyManifest writes one recovered file and its shards.
func (s *Service) applyManifest(ctx context.Context, userID uuid.UUID, m Manifest, report *ReconstructReport) error {
	folderID, err := s.ensureFolderPath(ctx, userID, m.FolderPath, report)
	if err != nil {
		return err
	}

	inserted, err := s.store.InsertReconstructedFile(ctx, ReconstructedFile{
		ID:           m.FileID,
		UserID:       userID,
		FolderID:     folderID,
		Name:         m.FileName,
		SizeBytes:    m.PlainSizeBytes,
		DeclaredMime: m.MimeType,
		IsStriped:    len(m.Shards) > 1,
		IsEncrypted:  s.encrypt,
		CreatedAt:    m.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("insert file: %w", err)
	}
	if inserted {
		report.FilesRecovered++
	} else {
		report.FilesAlreadyPresent++
	}

	// Shards are inserted whether or not the file row was: a file present with
	// a missing shard row is exactly the state a half-finished earlier run
	// leaves, and the point of idempotence is that a re-run completes it.
	for _, ms := range m.Shards {
		digest, derr := hex.DecodeString(ms.SHA256)
		if derr != nil {
			digest = nil // a manifest without a usable digest is still worth recovering
		}
		added, serr := s.store.InsertReconstructedShard(ctx, NewShard{
			ID:          uuid.New(),
			FileID:      m.FileID,
			Index:       ms.Index,
			AccountID:   ms.AccountID,
			ProviderID:  ms.ProviderObjectID,
			SizeBytes:   ms.CiphertextSize,
			PlainSize:   ms.PlainSize,
			PlainOffset: ms.PlainOffset,
			SHA256:      digest,
		})
		if serr != nil {
			return fmt.Errorf("insert shard %d: %w", ms.Index, serr)
		}
		if added {
			report.ShardsRecovered++
		}
	}
	return nil
}

// ensureFolderPath recreates a file's folder chain, outermost first.
func (s *Service) ensureFolderPath(ctx context.Context, userID uuid.UUID, path []string, report *ReconstructReport) (*uuid.UUID, error) {
	var parent *uuid.UUID
	for _, name := range path {
		before, _ := s.store.ListFolders(ctx, userID)
		id, err := s.store.EnsureFolder(ctx, userID, parent, name)
		if err != nil {
			return nil, fmt.Errorf("ensure folder %q: %w", name, err)
		}
		after, _ := s.store.ListFolders(ctx, userID)
		if len(after) > len(before) {
			report.FoldersRecovered++
		}
		next := id
		parent = &next
	}
	return parent, nil
}

// manifestFileID recovers the file id from a manifest object name.
func manifestFileID(name string) (uuid.UUID, error) {
	if !IsManifestName(name) {
		return uuid.Nil, errors.New("not a manifest name")
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, ManifestPrefix), ".enc")
	return uuid.Parse(raw)
}

// describeScanFailure turns a listing error into something an operator can act
// on, without collapsing a rate limit into "no files here".
func describeScanFailure(err error) string {
	switch {
	case errors.Is(err, storage.ErrRateLimited):
		return "the drive was rate limiting and the scan was not completed"
	case errors.Is(err, storage.ErrUnauthorized):
		return "the drive needs to be reconnected"
	case errors.Is(err, context.Canceled):
		return "the scan was cancelled"
	default:
		return "the drive could not be listed"
	}
}
