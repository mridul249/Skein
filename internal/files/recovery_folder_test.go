package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
)

// THE SECOND HALF OF THE 2026-08-06 RECOVERY FAILURE.
//
// The first half was that listing was scoped to the app folder, whose name is
// derived from the user id — so a rebuilt database looked in the wrong place
// and found nothing (see storage/gdrive/list_test.go).
//
// Fixing that makes every file come back, and leaves a subtler problem behind:
// the account is still BOUND to the freshly created empty folder, so recovery
// succeeds and then every new upload goes somewhere other than where the
// recovered shards live. The user is left with two Skein folders and no way to
// know which is which.
//
// Reconstruction is the one place that knows the answer, having just read
// manifests out of the right folder. These tests pin that it uses it.

// testRebinder records the folder each account was repointed at.
type testRebinder struct {
	mu    sync.Mutex
	calls map[uuid.UUID]string
	fail  error
}

func newTestRebinder() *testRebinder {
	return &testRebinder{calls: map[uuid.UUID]string{}}
}

func (r *testRebinder) RebindAppFolder(_ context.Context, accountID uuid.UUID, folderID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.calls[accountID] = folderID
	return nil
}

func (r *testRebinder) got(accountID uuid.UUID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[accountID]
}

func (r *testRebinder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// After a rebuild, every drive that yielded recovered files must be repointed
// at the folder those files were actually found in.
func TestRecoveryRepointsEachDriveAtTheFolderHoldingItsFiles(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	f.uploadAs(t, f.user1, "striped.bin", randomBytes(t, 6<<20))

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	rb := newTestRebinder()
	f.svc.SetFolderRebinder(rb)

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 1 {
		t.Fatalf("recovered %d of 1 file; the rebind assertions below assume a working recovery",
			report.FilesRecovered)
	}

	// A striped file puts a manifest on BOTH drives, so both get repointed.
	if rb.count() != len(f.accounts) {
		t.Fatalf("repointed %d of %d drives", rb.count(), len(f.accounts))
	}
	for _, acct := range f.accounts {
		// f.roots is the local backend's directory, which is what its List
		// reports as the parent — the fixture's stand-in for a Drive folder id.
		want := f.roots[acct]
		if got := rb.got(acct); got != want {
			t.Errorf("account %s repointed at %q, want the folder its manifests were found in, %q",
				acct, got, want)
		}
	}
}

// A DRY RUN MUST NOT WRITE. The preview exists so someone can see what
// recovery would do before anything changes; a rebind is a change.
func TestRecoveryDryRunDoesNotRepointAnything(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	f.uploadAs(t, f.user1, "striped.bin", randomBytes(t, 6<<20))

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	rb := newTestRebinder()
	f.svc.SetFolderRebinder(rb)

	report, err := f.svc.ReconstructDryRun(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("ReconstructDryRun() = %v", err)
	}
	if report.ManifestsFound == 0 {
		t.Fatal("the dry run found no manifests; it is not exercising the path being asserted")
	}
	if rb.count() != 0 {
		t.Errorf("a dry run repointed %d drive(s); it must write nothing", rb.count())
	}
}

// ISOLATION. Two Skein users share a Google account, so a drive holds both
// users' manifests. Repointing on any manifest found would aim one user's
// account at the other user's folder. Only CLAIMED manifests may count.
func TestRecoveryNeverRepointsAtAnotherUsersFolder(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	// Only user2 has files. user1 is recovering and owns nothing.
	f.dir.set(f.user2, "other@example.com")
	f.uploadAs(t, f.user2, "not-yours.bin", randomBytes(t, 6<<20))

	stranger := uuid.New()
	f.dir.set(stranger, "stranger@example.com")

	rb := newTestRebinder()
	f.svc.SetFolderRebinder(rb)

	report, err := f.svc.Reconstruct(ctx, stranger, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 0 {
		t.Fatalf("a stranger recovered %d files", report.FilesRecovered)
	}
	if report.ManifestsFound == 0 {
		t.Fatal("no manifests were visible to the stranger; the isolation " +
			"assertion below would pass for the wrong reason")
	}
	if rb.count() != 0 {
		t.Errorf("repointed %d drive(s) using manifests belonging to another user", rb.count())
	}
}

// THE SHARP VERSION OF THE ISOLATION CHECK, and the reason the one above is
// not enough on its own.
//
// TestRecoveryNeverRepointsAtAnotherUsersFolder passes even if the folder vote
// ignores ownership entirely, because a user who recovers NOTHING is already
// stopped by the "no claimed manifests" guard — verified by mutation. The
// dangerous case is a user who recovers SOMETHING while another user's data
// sits on the same drive in a different folder: get the filter wrong and the
// account is repointed at a folder it does not own, and every later upload
// goes there.
//
// This needs two folders on one drive, which the local backend cannot express
// on its own; see multiResolver.parents.
func TestRecoveryRepointsAtTheOwnFolderNotTheBusiestOne(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	// The other user gets THREE files, so a vote that ignored ownership would
	// be outnumbered and pick their folder.
	f.dir.set(f.user2, "other@example.com")
	// Small files: the fixture's drives hold 8 MiB each, and this test needs
	// four striped files to fit rather than one big one.
	for _, name := range []string{"theirs-a.bin", "theirs-b.bin", "theirs-c.bin"} {
		f.uploadAs(t, f.user2, name, randomBytes(t, 2<<20))
	}
	// Everything written so far belongs to user2 and lives in their folder.
	for _, acct := range f.accounts {
		for name := range f.objects(t, acct) {
			(*f.parents)[name] = "their-folder"
		}
	}

	// The recovering user gets ONE.
	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	f.uploadAs(t, f.user1, "mine.bin", randomBytes(t, 2<<20))
	for _, acct := range f.accounts {
		for name := range f.objects(t, acct) {
			if _, already := (*f.parents)[name]; !already {
				(*f.parents)[name] = "my-folder"
			}
		}
	}

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	rb := newTestRebinder()
	f.svc.SetFolderRebinder(rb)

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 1 {
		t.Fatalf("recovered %d of 1 owned file", report.FilesRecovered)
	}
	if report.ManifestsForOtherUsers == 0 {
		t.Fatal("no other-user manifests were seen; this test is not exercising " +
			"the case it exists for")
	}

	for _, acct := range f.accounts {
		got := rb.got(acct)
		if got == "their-folder" {
			t.Errorf("account %s was repointed at ANOTHER USER'S folder", acct)
		}
		if got != "my-folder" {
			t.Errorf("account %s repointed at %q, want %q", acct, got, "my-folder")
		}
	}
}

// A rebind failure must not turn a successful recovery into a failed one.
// Every file is already restored by the time this runs; the rebind only
// decides where the next upload goes.
func TestARebindFailureDoesNotFailTheRecovery(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	original := randomBytes(t, 6<<20)
	f.uploadAs(t, f.user1, "striped.bin", original)

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	rb := newTestRebinder()
	rb.fail = errors.New("the database rejected the write")
	f.svc.SetFolderRebinder(rb)

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v, want a rebind failure to be survivable", err)
	}
	if report.FilesRecovered != 1 {
		t.Fatalf("recovered %d of 1 file when the rebind failed", report.FilesRecovered)
	}
	if !report.Complete {
		t.Error("a failed rebind marked the recovery incomplete; the files all came back")
	}

	// And the bytes are still right.
	listed, lerr := f.svc.List(ctx, rebuilt, files.ListParams{Limit: 10})
	if lerr != nil || len(listed) != 1 {
		t.Fatalf("List() = %v, %v", listed, lerr)
	}
	content, oerr := f.svc.Open(ctx, rebuilt, listed[0].ID, nil)
	if oerr != nil {
		t.Fatalf("Open() after a failed rebind = %v", oerr)
	}
	got, rerr := io.ReadAll(content.Body)
	_ = content.Body.Close()
	if rerr != nil {
		t.Fatalf("read after a failed rebind = %v", rerr)
	}
	if !bytes.Equal(got, original) {
		t.Error("the recovered file's bytes do not match the original")
	}
}

// Recovery must work with no rebinder wired at all — that is the tests' own
// configuration, and it must stay a degraded-but-correct path rather than a
// nil dereference.
func TestRecoveryWorksWithNoRebinderWired(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	f.uploadAs(t, f.user1, "striped.bin", randomBytes(t, 6<<20))

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 1 {
		t.Fatalf("recovered %d of 1 file with no rebinder wired", report.FilesRecovered)
	}
}

// "RECOVERED 7 FILES, 0 SHARDS" — the state a live user was left in on
// 2026-08-06, after the folder fix above made their files findable again.
//
// A manifest records each shard's location as a connected-account id, which is
// database-local: reconnecting a drive after a rebuild mints a new one. Every
// shard insert then failed its foreign key while the file row survived, so the
// library listed, previewed, and could not download a single byte.
//
// The fix is to stop trusting a recorded id that cannot survive and resolve
// each shard against WHERE ITS OBJECT ACTUALLY IS. This test is the proof, and
// it is written as a download rather than a row count on purpose: shard rows
// that exist but point at the wrong drive pass every count assertion and still
// hand the user a broken file.
func TestRecoveredFilesHaveWorkingShardsAfterDrivesAreReconnected(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	original := randomBytes(t, 6<<20)
	f.uploadAs(t, f.user1, "striped.bin", original)

	// destroyDatabase also re-mints every connected-account id, exactly as
	// reconnecting the drives does. See sharedDriveFixture.reconnectDrives.
	before := append([]uuid.UUID(nil), f.accounts...)
	f.destroyDatabase(t)
	for i := range before {
		if before[i] == f.accounts[i] {
			t.Fatal("the drives kept their old ids across the rebuild; this test " +
				"is not exercising the case it exists for")
		}
	}

	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 1 {
		t.Fatalf("recovered %d of 1 file", report.FilesRecovered)
	}
	if report.ShardsRecovered == 0 {
		t.Fatalf("recovered %d file(s) and ZERO shards — the file is listable and "+
			"undownloadable, which is worse than not recovering it",
			report.FilesRecovered)
	}
	if report.ShardsUnresolved != 0 {
		t.Errorf("%d shard(s) could not be located on any drive", report.ShardsUnresolved)
	}

	// Every shard must point at a drive that REALLY holds its object.
	listed, lerr := f.svc.List(ctx, rebuilt, files.ListParams{Limit: 10})
	if lerr != nil || len(listed) != 1 {
		t.Fatalf("List() = %v, %v", listed, lerr)
	}
	file, gerr := f.svc.Get(ctx, rebuilt, listed[0].ID)
	if gerr != nil {
		t.Fatalf("Get() = %v", gerr)
	}
	if len(file.Shards) == 0 {
		t.Fatal("the recovered file has no shard rows")
	}
	for _, sh := range file.Shards {
		if sh.AccountID == nil {
			t.Fatalf("shard %d has no account", sh.Index)
		}
		if _, ok := f.objects(t, *sh.AccountID)[sh.ProviderID]; !ok {
			t.Errorf("shard %d points at account %s, which does not hold object %q",
				sh.Index, *sh.AccountID, sh.ProviderID)
		}
	}

	// THE ASSERTION THAT MATTERS. Row counts prove nothing; bytes do.
	content, oerr := f.svc.Open(ctx, rebuilt, listed[0].ID, nil)
	if oerr != nil {
		t.Fatalf("Open() after recovery = %v", oerr)
	}
	got, rerr := io.ReadAll(content.Body)
	_ = content.Body.Close()
	if rerr != nil {
		t.Fatalf("read after recovery = %v", rerr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("recovered %d bytes, want %d, and they differ", len(got), len(original))
	}
}

// The preview must promise exactly what the run delivers. Counting every
// shard in the manifest as recoverable would have shown "14 shard records
// added" for a run that then added none.
func TestThePreviewCountsOnlyShardsTheRunCanActuallyPlace(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	f.uploadAs(t, f.user1, "striped.bin", randomBytes(t, 6<<20))

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	// ONE DRIVE UNREADABLE, and that is what makes this test discriminating.
	// With every drive readable, "every shard in the manifest" and "every
	// shard that can be placed" are the same number, so a preview that simply
	// counted len(shards) would agree with the run by coincidence. Shards on
	// the throttled drive cannot be located, so the two numbers only match if
	// the preview is doing the same resolution the run does.
	f.throttle(f.accounts[0])

	preview, perr := f.svc.ReconstructDryRun(ctx, rebuilt, f.accounts)
	if perr != nil {
		t.Fatalf("ReconstructDryRun() = %v", perr)
	}
	real, rerr := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if rerr != nil {
		t.Fatalf("Reconstruct() = %v", rerr)
	}

	if preview.ShardsRecovered != real.ShardsRecovered {
		t.Errorf("the preview promised %d shards and the run added %d",
			preview.ShardsRecovered, real.ShardsRecovered)
	}
	if preview.FilesRecovered != real.FilesRecovered {
		t.Errorf("the preview promised %d files and the run added %d",
			preview.FilesRecovered, real.FilesRecovered)
	}
	if preview.ShardsUnresolved != real.ShardsUnresolved {
		t.Errorf("the preview reported %d unresolved shards and the run reported %d",
			preview.ShardsUnresolved, real.ShardsUnresolved)
	}
	if real.ShardsRecovered == 0 {
		t.Fatal("both agreed on zero placed shards; they agree for the wrong reason")
	}
	if real.ShardsUnresolved == 0 {
		t.Fatal("no shard was unplaceable, so a preview that ignored resolution " +
			"entirely would still have matched; this test proves nothing")
	}
}

// A drive that cannot be read must not cause its shards to be silently
// dropped. They are reported unresolved and the run says it is incomplete, so
// a re-run with the drive back completes it.
func TestShardsOnAnUnreadableDriveAreReportedNotDiscarded(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)
	original := randomBytes(t, 6<<20)
	f.uploadAs(t, f.user1, "striped.bin", original)

	f.destroyDatabase(t)
	rebuilt := uuid.New()
	f.dir.set(rebuilt, email)

	// One drive is throttled, so its listing fails and nothing on it can be
	// located.
	f.throttle(f.accounts[0])

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.Complete {
		t.Error("a run that could not read a drive reported itself complete")
	}
	if report.ShardsUnresolved == 0 {
		t.Error("shards on the unreadable drive were neither placed nor reported; " +
			"silently dropping them is how a file becomes undownloadable with no " +
			"indication anything is wrong")
	}

	// The drive comes back. A re-run must complete the job.
	delete(f.throttled, f.accounts[0])
	second, serr := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if serr != nil {
		t.Fatalf("second Reconstruct() = %v", serr)
	}
	if second.ShardsUnresolved != 0 {
		t.Errorf("%d shard(s) still unresolved after every drive came back",
			second.ShardsUnresolved)
	}

	content, oerr := f.svc.Open(ctx, rebuilt, mustOneFileID(t, f, rebuilt), nil)
	if oerr != nil {
		t.Fatalf("Open() after the re-run = %v", oerr)
	}
	got, rerr := io.ReadAll(content.Body)
	_ = content.Body.Close()
	if rerr != nil {
		t.Fatalf("read after the re-run = %v", rerr)
	}
	if !bytes.Equal(got, original) {
		t.Error("the file does not read back correctly after the completing re-run")
	}
}

func mustOneFileID(t *testing.T, f *sharedDriveFixture, userID uuid.UUID) uuid.UUID {
	t.Helper()
	listed, err := f.svc.List(context.Background(), userID, files.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List() returned %d files, want 1", len(listed))
	}
	return listed[0].ID
}
