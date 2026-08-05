package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// ShardCheck is the outcome of asking one drive whether one shard is still
// there.
//
// THREE STATES, NOT TWO. Absence and "could not tell" are different facts, and
// collapsing them is how a healthy file gets flagged as corrupted because
// Drive throttled us — the worst possible failure for this feature.
type ShardCheck string

const (
	// ShardPresent — the provider confirmed the object exists.
	ShardPresent ShardCheck = "present"

	// ShardMissing — the provider confirmed the object is GONE. Only ever set
	// from a positive storage.ErrObjectNotFound. Nothing else, ever.
	ShardMissing ShardCheck = "missing"

	// ShardIndeterminate — the check did not produce an answer: exhausted
	// retries, rate limiting, a transport failure, a dead grant, a cancelled
	// run. NEVER flags a file.
	//
	// Verified rather than assumed (gdrive/exhaustion_test.go): an exhausted
	// retry surfaces as storage.ErrRateLimited and never matches
	// ErrObjectNotFound, so the two are cleanly separable.
	ShardIndeterminate ShardCheck = "indeterminate"
)

// FileHealth is one file's reconciled state.
type FileHealth struct {
	FileID uuid.UUID `json:"file_id"`
	Name   string    `json:"name"`
	// State is derived by this run AND persisted to files.status when the
	// file was fully checked -- so a badge survives a page reload rather than
	// living only in one response body. The persistence rule is on Reconcile:
	// a file with any indeterminate shard has nothing written for it.
	State string `json:"state"`

	TotalShards   int     `json:"total_shards"`
	MissingShards []int32 `json:"missing_shard_indexes,omitempty"`
	// UncheckedShards are the indeterminate ones. A file with any of these is
	// NOT flagged, and the run that produced it is not complete.
	UncheckedShards []int32 `json:"unchecked_shard_indexes,omitempty"`
}

// Derived file states.
const (
	// HealthOK — every shard confirmed present.
	HealthOK = "ok"
	// HealthPartiallyMissing — some shards confirmed gone, some present.
	HealthPartiallyMissing = "partially_missing"
	// HealthCorrupted — every shard confirmed gone. The file is unrecoverable
	// from its own shards; only the database row remains.
	HealthCorrupted = "corrupted"
	// HealthUnknown — at least one shard could not be checked and none was
	// confirmed missing. Says nothing about the file.
	HealthUnknown = "unknown"
)

// ReconcileReport is one run's result.
type ReconcileReport struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	FilesChecked  int `json:"files_checked"`
	ShardsChecked int `json:"shards_checked"`

	// Complete is false when ANY shard came back indeterminate.
	//
	// A run that could not check everything must not present itself as clean,
	// and must not be recorded as a completed reconciliation: partial results
	// presented as complete are how a healthy file gets purged.
	Complete bool `json:"complete"`
	// UncheckedShards is how many checks produced no answer.
	UncheckedShards int `json:"unchecked_shards"`
	// Incomplete carries the reasons, deduplicated, for the operator.
	IncompleteReasons []string `json:"incomplete_reasons,omitempty"`

	// Damaged lists only files with at least one CONFIRMED missing shard.
	Damaged []FileHealth `json:"damaged"`
	// Unknown lists files that could not be fully checked. Present so the run
	// can say what it did not manage to look at, rather than staying silent.
	Unknown []FileHealth `json:"unknown,omitempty"`
}

