package files_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/storage"
)

// THE HARD REQUIREMENT.
//
// A reconcile run that cannot check its shards must flag NOTHING, and must not
// report itself complete. Flagging a healthy file as corrupted because Drive
// throttled us is the worst possible failure for this feature.
func TestReconcileFlagsNothingWhenRetriesAreExhausted(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "healthy.bin", data)

	// Every drive now reports rate limiting, exactly as an exhausted retry
	// does. The file itself is untouched and perfectly healthy.
	for id := range f.backends {
		f.throttle(id)
	}

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}

	// NOTHING is flagged.
	if len(report.Damaged) != 0 {
		t.Fatalf("%d files flagged as damaged while every check was throttled: %+v. "+
			"A healthy file was flagged because the provider was rate limiting",
			len(report.Damaged), report.Damaged)
	}

	// The run does NOT claim to be clean.
	if report.Complete {
		t.Error("a run with unchecked shards reported itself complete; " +
			"partial results presented as complete are how a healthy file gets purged")
	}
	if report.UncheckedShards == 0 {
		t.Error("no shards were reported unchecked; the throttling was not observed")
	}
	if len(report.IncompleteReasons) == 0 {
		t.Error("the run is incomplete but gives no reason")
	}
	// And it says WHICH file it could not verify, rather than staying silent.
	found := false
	for _, u := range report.Unknown {
		if u.FileID == file.ID {
			found = true
			if u.State != files.HealthUnknown {
				t.Errorf("state = %q, want unknown", u.State)
			}
		}
	}
	if !found {
		t.Error("the unverifiable file is not listed under Unknown")
	}
}

// A CONFIRMED missing shard is flagged. This is the other half: the
// indeterminate rule must not make reconcile useless.
func TestReconcileFlagsAConfirmedMissingShard(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "damaged.bin", data)

	victim := file.Shards[0]
	backend := f.backends[*victim.AccountID]
	if err := backend.Delete(ctx, storage.ObjectRef{ProviderID: victim.ProviderID}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}

	if report.Complete != true {
		t.Errorf("a run with no indeterminate results reported incomplete: %v",
			report.IncompleteReasons)
	}
	if len(report.Damaged) != 1 {
		t.Fatalf("%d damaged files, want 1", len(report.Damaged))
	}

	d := report.Damaged[0]
	if d.FileID != file.ID {
		t.Errorf("flagged %s, want %s", d.FileID, file.ID)
	}
	if len(d.MissingShards) != 1 || d.MissingShards[0] != victim.Index {
		t.Errorf("MissingShards = %v, want [%d]", d.MissingShards, victim.Index)
	}
	// One of several shards gone: partially missing, not corrupted.
	if len(file.Shards) > 1 && d.State != files.HealthPartiallyMissing {
		t.Errorf("state = %q, want partially_missing", d.State)
	}
}

// A healthy file on reachable drives is reported clean, and the run complete.
func TestReconcileReportsAHealthyFileAsClean(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	f.uploadAs(t, f.user1, "fine.bin", data)

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if !report.Complete {
		t.Errorf("a fully checkable run reported incomplete: %v", report.IncompleteReasons)
	}
	if len(report.Damaged) != 0 {
		t.Errorf("%d files flagged on a healthy set", len(report.Damaged))
	}
	if report.FilesChecked == 0 || report.ShardsChecked == 0 {
		t.Errorf("checked %d files / %d shards; the run did nothing",
			report.FilesChecked, report.ShardsChecked)
	}
}

// Reconcile is scoped to the caller: it must not report another user's files.
func TestReconcileIsScopedToTheCaller(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	theirs := f.uploadAs(t, f.user2, "theirs.bin", data)

	// Break user2's file, then reconcile as user1.
	victim := theirs.Shards[0]
	backend := f.backends[*victim.AccountID]
	if err := backend.Delete(ctx, storage.ObjectRef{ProviderID: victim.ProviderID}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	for _, d := range report.Damaged {
		if d.FileID == theirs.ID {
			t.Error("reconcile reported another user's damaged file")
		}
	}
	if report.FilesChecked != 0 {
		t.Errorf("checked %d files for a user who owns none", report.FilesChecked)
	}
}

// A mix of confirmed-missing and unchecked: the confirmed one is still
// flagged, and the run is still incomplete.
func TestReconcileFlagsConfirmedDamageEvenWhenTheRunIsIncomplete(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "mixed.bin", data)
	if len(file.Shards) < 2 {
		t.Skip("needs a file striped across at least two drives")
	}

	// Shard 0 confirmed gone; the drive holding shard 1 throttles.
	victim := file.Shards[0]
	if err := f.backends[*victim.AccountID].Delete(ctx,
		storage.ObjectRef{ProviderID: victim.ProviderID}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	f.throttle(*file.Shards[1].AccountID)

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}

	if report.Complete {
		t.Error("the run had an unchecked shard but reported complete")
	}
	if len(report.Damaged) != 1 {
		t.Fatalf("%d damaged, want 1: a CONFIRMED missing shard is a fact even "+
			"when a sibling could not be checked", len(report.Damaged))
	}
	d := report.Damaged[0]
	if len(d.UncheckedShards) == 0 {
		t.Error("the damaged entry does not say which shards went unchecked")
	}
	if !strings.Contains(strings.Join(report.IncompleteReasons, " "), "rate limiting") {
		t.Errorf("reasons = %v, want the rate limiting named", report.IncompleteReasons)
	}
}

