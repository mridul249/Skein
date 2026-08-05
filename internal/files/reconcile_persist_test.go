package files_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/storage"
)

// Reconcile's three-state design has to survive contact with persistence.
//
// Before the schema bundle, damage was derived per run and returned — so a
// badge was correct immediately after a run and gone on the next page load.
// Persisting it is the point of widening files.status, but persistence adds a
// failure mode deriving never had: a row asserting damage, or asserting
// freshness, on evidence that was never gathered.
//
// The rule these tests pin: a run with ANY indeterminate result stamps
// NOTHING. Not the status, not reconciled_at.

// A complete run that confirms damage records it, so the badge survives a
// reload rather than living only in one response body.
func TestReconcilePersistsConfirmedDamage(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "damaged.bin", data)

	// One shard deleted out of band, the rest confirmed present: a complete
	// run with a positive ErrObjectNotFound, which is the only thing that may
	// ever flag a file.
	stored, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if len(stored.Shards) < 2 {
		t.Fatalf("want a striped file, got %d shard(s)", len(stored.Shards))
	}
	victim := stored.Shards[0]
	if derr := f.backends[*victim.AccountID].Delete(ctx,
		storage.ObjectRef{ProviderID: victim.ProviderID}); derr != nil {
		t.Fatalf("delete shard object: %v", derr)
	}

	before := time.Now()
	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if !report.Complete {
		t.Fatalf("run reported incomplete: %v", report.IncompleteReasons)
	}
	if len(report.Damaged) != 1 {
		t.Fatalf("%d damaged in the report, want 1", len(report.Damaged))
	}

	// The row, not the report. This is the whole difference from deriving.
	got, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() after reconcile = %v", err)
	}
	if got.Status != files.StatusPartiallyMissing {
		t.Errorf("persisted status = %q, want %q; the badge dies on reload",
			got.Status, files.StatusPartiallyMissing)
	}
	if got.ReconciledAt == nil {
		t.Fatal("reconciled_at was not stamped by a complete run")
	}
	if got.ReconciledAt.Before(before.Add(-time.Second)) {
		t.Errorf("reconciled_at = %v, want a time from this run (after %v)",
			got.ReconciledAt, before)
	}
}

// THE FAILURE MODE PERSISTENCE INTRODUCES, asserted directly.
//
// A throttled run has checked nothing. It must not write a status, and it must
// not stamp reconciled_at — a timestamp is an assertion that the evidence was
// gathered at that moment, and stamping one here makes the UI claim freshness
// for a scan that never happened.
func TestReconcileDoesNotPersistAnythingFromAnIncompleteRun(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "healthy.bin", data)

	// Every drive throttles. The file is perfectly healthy; the run simply
	// cannot see it.
	for id := range f.backends {
		f.throttle(id)
	}

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if report.Complete {
		t.Fatal("a fully throttled run reported itself complete")
	}

	got, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got.Status != files.StatusReady {
		t.Errorf("status = %q, want %q: an incomplete run rewrote the status",
			got.Status, files.StatusReady)
	}
	if got.ReconciledAt != nil {
		t.Errorf("reconciled_at = %v, want nil: an incomplete run stamped "+
			"freshness on evidence it never gathered", got.ReconciledAt)
	}
}

// A healthy file confirmed healthy by a complete run gets its reconciled_at
// stamped and its status left alone. This is the other half of the persistence
// contract: "reconciled and found fine" is a real result and must be
// recordable, distinct from "never reconciled" (NULL).
func TestReconcileStampsAHealthyFileWithoutChangingItsStatus(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "healthy.bin", data)

	if got, gerr := f.svc.Get(ctx, f.user1, file.ID); gerr != nil {
		t.Fatalf("Get() = %v", gerr)
	} else if got.ReconciledAt != nil {
		t.Fatal("a freshly uploaded file already carries reconciled_at")
	}

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if !report.Complete {
		t.Fatalf("run reported incomplete: %v", report.IncompleteReasons)
	}

	got, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got.Status != files.StatusReady {
		t.Errorf("status = %q, want %q: a healthy file was reclassified",
			got.Status, files.StatusReady)
	}
	if got.ReconciledAt == nil {
		t.Error("a complete run did not stamp reconciled_at on a healthy file, " +
			"so the UI cannot tell 'checked and fine' from 'never checked'")
	}
}

// A damaged file must stay VISIBLE. Both ListFiles implementations filtered on
// status = 'ready', so persisting a damage status would have made the file
// vanish from the listing entirely — the exact opposite of the badge this work
// exists to render, and a silent disappearance rather than a visible warning.
func TestADamagedFileStaysInTheListing(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "still-listed.bin", data)

	stored, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	victim := stored.Shards[0]
	if derr := f.backends[*victim.AccountID].Delete(ctx,
		storage.ObjectRef{ProviderID: victim.ProviderID}); derr != nil {
		t.Fatalf("delete shard object: %v", derr)
	}
	if _, rerr := f.svc.Reconcile(ctx, f.user1); rerr != nil {
		t.Fatalf("Reconcile() = %v", rerr)
	}

	listed, err := f.svc.List(ctx, f.user1, files.ListParams{Limit: 50})
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	var found bool
	for _, l := range listed {
		if l.ID == file.ID {
			found = true
			if l.Status == files.StatusReady {
				t.Error("the listing reports a damaged file as ready")
			}
		}
	}
	if !found {
		t.Error("a damaged file DISAPPEARED from the listing; " +
			"it must be visible and marked, not hidden")
	}
}
