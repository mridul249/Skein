package files_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/storage"
)

func sameAccount(a, b *uuid.UUID) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func derefUUID(u *uuid.UUID) string {
	if u == nil {
		return "<nil>"
	}
	return u.String()
}

// sprint renders a value for a diff line.
func sprint(v any) string { return fmt.Sprintf("%v", v) }

// Manifest backfill: writing sidecar manifests for files uploaded before
// manifests existed.
//
// Without this, reconstruction against an existing library correctly finds
// nothing and recovers nothing — the feature is present and useless.

// stripManifests deletes every manifest object from every account, leaving the
// shards untouched. It reproduces the state of a library uploaded before
// manifests shipped, which is the only state backfill exists to fix.
func stripManifests(t *testing.T, f *sharedDriveFixture) {
	t.Helper()
	ctx := context.Background()
	var removed int
	for _, acct := range f.accounts {
		backend := f.backends[acct]
		for name := range f.objects(t, acct) {
			if !files.IsManifestName(name) {
				continue
			}
			// The local backend addresses objects by name, so the name is the
			// provider id.
			if derr := backend.Delete(ctx, storage.ObjectRef{ProviderID: name}); derr != nil {
				t.Fatalf("delete manifest %s: %v", name, derr)
			}
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("stripManifests removed nothing; the fixture is not in the " +
			"state backfill is supposed to repair")
	}
	// Assert the premise rather than assume it.
	for _, acct := range f.accounts {
		for name := range f.objects(t, acct) {
			if files.IsManifestName(name) {
				t.Fatalf("a manifest survived stripManifests: %s", name)
			}
		}
	}
}

// THE EQUIVALENCE THAT MAKES BACKFILL WORTH ANYTHING.
//
// A backfilled manifest must be byte-identical to the one the upload path
// would have written. If it is not, the two records diverge and the difference
// surfaces only during a recovery — and a recovery is the one moment nobody
// can afford a surprise.
//
// Asserted on the SEALED BYTES, not on the decoded struct: field-by-field
// comparison would pass even if the two were serialised differently, and it is
// the stored object that a reconstruction reads.
func TestABackfilledManifestIsIdenticalToAnUploadTimeOne(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	// A file in a folder, so the derived folder_path is exercised too — that
	// is one of the two fields the pre-write audit found could diverge.
	folder, ferr := f.svc.CreateFolder(ctx, f.user1, nil, "Documents")
	if ferr != nil {
		t.Fatalf("CreateFolder() = %v", ferr)
	}
	fid := folder.ID
	data := randomBytes(t, 6<<20)
	file, uerr := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.user1, Name: "identical.bin", Size: int64(len(data)),
		FolderID: &fid, DeclaredMime: "application/octet-stream",
	}, bytes.NewReader(data))
	if uerr != nil {
		t.Fatalf("Upload() = %v", uerr)
	}

	atUpload := findAnyManifest(t, f, file.ID)
	if len(atUpload) == 0 {
		t.Fatal("the upload wrote no manifest; nothing to compare against")
	}

	// Now remove them and rebuild from the database alone.
	stripManifests(t, f)

	report, berr := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
	if berr != nil {
		t.Fatalf("BackfillManifests() = %v", berr)
	}
	if !report.Complete {
		t.Fatalf("backfill incomplete: %v", report.IncompleteReasons)
	}

	backfilled := findAnyManifest(t, f, file.ID)

	// The sealed bytes cannot be compared directly — the envelope carries a
	// random nonce, so two seals of identical plaintext differ. Compare the
	// PLAINTEXT both decrypt to, which is the thing a reconstruction reads.
	wantM, werr := files.OpenManifest(f.ring, file.ID, atUpload)
	if werr != nil {
		t.Fatalf("open the upload-time manifest: %v", werr)
	}
	gotM, gerr := files.OpenManifest(f.ring, file.ID, backfilled)
	if gerr != nil {
		t.Fatalf("open the backfilled manifest: %v", gerr)
	}

	if diff := manifestDiff(wantM, gotM); diff != "" {
		t.Errorf("a backfilled manifest DIFFERS from the upload-time one:\n%s\n"+
			"This difference would only surface during a recovery.", diff)
	}
}

