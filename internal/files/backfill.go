package files

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/storage"
)

// BackfillState is one file's outcome from a backfill run.
//
// THREE STATES, as reconcile and reconstruct have, and for the same reason: a
// file whose drives could not be reached has NOT failed, and recording it as
// failed would tell the user their library is less recoverable than it is.
type BackfillState string

const (
	// BackfillWritten — a manifest was written for this file by this run.
	BackfillWritten BackfillState = "written"
	// BackfillAlreadyCovered — every participating account already held one.
	// On a re-run this is everything, which is what idempotence looks like.
	BackfillAlreadyCovered BackfillState = "already_covered"
	// BackfillSkippedDamaged — reconcile has confirmed shards missing.
	//
	// A manifest for a file with a dead shard promises a recovery that cannot
	// happen: reconstruction would rebuild rows pointing at objects that are
	// gone, and the user would find out at download time. Reported separately
	// rather than folded into failures, because nothing is wrong with the
	// backfill — the file is damaged and that is a different problem.
	BackfillSkippedDamaged BackfillState = "skipped_damaged"
	// BackfillIndeterminate — the accounts holding this file could not be
	// reached, so whether it is covered is unknown. NEVER counted as covered
	// and never counted as failed.
	BackfillIndeterminate BackfillState = "indeterminate"
	// BackfillFailed — the accounts were reachable and the write did not
	// succeed. A real failure, distinct from not being able to look.
	BackfillFailed BackfillState = "failed"
	// BackfillUncovered — this file has no manifest and none was attempted.
	// Only produced by a coverage read; a real run either writes or fails.
	BackfillUncovered BackfillState = "uncovered"
)

// BackfillResult is one file's line in the report.
type BackfillResult struct {
	FileID uuid.UUID     `json:"file_id"`
	State  BackfillState `json:"state"`
	// Copies is how many accounts hold a manifest for this file after the run.
	Copies int `json:"copies"`
	// Accounts is how many accounts hold one of its shards, so a client can
	// see partial coverage rather than only "written".
	Accounts int    `json:"accounts"`
	Reason   string `json:"reason,omitempty"`
}

// ManifestCoverage answers whether the drives are actually a recovery source.
//
// The headline number the UI needs: N of M files have a manifest. Without it a
// user cannot tell whether reconstruction would recover their library or find
// nothing, and "reconstruction exists" is not the same claim as "your files
// are recoverable".
type ManifestCoverage struct {
	// Files is every live, committed file the user owns.
	Files int `json:"files"`
	// Covered have a manifest on EVERY account holding one of their shards —
	// the property reconstruction actually depends on, since any single
	// surviving drive must bootstrap discovery of the rest.
	Covered int `json:"covered"`
	// PartiallyCovered have a manifest on some but not all of their accounts.
	// Recoverable today, but not if the wrong drive is the one that survives.
	PartiallyCovered int `json:"partially_covered"`
	// Uncovered have none at all.
	Uncovered int `json:"uncovered"`
	// Damaged are excluded from backfill by design; counted so the totals add
	// up and the UI can say why they are not covered.
	Damaged int `json:"damaged"`
	// Indeterminate could not be established because an account was
	// unreachable. Complete is false whenever this is non-zero.
	Indeterminate int `json:"indeterminate"`
}

// BackfillReport is one run's result.
type BackfillReport struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// DryRun is true for a coverage read, which writes nothing.
	DryRun bool `json:"dry_run"`

	// Complete is false when any account could not be listed. A run that could
	// not see every drive must not present its coverage as the whole truth.
	Complete          bool     `json:"complete"`
	IncompleteReasons []string `json:"incomplete_reasons,omitempty"`

	Accounts []AccountScan    `json:"accounts"`
	Coverage ManifestCoverage `json:"coverage"`
	Results  []BackfillResult `json:"results"`
}

// BackfillManifests writes a sidecar manifest for every file that lacks one.
//
// WHY THIS EXISTS: manifests are written at upload commit, so every file
// uploaded before that shipped has none. Reconstruction against such a library
// correctly finds nothing and recovers nothing — the feature is present and
// useless. Everything a manifest needs is already in `files` and `file_shards`,
// so this derives them from the database rather than from the drives.
//
// IDEMPOTENT. A file already covered on every participating account is left
// alone and reported as `already_covered`; re-running writes nothing.
//
// DAMAGED FILES ARE SKIPPED, not failed. See BackfillSkippedDamaged.
//
// A file whose accounts could not be listed is INDETERMINATE. It is not
// covered, not failed, and the run is not complete.
func (s *Service) BackfillManifests(ctx context.Context, userID uuid.UUID, accountIDs []uuid.UUID) (BackfillReport, error) {
	return s.backfillManifests(ctx, userID, accountIDs, false)
}