// PURGE'S SEMANTIC IS ASSERTED, NOT INHERITED.
//
// BulkDelete shipped destroying files because it reused Service.Delete without
// checking which of trash-or-destroy that method carried (#41). This test
// exists so PurgeDamaged cannot acquire the wrong semantic the same way: it
// pins that the file is DESTROYED, not trashed.
func TestPurgeDamagedDestroysAndDoesNotTrash(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "gone.bin", data)

	// Confirmed damage: every shard deleted out of band.
	for _, sh := range file.Shards {
		if err := f.backends[*sh.AccountID].Delete(ctx,
			storage.ObjectRef{ProviderID: sh.ProviderID}); err != nil {
			t.Fatalf("Delete() = %v", err)
		}
	}

	if err := f.svc.PurgeDamaged(ctx, f.user1, file.ID); err != nil {
		t.Fatalf("PurgeDamaged() = %v", err)
	}

	// Gone from the listing...
	if live, _ := f.svc.List(ctx, f.user1, files.ListParams{Limit: 100}); len(live) != 0 {
		t.Errorf("%d files still listed after a purge", len(live))
	}
	// ...and NOT in the trash. A damaged file in the trash pretends to be
	// recoverable when its shards are already gone.
	trashed, err := f.svc.ListTrashed(ctx, f.user1, 100)
	if err != nil {
		t.Fatalf("ListTrashed() = %v", err)
	}
	if len(trashed) != 0 {
		t.Errorf("%d files in the trash after a purge; purge must DESTROY, "+
			"not trash — a damaged file in the trash claims to be recoverable",
			len(trashed))
	}
	if _, gerr := f.svc.Get(ctx, f.user1, file.ID); gerr == nil {
		t.Error("the purged file is still readable")
	}
}

// PURGE REFUSES WHAT IT CANNOT CONFIRM.
//
// This is the same hazard as the indeterminate rule, one step further along:
// purging on an unverified result destroys a healthy file because a drive was
// throttling. The confirmation is re-run inside PurgeDamaged rather than
// trusted from a report the client hands back.
func TestPurgeDamagedRefusesAnUnverifiedFile(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "healthy.bin", data)

	// Every drive throttles: the file cannot be verified either way.
	for id := range f.backends {
		f.throttle(id)
	}

	err := f.svc.PurgeDamaged(ctx, f.user1, file.ID)
	if err == nil {
		t.Fatal("PurgeDamaged() destroyed a file it could not verify; " +
			"a throttled drive is not evidence of damage")
	}
	if !strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("error = %v, want it to say the file could not be verified", err)
	}

	// The file survives, intact.
	if _, gerr := f.svc.Get(ctx, f.user1, file.ID); gerr != nil {
		t.Errorf("the unverified file was destroyed anyway: %v", gerr)
	}
}

// And a healthy file is refused too: purge is not a general delete.
func TestPurgeDamagedRefusesAnIntactFile(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "fine.bin", data)

	err := f.svc.PurgeDamaged(ctx, f.user1, file.ID)
	if err == nil {
		t.Fatal("PurgeDamaged() destroyed an intact file")
	}
	if !strings.Contains(err.Error(), "intact") {
		t.Errorf("error = %v, want it to say the file is intact", err)
	}
	if _, gerr := f.svc.Get(ctx, f.user1, file.ID); gerr != nil {
		t.Errorf("the intact file was destroyed: %v", gerr)
	}
}

// A clean run returns an EMPTY LIST, not null.
//
// `"damaged": null` breaks any client doing `.length` on it — observed live,
// where the check script itself crashed on the first clean run.
func TestReconcileReturnsEmptyListNotNull(t *testing.T) {
	f := newSharedDrive(t)

	report, err := f.svc.Reconcile(context.Background(), f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if report.Damaged == nil {
		t.Error("Damaged is nil; it must be an empty slice so it serialises " +
			"as [] rather than null")
	}
}