// manifestDiff reports the first field on which two manifests disagree,
// returning "" when they are equivalent. Written out field by field rather
// than with reflect.DeepEqual so a failure names WHAT diverged.
func manifestDiff(want, got files.Manifest) string {
	var problems []string
	add := func(field string, w, g any) {
		problems = append(problems, "  "+field+": upload-time="+sprint(w)+" backfilled="+sprint(g))
	}
	if want.Version != got.Version {
		add("version", want.Version, got.Version)
	}
	if want.FileID != got.FileID {
		add("file_id", want.FileID, got.FileID)
	}
	if want.UserID != got.UserID {
		add("user_id", want.UserID, got.UserID)
	}
	if want.FileName != got.FileName {
		add("file_name", want.FileName, got.FileName)
	}
	if want.PlainSizeBytes != got.PlainSizeBytes {
		add("plain_size_bytes", want.PlainSizeBytes, got.PlainSizeBytes)
	}
	if want.MimeType != got.MimeType {
		add("mime_type", want.MimeType, got.MimeType)
	}
	if !want.CreatedAt.Equal(got.CreatedAt) {
		add("created_at", want.CreatedAt, got.CreatedAt)
	}
	if strings.Join(want.FolderPath, "/") != strings.Join(got.FolderPath, "/") {
		add("folder_path", want.FolderPath, got.FolderPath)
	}
	// The field the audit added. A nil here on either side means one of the
	// two paths is not recording encryption state.
	switch {
	case (want.IsEncrypted == nil) != (got.IsEncrypted == nil):
		add("is_encrypted (nil-ness)", want.IsEncrypted, got.IsEncrypted)
	case want.IsEncrypted != nil && *want.IsEncrypted != *got.IsEncrypted:
		add("is_encrypted", *want.IsEncrypted, *got.IsEncrypted)
	}
	if len(want.Shards) != len(got.Shards) {
		add("shard count", len(want.Shards), len(got.Shards))
		return strings.Join(problems, "\n")
	}
	for i := range want.Shards {
		w, g := want.Shards[i], got.Shards[i]
		// AccountID is a POINTER, so `w != g` compares addresses and reports a
		// difference between two structs whose every value is equal — which is
		// exactly what it did the first time this test ran, printing two
		// identical-looking lines. Compare the pointed-to values.
		if !sameAccount(w.AccountID, g.AccountID) {
			add("shard "+sprint(i)+" account_id", derefUUID(w.AccountID), derefUUID(g.AccountID))
			continue
		}
		w.AccountID, g.AccountID = nil, nil
		if w != g {
			add("shard "+sprint(i), w, g)
		}
	}
	return strings.Join(problems, "\n")
}

// Backfill covers a library that has none, and reports coverage honestly.
func TestBackfillCoversAnUncoveredLibrary(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	for _, name := range []string{"one.bin", "two.bin", "three.bin"} {
		f.uploadAs(t, f.user1, name, randomBytes(t, 3<<20))
	}
	stripManifests(t, f)

	report, err := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("BackfillManifests() = %v", err)
	}
	if !report.Complete {
		t.Fatalf("incomplete: %v", report.IncompleteReasons)
	}

	if report.Coverage.Files != 3 {
		t.Errorf("files = %d, want 3", report.Coverage.Files)
	}
	if report.Coverage.Covered != 3 {
		t.Errorf("covered = %d, want 3 (partial=%d uncovered=%d)",
			report.Coverage.Covered, report.Coverage.PartiallyCovered,
			report.Coverage.Uncovered)
	}
	for _, r := range report.Results {
		if r.State != files.BackfillWritten {
			t.Errorf("file %s: state = %q, want %q", r.FileID, r.State, files.BackfillWritten)
		}
		if r.Copies != r.Accounts {
			t.Errorf("file %s: %d copies across %d accounts; every participating "+
				"account must hold one or a single surviving drive loses the map",
				r.FileID, r.Copies, r.Accounts)
		}
	}
}