// backfillManifests is the shared implementation. dryRun reports coverage
// without writing, which is what ManifestCoverageForUser needs — one code path
// so the numbers a user sees before acting are produced by the same logic that
// acts.
func (s *Service) backfillManifests(ctx context.Context, userID uuid.UUID, accountIDs []uuid.UUID, dryRun bool) (BackfillReport, error) {
	report := BackfillReport{StartedAt: time.Now(), Complete: true, DryRun: dryRun}

	// One listing per account, up front. Coverage is a question about what is
	// AT THE PROVIDER, and asking it per file would be N listings of the same
	// folder. The manifest bodies are not downloaded: presence by name is the
	// whole question, and each name carries its file id.
	present := map[uuid.UUID]map[uuid.UUID]bool{} // account -> file ids covered
	for _, acct := range accountIDs {
		scan, covered := s.listManifestNames(ctx, userID, acct)
		report.Accounts = append(report.Accounts, scan)
		if !scan.Scanned {
			report.Complete = false
			report.IncompleteReasons = appendUnique(report.IncompleteReasons, scan.Reason)
			continue
		}
		present[acct] = covered
	}

	// ListAllFiles, NOT ListFiles: a nil ListParams.FolderID means the ROOT
	// folder, not "everywhere". This operation swept 2 of the owner's 20 live
	// files for that reason while reporting itself complete — known issue #50.
	files, err := s.store.ListAllFiles(ctx, userID)
	if err != nil {
		return report, err
	}

	for _, f := range files {
		result := s.backfillOne(ctx, userID, f, present, dryRun)
		report.Results = append(report.Results, result)

		report.Coverage.Files++
		switch result.State {
		case BackfillWritten, BackfillAlreadyCovered:
			if result.Copies >= result.Accounts && result.Accounts > 0 {
				report.Coverage.Covered++
			} else {
				report.Coverage.PartiallyCovered++
			}
		case BackfillSkippedDamaged:
			report.Coverage.Damaged++
		case BackfillIndeterminate:
			report.Coverage.Indeterminate++
			report.Complete = false
		case BackfillFailed, BackfillUncovered:
			report.Coverage.Uncovered++
		}
	}

	if report.Results == nil {
		report.Results = []BackfillResult{}
	}
	if report.Accounts == nil {
		report.Accounts = []AccountScan{}
	}
	sort.Strings(report.IncompleteReasons)

	report.FinishedAt = time.Now()
	s.log.InfoContext(ctx, "manifest backfill finished",
		slog.Int("files", report.Coverage.Files),
		slog.Int("covered", report.Coverage.Covered),
		slog.Int("partial", report.Coverage.PartiallyCovered),
		slog.Int("damaged", report.Coverage.Damaged),
		slog.Int("indeterminate", report.Coverage.Indeterminate),
		slog.Bool("complete", report.Complete))
	return report, nil
}

// ManifestCoverageForUser reports coverage WITHOUT writing anything.
//
// A read-only counterpart to backfill, so the UI can state whether the drives
// are actually a recovery source before offering to change anything. Asking
// "am I recoverable?" must not be an operation that mutates storage — a user
// checking their safety net should not be surprised by writes to four Drive
// accounts.
func (s *Service) ManifestCoverageForUser(ctx context.Context, userID uuid.UUID) (BackfillReport, error) {
	if s.accounts == nil {
		return BackfillReport{}, fmt.Errorf("files: no account lister wired")
	}
	ids, err := s.accounts.AccountIDsForUser(ctx, userID)
	if err != nil {
		return BackfillReport{}, fmt.Errorf("list connected drives: %w", err)
	}
	return s.backfillManifests(ctx, userID, ids, true)
}

// BackfillManifestsForUser runs backfill across every drive the user has
// connected. The HTTP entry point; BackfillManifests takes explicit account
// ids so tests can name them.
func (s *Service) BackfillManifestsForUser(ctx context.Context, userID uuid.UUID) (BackfillReport, error) {
	if s.accounts == nil {
		return BackfillReport{}, fmt.Errorf("files: no account lister wired")
	}
	ids, err := s.accounts.AccountIDsForUser(ctx, userID)
	if err != nil {
		return BackfillReport{}, fmt.Errorf("list connected drives: %w", err)
	}
	return s.BackfillManifests(ctx, userID, ids)
}