// Reconcile checks every shard of every live file against its provider.
//
// ON-DEMAND, NOT SCHEDULED. This is N metadata calls across every shard of
// every file, on a rate budget shared with bulk delete, empty-trash and quota
// sync. Out-of-band deletion is rare and user-initiated, so a timer would
// spend that budget continuously to find nothing almost every time. It is a
// diagnostic the user runs when something looks wrong.
//
// RESULTS ARE PERSISTED, PER FILE, AND ONLY WHEN DEFINITE. The schema bundle
// widened the files.status CHECK to admit partially_missing and corrupted and
// added files.reconciled_at, so a damaged badge now survives a page reload
// instead of dying with the response that produced it.
//
// The rule, and it is the whole safety property: a file with ANY indeterminate
// shard has NOTHING written for it — not the status, not the timestamp.
// reconciled_at asserts that the evidence was gathered at that moment, so
// stamping it for a file nobody could check is a UI claiming freshness for a
// scan that never happened. Gating is per FILE rather than per RUN, because a
// file fully checked in a partly-throttled run has still been established.
//
// A persistence failure is logged and does not fail the run: the report is
// still correct, and the next run re-derives everything.
func (s *Service) Reconcile(ctx context.Context, userID uuid.UUID) (ReconcileReport, error) {
	report := ReconcileReport{StartedAt: time.Now(), Complete: true}

	files, err := s.store.ListFiles(ctx, userID, ListParams{Limit: maxBulkFiles})
	if err != nil {
		return report, fmt.Errorf("list files: %w", err)
	}

	var (
		mu      sync.Mutex
		reasons = map[string]struct{}{}
		wg      sync.WaitGroup
	)

	healths := make([]FileHealth, len(files))

	for i, file := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, unchecked, why := s.checkFile(ctx, userID, file)

			mu.Lock()
			healths[i] = h
			report.ShardsChecked += h.TotalShards
			report.UncheckedShards += unchecked
			for _, r := range why {
				reasons[r] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	// PERSISTENCE IS PER FILE, NOT PER RUN, and the distinction is deliberate.
	//
	// A file every one of whose shards produced a definite answer has been
	// fully established, even if some OTHER file in the same run was
	// throttled. Gating on report.Complete would throw away good evidence
	// because of an unrelated file; gating on the file's own unchecked count
	// keeps the guarantee exactly where it belongs.
	//
	// The invariant that must hold either way: nothing is written for a file
	// with any indeterminate shard. Not the status, not the timestamp.
	at := time.Now()
	for _, h := range healths {
		if h.TotalShards == 0 {
			continue
		}
		report.FilesChecked++
		switch h.State {
		case HealthPartiallyMissing, HealthCorrupted:
			report.Damaged = append(report.Damaged, h)
		case HealthUnknown:
			report.Unknown = append(report.Unknown, h)
		}

		if len(h.UncheckedShards) > 0 {
			// Nothing definite was established about this file. Leave both
			// columns exactly as they were.
			continue
		}
		if perr := s.store.RecordReconciledHealth(ctx, userID, h.FileID,
			statusForHealth(h.State), at); perr != nil {
			// A write failure does not fail the run: the report is still
			// correct and useful, and the next run re-derives everything. It
			// does mean the badge will be stale, so it is logged rather than
			// swallowed.
			s.log.WarnContext(ctx, "could not persist reconciled health",
				slog.String("file_id", h.FileID.String()),
				slog.String("state", h.State),
				slog.String("error", perr.Error()))
		}
	}

	// ANY indeterminate result makes the run incomplete. This is the guard
	// that stops a throttled run reading as a clean bill of health.
	if report.UncheckedShards > 0 {
		report.Complete = false
		for r := range reasons {
			report.IncompleteReasons = append(report.IncompleteReasons, r)
		}
	}

	// Empty slices, never nil: `"damaged": null` breaks any client that does
	// `.length` on it, which my own check script did the first time this ran
	// live. An empty result is a list with nothing in it, not an absent list.
	if report.Damaged == nil {
		report.Damaged = []FileHealth{}
	}

	report.FinishedAt = time.Now()
	s.log.InfoContext(ctx, "reconcile finished",
		slog.Int("files", report.FilesChecked),
		slog.Int("shards", report.ShardsChecked),
		slog.Int("damaged", len(report.Damaged)),
		slog.Int("unchecked", report.UncheckedShards),
		slog.Bool("complete", report.Complete))

	return report, nil
}

// statusForHealth maps a derived health state onto the persisted status
// column.
//
// HealthUnknown deliberately has no mapping and never reaches here: a file
// with any indeterminate shard is skipped before this is called. It panics
// rather than defaulting, because a silent default is how an unchecked file
// would come to be recorded as healthy.
func statusForHealth(state string) string {
	switch state {
	case HealthOK:
		return StatusReady
	case HealthPartiallyMissing:
		return StatusPartiallyMissing
	case HealthCorrupted:
		return StatusCorrupted
	default:
		panic("files: no persisted status for health state " + state)
	}
}

// checkFile classifies one file's shards, returning its health, how many
// checks produced no answer, and why.
func (s *Service) checkFile(ctx context.Context, userID uuid.UUID, file File) (FileHealth, int, []string) {
	shards, err := s.store.ListShards(ctx, file.ID)
	if err != nil {
		return FileHealth{FileID: file.ID, Name: file.Name, State: HealthUnknown},
			1, []string{"could not read the shard manifest"}
	}

	h := FileHealth{
		FileID: file.ID, Name: file.Name, TotalShards: len(shards),
	}

	var (
		mu       sync.Mutex
		reasons  []string
		presence = make([]ShardCheck, len(shards))
		wg       sync.WaitGroup
	)

	for i, sh := range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, why := s.checkShard(ctx, userID, sh)

			mu.Lock()
			presence[i] = state
			if why != "" {
				reasons = append(reasons, why)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	var missing, unchecked, present int
	for i, state := range presence {
		switch state {
		case ShardMissing:
			missing++
			h.MissingShards = append(h.MissingShards, shards[i].Index)
		case ShardIndeterminate:
			unchecked++
			h.UncheckedShards = append(h.UncheckedShards, shards[i].Index)
		case ShardPresent:
			present++
		}
	}

	switch {
	case missing > 0 && present == 0 && unchecked == 0:
		h.State = HealthCorrupted
	case missing > 0:
		// Deliberately damaged even when some shards are unchecked: a
		// CONFIRMED missing shard is a fact, and an unchecked sibling does not
		// make it less true. The unchecked ones are still listed.
		h.State = HealthPartiallyMissing
	case unchecked > 0:
		h.State = HealthUnknown
	default:
		h.State = HealthOK
	}

	sortInt32(h.MissingShards)
	sortInt32(h.UncheckedShards)
	return h, unchecked, reasons
}

// checkShard asks one provider whether one object is still there.
//
// THE CLASSIFICATION RULE: `missing` requires a POSITIVE
// storage.ErrObjectNotFound. Every other error — rate limiting, an exhausted
// retry, a dead grant, a transport failure, cancellation — is indeterminate.
// There is no default-to-missing branch, by design.
func (s *Service) checkShard(ctx context.Context, userID uuid.UUID, sh Shard) (ShardCheck, string) {
	backend, err := s.backends.For(ctx, userID, sh.AccountID)
	if err != nil {
		return ShardIndeterminate, "a drive could not be reached"
	}

	var result ShardCheck
	var reason string

	perr := s.runPooled(ctx, func(pctx context.Context) error {
		body, _, gerr := backend.Get(pctx, storage.ObjectRef{
			ProviderID: sh.ProviderID, Size: sh.SizeBytes,
		}, &storage.ByteRange{Start: 0, Length: 1})
		if gerr == nil {
			_ = body.Close()
			result = ShardPresent
			return nil
		}
		// The ONLY path to `missing`.
		if errors.Is(gerr, storage.ErrObjectNotFound) {
			result = ShardMissing
			return nil
		}
		return gerr
	})

	if perr != nil {
		// Everything that reaches here is indeterminate. Classified for the
		// operator's benefit only — none of these flags a file.
		switch {
		case errors.Is(perr, storage.ErrRateLimited):
			reason = "a drive was rate limiting and the retries were exhausted"
		case errors.Is(perr, storage.ErrUnauthorized):
			reason = "a drive needs reconnecting"
		case errors.Is(perr, context.Canceled), errors.Is(perr, context.DeadlineExceeded):
			reason = "the run was cancelled before it finished"
		default:
			reason = "a drive could not be reached"
		}
		return ShardIndeterminate, reason
	}
	return result, ""
}

// PurgeDamaged permanently deletes a file whose shards are confirmed gone.
//
// ITS SEMANTIC IS DESTRUCTIVE AND IS ASSERTED BY TEST, NOT INHERITED.
// BulkDelete shipped destroying files because it reused Service.Delete without
// checking which of trash-or-destroy that method carried (issue #41). This
// method does not reuse either: it takes its own path, and
// TestPurgeDamagedDestroysAndDoesNotTrash pins what it does.
//
// Trashing a damaged file would be worse than useless — the shards are already
// gone, so the row would sit in the trash pretending to be recoverable.
//
// It REFUSES a file that is not confirmed damaged. A purge acting on an
// indeterminate result is exactly how a healthy file gets destroyed because a
// drive was rate limiting, so the confirmation is re-run here rather than
// trusted from a report the client sends back.
func (s *Service) PurgeDamaged(ctx context.Context, userID, fileID uuid.UUID) error {
	file, err := s.Get(ctx, userID, fileID)
	if err != nil {
		return err
	}

	health, _, _ := s.checkFile(ctx, userID, file)
	switch health.State {
	case HealthPartiallyMissing, HealthCorrupted:
		// Confirmed damage. Proceed.
	case HealthUnknown:
		return skerr.Public(skerr.ErrConflict,
			"This file could not be verified — a drive did not answer. "+
				"Nothing was deleted. Try again when the drive is reachable.")
	default:
		return skerr.Public(skerr.ErrConflict,
			"This file is intact. Delete it normally if you no longer want it.")
	}

	// Destroys: removes whatever shards remain at their providers and drops
	// the row. Deliberately NOT Trash — see the doc comment.
	if derr := s.Delete(ctx, userID, fileID); derr != nil {
		return derr
	}
	s.log.InfoContext(ctx, "purged a damaged file",
		slog.String("file_id", fileID.String()),
		slog.String("state", health.State),
		slog.Int("missing_shards", len(health.MissingShards)))
	return nil
}