// IDEMPOTENT. Three runs: the first writes, the second and third write nothing.
func TestBackfillIsIdempotentAcrossThreeRuns(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	f.uploadAs(t, f.user1, "thrice.bin", randomBytes(t, 6<<20))
	stripManifests(t, f)

	var reports []files.BackfillReport
	for i := 0; i < 3; i++ {
		r, err := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		reports = append(reports, r)
	}

	if reports[0].Results[0].State != files.BackfillWritten {
		t.Errorf("run 1 state = %q, want %q", reports[0].Results[0].State, files.BackfillWritten)
	}
	for i, r := range reports[1:] {
		if got := r.Results[0].State; got != files.BackfillAlreadyCovered {
			t.Errorf("run %d state = %q, want %q: it is rewriting covered files",
				i+2, got, files.BackfillAlreadyCovered)
		}
		if r.Coverage.Covered != 1 {
			t.Errorf("run %d covered = %d, want 1", i+2, r.Coverage.Covered)
		}
	}

	// And exactly one manifest per account, not three.
	for _, acct := range f.accounts {
		var count int
		for name := range f.objects(t, acct) {
			if files.IsManifestName(name) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("account %s holds %d manifests after three runs, want 1", acct, count)
		}
	}
}

// A DAMAGED FILE IS SKIPPED, and reported as skipped rather than failed.
//
// A manifest for a file with a confirmed-missing shard promises a recovery
// that cannot happen: reconstruction would rebuild rows pointing at objects
// that are gone, and the user finds out at download time.
func TestBackfillSkipsFilesReconcileMarkedDamaged(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	healthy := f.uploadAs(t, f.user1, "healthy.bin", randomBytes(t, 3<<20))
	damaged := f.uploadAs(t, f.user1, "damaged.bin", randomBytes(t, 3<<20))
	stripManifests(t, f)

	if err := f.store.RecordReconciledHealth(ctx, f.user1, damaged.ID,
		files.StatusPartiallyMissing, time.Now()); err != nil {
		t.Fatalf("RecordReconciledHealth() = %v", err)
	}

	report, err := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("BackfillManifests() = %v", err)
	}

	byID := map[uuid.UUID]files.BackfillResult{}
	for _, r := range report.Results {
		byID[r.FileID] = r
	}

	if got := byID[damaged.ID].State; got != files.BackfillSkippedDamaged {
		t.Errorf("damaged file state = %q, want %q", got, files.BackfillSkippedDamaged)
	}
	if got := byID[healthy.ID].State; got != files.BackfillWritten {
		t.Errorf("healthy file state = %q, want %q", got, files.BackfillWritten)
	}
	if report.Coverage.Damaged != 1 {
		t.Errorf("coverage.damaged = %d, want 1", report.Coverage.Damaged)
	}

	// And no manifest was written for it.
	want := files.ManifestName(damaged.ID)
	for _, acct := range f.accounts {
		if _, ok := f.objects(t, acct)[want]; ok {
			t.Errorf("a manifest was written for a damaged file on account %s", acct)
		}
	}
}