// backfillOne covers a single file.
func (s *Service) backfillOne(ctx context.Context, userID uuid.UUID, f File, present map[uuid.UUID]map[uuid.UUID]bool, dryRun bool) BackfillResult {
	result := BackfillResult{FileID: f.ID}

	// A manifest promising recovery of a file with a confirmed-missing shard
	// is worse than none: reconstruction would rebuild rows pointing at
	// objects that are gone. Skipped by design, reported separately.
	if f.Status == StatusPartiallyMissing || f.Status == StatusCorrupted {
		result.State = BackfillSkippedDamaged
		result.Reason = "reconcile confirmed shards missing for this file"
		return result
	}

	shards, err := s.store.ListShards(ctx, f.ID)
	if err != nil {
		result.State = BackfillFailed
		result.Reason = "the shard manifest could not be read"
		return result
	}

	accounts := distinctAccounts(shards)
	result.Accounts = len(accounts)
	if len(accounts) == 0 {
		// Every shard orphaned: there is no drive to write to, and nothing to
		// recover from either.
		result.State = BackfillIndeterminate
		result.Reason = "no connected drive holds a shard of this file"
		return result
	}

	// Which participating accounts are already covered, and which could not be
	// checked at all.
	var missing []uuid.UUID
	for _, acct := range accounts {
		covered, listed := present[acct]
		if !listed {
			// An account holding a shard of this file could not be listed, so
			// whether this file is covered is genuinely unknown. Writing
			// anyway risks a duplicate; claiming coverage risks a lie.
			result.State = BackfillIndeterminate
			result.Reason = "a drive holding a shard of this file could not be listed"
			return result
		}
		if covered[f.ID] {
			result.Copies++
			continue
		}
		missing = append(missing, acct)
	}

	if len(missing) == 0 {
		result.State = BackfillAlreadyCovered
		return result
	}
	if dryRun {
		// Coverage only: say what IS, write nothing. Uncovered rather than
		// failed — nothing was attempted, so nothing failed.
		result.State = BackfillUncovered
		return result
	}

	// Build the manifest from the database, exactly as the upload path does.
	// ManifestFor is the single definition of the format, so a backfilled
	// manifest and an upload-time one are identical by construction rather
	// than by two implementations agreeing — pinned by
	// TestABackfilledManifestIsIdenticalToAnUploadTimeOne.
	full := f
	full.Shards = shards
	sealed, serr := SealManifest(s.keyring, ManifestFor(full, s.folderPathFor(ctx, userID, f.FolderID), s.emailFor(ctx, userID)))
	if serr != nil {
		result.State = BackfillFailed
		result.Reason = "the manifest could not be built"
		return result
	}

	name := ManifestName(f.ID)
	for _, acct := range missing {
		backend, berr := s.backends.For(ctx, userID, &acct)
		if berr != nil {
			result.Reason = "a drive could not be opened"
			continue
		}
		werr := s.runPooled(ctx, func(ctx context.Context) error {
			_, e := backend.Put(ctx, bytes.NewReader(sealed), storage.ObjectSpec{
				Name:        name,
				Size:        int64(len(sealed)),
				ContentType: "application/octet-stream",
			})
			return e
		})
		if werr != nil {
			s.log.WarnContext(ctx, "could not backfill a sidecar manifest",
				slog.String("file_id", f.ID.String()),
				slog.String("account_id", acct.String()),
				slog.String("error", werr.Error()))
			result.Reason = "a manifest write was refused"
			continue
		}
		result.Copies++
	}

	// Zero copies after attempting every missing account is a real failure:
	// the accounts were reachable enough to list, and the write still did not
	// land. Distinct from indeterminate, which is "could not look".
	result.State = BackfillWritten
	if result.Copies == 0 {
		result.State = BackfillFailed
	}
	return result
}

// listManifestNames lists one account and returns the file ids it already
// holds a manifest for.
//
// Names only — the bodies are not downloaded. Coverage asks whether an object
// exists, and each manifest's name carries its file id, so fetching the
// ciphertext would be N downloads to learn something the listing already said.
func (s *Service) listManifestNames(ctx context.Context, userID, accountID uuid.UUID) (AccountScan, map[uuid.UUID]bool) {
	scan := AccountScan{AccountID: accountID}

	backend, err := s.backends.For(ctx, userID, &accountID)
	if err != nil {
		scan.Reason = "the drive could not be opened"
		return scan, nil
	}
	lister, ok := backend.(storage.Lister)
	if !ok {
		scan.Reason = "this drive does not support listing"
		return scan, nil
	}

	var objects []storage.ListedObject
	if perr := s.runPooled(ctx, func(ctx context.Context) error {
		var lerr error
		objects, lerr = lister.List(ctx)
		return lerr
	}); perr != nil {
		scan.Reason = describeScanFailure(perr)
		return scan, nil
	}

	scan.Scanned = true
	covered := map[uuid.UUID]bool{}
	for _, obj := range objects {
		if !IsManifestName(obj.Name) {
			continue
		}
		if id, perr := manifestFileID(obj.Name); perr == nil {
			covered[id] = true
			scan.ManifestsFound++
		}
	}
	return scan, covered
}

// distinctAccounts returns the accounts holding at least one of these shards.
func distinctAccounts(shards []Shard) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	out := make([]uuid.UUID, 0, len(shards))
	for _, sh := range shards {
		if sh.AccountID == nil || seen[*sh.AccountID] {
			continue
		}
		seen[*sh.AccountID] = true
		out = append(out, *sh.AccountID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func appendUnique(xs []string, x string) []string {
	if x == "" {
		return xs
	}
	for _, existing := range xs {
		if existing == x {
			return xs
		}
	}
	return append(xs, x)
}