// THREE-STATE: an unreachable drive is INDETERMINATE, not failed and not
// covered.
//
// AND THE REASON MUST BE THE RATE LIMIT. The Block 3 throttling test passed
// for an unrelated cause — the wrapper did not implement Lister, so the
// account was indeterminate because listing was unsupported and the
// rate-limit path never executed. Asserting the reason is what makes this
// test about throttling rather than about wrapper plumbing.
func TestBackfillTreatsAnUnreachableDriveAsIndeterminate(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	f.uploadAs(t, f.user1, "unreachable.bin", randomBytes(t, 6<<20))
	stripManifests(t, f)

	for id := range f.backends {
		f.throttle(id)
	}

	report, err := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("BackfillManifests() = %v", err)
	}

	if report.Complete {
		t.Error("a fully rate-limited run reported itself COMPLETE")
	}
	if report.Coverage.Covered != 0 {
		t.Errorf("covered = %d from a run that listed nothing", report.Coverage.Covered)
	}
	if report.Coverage.Indeterminate != 1 {
		t.Errorf("indeterminate = %d, want 1", report.Coverage.Indeterminate)
	}
	for _, r := range report.Results {
		if r.State != files.BackfillIndeterminate {
			t.Errorf("state = %q, want %q", r.State, files.BackfillIndeterminate)
		}
	}
	for _, scan := range report.Accounts {
		if scan.Scanned {
			t.Errorf("account %s reported as scanned while rate limited", scan.AccountID)
		}
		if !strings.Contains(scan.Reason, "rate limiting") {
			t.Errorf("account %s reason = %q, want the RATE LIMIT reason; an "+
				"indeterminate result for a different cause means this test is "+
				"not exercising throttling at all", scan.AccountID, scan.Reason)
		}
	}
}

// A manifest written before `user_email` existed cannot be repaired from the
// drives — only regenerated from the database. Ordinary backfill SKIPS it,
// because a manifest is already there, so a rewrite mode is the only way to
// make an existing library recoverable after issue #52.
//
// THIS IS THE UPGRADE PATH FOR A REAL LIBRARY. Without it, every file uploaded
// before the fix stays permanently unrecoverable after a database rebuild,
// while coverage cheerfully reports 100%.
func TestRewriteRepairsManifestsMissingTheEmailAnchor(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	file := f.uploadAs(t, f.user1, "legacy.bin", randomBytes(t, 3<<20))

	// Simulate a pre-fix manifest: same file, no email. Written by hand
	// because no current code path can produce one.
	stored, gerr := f.svc.Get(ctx, f.user1, file.ID)
	if gerr != nil {
		t.Fatalf("Get() = %v", gerr)
	}
	legacy := files.ManifestFor(stored, nil, "") // no email, as before the fix
	sealed, serr := files.SealManifest(f.ring, legacy)
	if serr != nil {
		t.Fatalf("SealManifest() = %v", serr)
	}
	for _, acct := range f.accounts {
		if _, perr := f.backends[acct].Put(ctx, bytes.NewReader(sealed), storage.ObjectSpec{
			Name: files.ManifestName(file.ID), Size: int64(len(sealed)),
		}); perr != nil {
			t.Fatalf("overwrite with a legacy manifest: %v", perr)
		}
	}

	// Ordinary backfill sees a manifest and leaves it alone — correct, and
	// exactly why a rewrite mode is needed.
	plain, berr := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
	if berr != nil {
		t.Fatalf("BackfillManifests() = %v", berr)
	}
	if plain.Results[0].State != files.BackfillAlreadyCovered {
		t.Errorf("plain backfill state = %q, want %q", plain.Results[0].State,
			files.BackfillAlreadyCovered)
	}
	after := findAnyManifest(t, f, file.ID)
	m, oerr := files.OpenManifest(f.ring, file.ID, after)
	if oerr != nil {
		t.Fatalf("OpenManifest() = %v", oerr)
	}
	if m.UserEmail != "" {
		t.Fatal("the legacy manifest was already repaired; this test proves nothing")
	}

	// The rewrite repairs it.
	if _, rerr := f.svc.RewriteManifests(ctx, f.user1, f.accounts); rerr != nil {
		t.Fatalf("RewriteManifests() = %v", rerr)
	}
	repaired, oerr2 := files.OpenManifest(f.ring, file.ID, findAnyManifest(t, f, file.ID))
	if oerr2 != nil {
		t.Fatalf("OpenManifest() after rewrite = %v", oerr2)
	}
	if repaired.UserEmail != email {
		t.Errorf("user_email = %q after a rewrite, want %q; the library is still "+
			"unrecoverable after a database rebuild", repaired.UserEmail, email)
	}
}
