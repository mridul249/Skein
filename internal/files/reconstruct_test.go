package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
)

// Reconstruction: rebuilding the database from the sidecar manifests on the
// drives. This is the block that proves the whole thing — Block 2 put the
// record on the drives, and until something reads it back, that record is a
// claim rather than a capability.
//
// FIXTURE HONESTY, checked rather than assumed. Every assertion below depends
// on the fixture destroying the database for real and on the manifests being
// read from the provider rather than from anything the service remembers.
// `f.objects()` reads the backend's own directory, and
// `TestTheFixtureActuallyDestroysTheDatabase` proves the destruction is real
// before any recovery is asserted. Without that, a "recovery" test could pass
// by the old rows never having gone away.

// THE FIXTURE CHECK. If this fails, every recovery assertion below is
// meaningless however green it looks.
func TestTheFixtureActuallyDestroysTheDatabase(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "will-be-lost.bin", data)

	// Present before.
	if _, err := f.svc.Get(ctx, f.user1, file.ID); err != nil {
		t.Fatalf("the file is not readable before the database is destroyed: %v", err)
	}

	f.destroyDatabase(t)

	// Gone after — and gone from the LISTING too, not merely unreadable by id.
	if _, err := f.svc.Get(ctx, f.user1, file.ID); err == nil {
		t.Fatal("the file is still in the database after destroyDatabase; " +
			"nothing below this proves recovery")
	}
	listed, err := f.svc.List(ctx, f.user1, files.ListParams{Limit: 50})
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("%d file(s) survived destroyDatabase", len(listed))
	}

	// And the SHARDS are untouched at the provider, which is the premise of
	// the whole feature: the database died, the bytes did not.
	var objects int
	for _, acct := range f.accounts {
		objects += len(f.objects(t, acct))
	}
	if objects == 0 {
		t.Fatal("destroyDatabase also removed the provider objects; " +
			"that is a fixture that destroys the thing being recovered FROM")
	}
	t.Logf("database destroyed; %d provider object(s) survive", objects)
}

// THE TEST THAT ESTABLISHES THE PROPERTY.
//
// Several striped files across multiple accounts, one of them in a folder. The
// database is destroyed entirely. Reconstruction runs. Every file must reappear
// with the correct name, size, folder and shard mapping — and then one is
// DOWNLOADED and its bytes compared against the original.
//
// ROW COUNTS PROVE NOTHING. A reconstruction that produces plausible rows
// pointing at the wrong shards passes every count assertion and hands the user
// garbage. Byte-for-byte is the test; everything above it is diagnosis for when
// the bytes do not match.
func TestReconstructRecoversFilesByteForByteAfterTheDatabaseIsDestroyed(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	type original struct {
		name   string
		data   []byte
		folder string
		id     uuid.UUID
	}
	originals := []*original{
		{name: "alpha.bin", data: randomBytes(t, 6<<20)},                      // striped
		{name: "beta.bin", data: randomBytes(t, 5<<20)},                       // striped
		{name: "gamma.bin", data: randomBytes(t, 2<<20), folder: "Documents"}, // in a folder
	}

	for _, o := range originals {
		var folderID *uuid.UUID
		if o.folder != "" {
			folder, ferr := f.svc.CreateFolder(ctx, f.user1, nil, o.folder)
			if ferr != nil {
				t.Fatalf("CreateFolder(%q) = %v", o.folder, ferr)
			}
			id := folder.ID
			folderID = &id
		}
		file, err := f.svc.Upload(ctx, files.UploadRequest{
			UserID: f.user1, Name: o.name, Size: int64(len(o.data)), FolderID: folderID,
		}, bytes.NewReader(o.data))
		if err != nil {
			t.Fatalf("Upload(%q) = %v", o.name, err)
		}
		o.id = file.ID
	}

	// Record the true shard layout while the database still knows it, so the
	// comparison afterwards is against reality rather than against the
	// manifest that is also under test.
	before := map[uuid.UUID]files.File{}
	for _, o := range originals {
		stored, err := f.svc.Get(ctx, f.user1, o.id)
		if err != nil {
			t.Fatalf("Get(%q) = %v", o.name, err)
		}
		if len(stored.Shards) < 2 && o.name != "gamma.bin" {
			t.Fatalf("%s has %d shard(s); this test needs striped files",
				o.name, len(stored.Shards))
		}
		before[o.id] = stored
	}

	// ---- THE DATABASE IS DESTROYED ----
	f.destroyDatabase(t)

	report, err := f.svc.Reconstruct(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if !report.Complete {
		t.Fatalf("reconstruct reported incomplete: %v", report.IncompleteReasons)
	}
	t.Logf("recovered %d file(s), %d shard(s), %d folder(s) from %d manifest(s)",
		report.FilesRecovered, report.ShardsRecovered,
		report.FoldersRecovered, report.ManifestsFound)

	if report.FilesRecovered != len(originals) {
		t.Errorf("recovered %d files, want %d", report.FilesRecovered, len(originals))
	}

	// Every file reappears, with the right metadata and the right map.
	for _, o := range originals {
		want := before[o.id]
		got, gerr := f.svc.Get(ctx, f.user1, o.id)
		if gerr != nil {
			t.Errorf("%s did not come back: %v", o.name, gerr)
			continue
		}
		if got.Name != want.Name {
			t.Errorf("%s: name = %q, want %q", o.name, got.Name, want.Name)
		}
		if got.SizeBytes != want.SizeBytes {
			t.Errorf("%s: size = %d, want %d", o.name, got.SizeBytes, want.SizeBytes)
		}
		if (got.FolderID == nil) != (want.FolderID == nil) {
			t.Errorf("%s: folder presence differs from the original", o.name)
		}
		if len(got.Shards) != len(want.Shards) {
			t.Errorf("%s: %d shards, want %d", o.name, len(got.Shards), len(want.Shards))
			continue
		}
		for i := range want.Shards {
			w, g := want.Shards[i], got.Shards[i]
			if g.Index != w.Index || g.ProviderID != w.ProviderID ||
				g.PlainOffset != w.PlainOffset || g.PlainSize != w.PlainSize ||
				g.SizeBytes != w.SizeBytes {
				t.Errorf("%s shard %d: recovered mapping differs from the original\n got=%+v\nwant=%+v",
					o.name, i, g, w)
			}
			// NOT compared against the ORIGINAL account id, which is exactly
			// the thing a rebuild destroys: reconnecting a drive mints a new
			// connected-account id, so the pre-loss id is gone and asserting
			// on it would demand behaviour that cannot exist. What must hold
			// is that the shard points at the drive REALLY HOLDING the object
			// — checked against the provider, not against either database.
			if g.AccountID == nil {
				t.Errorf("%s shard %d: recovered with no account; the drive holding it is unknown",
					o.name, i)
				continue
			}
			if _, ok := f.objects(t, *g.AccountID)[g.ProviderID]; !ok {
				t.Errorf("%s shard %d: points at account %s, which does not hold object %q",
					o.name, i, *g.AccountID, g.ProviderID)
			}
		}
	}

	// The folder came back, by name and by shape.
	folders, ferr := f.svc.ListFolders(ctx, f.user1)
	if ferr != nil {
		t.Fatalf("ListFolders() = %v", ferr)
	}
	var haveDocuments bool
	for _, fl := range folders {
		if fl.Name == "Documents" && fl.ParentID == nil {
			haveDocuments = true
		}
	}
	if !haveDocuments {
		t.Error("the folder \"Documents\" was not recreated")
	}

	// ---- AND THE BYTES. This is the test. ----
	for _, o := range originals {
		content, oerr := f.svc.Open(ctx, f.user1, o.id, nil)
		if oerr != nil {
			t.Errorf("%s: Open() after reconstruction = %v", o.name, oerr)
			continue
		}
		got, rerr := io.ReadAll(content.Body)
		_ = content.Body.Close()
		if rerr != nil {
			t.Errorf("%s: read after reconstruction = %v", o.name, rerr)
			continue
		}
		if !bytes.Equal(got, o.data) {
			t.Errorf("%s: BYTES DIFFER after reconstruction (%d bytes read, %d original). "+
				"The rows are plausible and point at the wrong data.",
				o.name, len(got), len(o.data))
			continue
		}
		t.Logf("%s: %d bytes recovered byte-for-byte", o.name, len(got))
	}
}

// THE DRY RUN WRITES NOTHING, which is the claim the two-step Recovery UI
// rests on: the user is shown what a restore would do, and is told nothing has
// happened yet. If a "scan" mutated the database, that sentence is a lie and
// the confirmation step is theatre.
//
// Asserted by destroying the database, scanning, and checking the database is
// STILL EMPTY — not by trusting the report's own dry_run flag.
func TestReconstructDryRunWritesNothing(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := randomBytes(t, 6<<20)
	file := f.uploadAs(t, f.user1, "preview.bin", data)
	f.destroyDatabase(t)

	preview, err := f.svc.ReconstructDryRun(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("ReconstructDryRun() = %v", err)
	}
	if !preview.DryRun {
		t.Error("the report does not mark itself as a dry run")
	}
	// It must still REPORT what it would do, or the preview is useless.
	if preview.FilesRecovered != 1 {
		t.Errorf("preview reported %d files recoverable, want 1", preview.FilesRecovered)
	}
	if preview.ShardsRecovered == 0 {
		t.Error("preview reported no shards recoverable")
	}

	// ...and the database is untouched.
	if _, gerr := f.svc.Get(ctx, f.user1, file.ID); gerr == nil {
		t.Fatal("the dry run WROTE the file row; the UI's 'nothing has been " +
			"written yet' is a lie and the confirm step is theatre")
	}
	listed, lerr := f.svc.List(ctx, f.user1, files.ListParams{Limit: 50})
	if lerr != nil {
		t.Fatalf("List() = %v", lerr)
	}
	if len(listed) != 0 {
		t.Errorf("%d files in the listing after a dry run, want 0", len(listed))
	}

	// And a real run afterwards still recovers everything — the preview must
	// not have consumed anything.
	real, rerr := f.svc.Reconstruct(ctx, f.user1, f.accounts)
	if rerr != nil {
		t.Fatalf("Reconstruct() after a dry run = %v", rerr)
	}
	if real.FilesRecovered != 1 {
		t.Errorf("the real run recovered %d files after a preview, want 1",
			real.FilesRecovered)
	}
	if _, gerr := f.svc.Get(ctx, f.user1, file.ID); gerr != nil {
		t.Errorf("the file did not come back after the real run: %v", gerr)
	}
}

// The preview's numbers must match what the real run actually does. A preview
// that overstates sends someone into a restore expecting more than they get.
func TestTheDryRunPredictsWhatTheRealRunDoes(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	folder, ferr := f.svc.CreateFolder(ctx, f.user1, nil, "Docs")
	if ferr != nil {
		t.Fatalf("CreateFolder() = %v", ferr)
	}
	fid := folder.ID
	for i := 0; i < 3; i++ {
		data := randomBytes(t, 1<<20)
		var target *uuid.UUID
		if i == 0 {
			target = &fid
		}
		if _, uerr := f.svc.Upload(ctx, files.UploadRequest{
			UserID: f.user1, Name: fmt.Sprintf("f%d.bin", i),
			Size: int64(len(data)), FolderID: target,
		}, bytes.NewReader(data)); uerr != nil {
			t.Fatalf("Upload(%d) = %v", i, uerr)
		}
	}
	f.destroyDatabase(t)

	preview, err := f.svc.ReconstructDryRun(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("ReconstructDryRun() = %v", err)
	}
	real, rerr := f.svc.Reconstruct(ctx, f.user1, f.accounts)
	if rerr != nil {
		t.Fatalf("Reconstruct() = %v", rerr)
	}

	if preview.FilesRecovered != real.FilesRecovered {
		t.Errorf("preview said %d files, the run recovered %d",
			preview.FilesRecovered, real.FilesRecovered)
	}
	if preview.ShardsRecovered != real.ShardsRecovered {
		t.Errorf("preview said %d shards, the run recovered %d",
			preview.ShardsRecovered, real.ShardsRecovered)
	}
	if preview.FoldersRecovered != real.FoldersRecovered {
		t.Errorf("preview said %d folders, the run recreated %d",
			preview.FoldersRecovered, real.FoldersRecovered)
	}
}

// IDEMPOTENT. Three runs against a live database: no duplicate rows, no
// corruption, and the second and third runs recover nothing because there is
// nothing left to recover.
func TestReconstructIsIdempotentAcrossThreeRuns(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := randomBytes(t, 6<<20)
	file := f.uploadAs(t, f.user1, "thrice.bin", data)
	f.destroyDatabase(t)

	var reports []files.ReconstructReport
	for i := 0; i < 3; i++ {
		r, err := f.svc.Reconstruct(ctx, f.user1, f.accounts)
		if err != nil {
			t.Fatalf("run %d: Reconstruct() = %v", i+1, err)
		}
		reports = append(reports, r)
	}

	// Run 1 recovers; runs 2 and 3 find everything already present.
	if reports[0].FilesRecovered != 1 {
		t.Errorf("run 1 recovered %d files, want 1", reports[0].FilesRecovered)
	}
	for i, r := range reports[1:] {
		if r.FilesRecovered != 0 {
			t.Errorf("run %d recovered %d files, want 0: it is duplicating work",
				i+2, r.FilesRecovered)
		}
		if r.ShardsRecovered != 0 {
			t.Errorf("run %d recovered %d shards, want 0: DUPLICATE SHARD ROWS",
				i+2, r.ShardsRecovered)
		}
		if r.FilesAlreadyPresent != 1 {
			t.Errorf("run %d saw %d files already present, want 1",
				i+2, r.FilesAlreadyPresent)
		}
	}

	// No duplicates, and the file is still readable after three runs.
	listed, err := f.svc.List(ctx, f.user1, files.ListParams{Limit: 50})
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("%d files in the listing after three runs, want 1", len(listed))
	}
	stored, gerr := f.svc.Get(ctx, f.user1, file.ID)
	if gerr != nil {
		t.Fatalf("Get() after three runs = %v", gerr)
	}
	seen := map[int32]bool{}
	for _, sh := range stored.Shards {
		if seen[sh.Index] {
			t.Errorf("shard index %d appears more than once after three runs", sh.Index)
		}
		seen[sh.Index] = true
	}

	content, oerr := f.svc.Open(ctx, f.user1, file.ID, nil)
	if oerr != nil {
		t.Fatalf("Open() after three runs = %v", oerr)
	}
	defer func() { _ = content.Body.Close() }()
	got, rerr := io.ReadAll(content.Body)
	if rerr != nil {
		t.Fatalf("read after three runs = %v", rerr)
	}
	if !bytes.Equal(got, data) {
		t.Error("the file no longer reads back byte-for-byte after three reconstruct runs")
	}
}

// NEVER DESTRUCTIVE.
//
// Reconstruction adds what is missing. A row the database has and the
// manifests do not must survive untouched, and — given the Block 1 finding
// that persisting a damage status nearly hid damaged files — a row whose
// STATUS the database knows and the manifest does not must keep that status.
// A manifest cannot know a file was reconciled as damaged; overwriting it back
// to `ready` would erase a finding and re-offer a download that cannot work.
func TestReconstructNeverOverwritesWhatTheDatabaseAlreadyHas(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := randomBytes(t, 3<<20)
	file := f.uploadAs(t, f.user1, "keeps-its-state.bin", data)

	// The database knows two things the manifest cannot: a rename, and a
	// damaged verdict from reconcile.
	renamed := "renamed-after-upload.bin"
	if _, err := f.svc.Rename(ctx, f.user1, file.ID, &renamed, nil); err != nil {
		t.Fatalf("Rename() = %v", err)
	}
	if err := f.store.RecordReconciledHealth(ctx, f.user1, file.ID,
		files.StatusPartiallyMissing, time.Now()); err != nil {
		t.Fatalf("RecordReconciledHealth() = %v", err)
	}

	report, err := f.svc.Reconstruct(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 0 {
		t.Errorf("recovered %d files against a live database, want 0", report.FilesRecovered)
	}
	if report.FilesAlreadyPresent != 1 {
		t.Errorf("saw %d already present, want 1", report.FilesAlreadyPresent)
	}

	got, gerr := f.svc.Get(ctx, f.user1, file.ID)
	if gerr != nil {
		t.Fatalf("Get() = %v", gerr)
	}
	if got.Name != renamed {
		t.Errorf("name = %q, want %q: reconstruction overwrote a rename the "+
			"manifest could not know about", got.Name, renamed)
	}
	if got.Status != files.StatusPartiallyMissing {
		t.Errorf("status = %q, want %q: reconstruction erased a reconcile "+
			"verdict and re-offered a download that cannot work",
			got.Status, files.StatusPartiallyMissing)
	}
}

// THREE-STATE CLASSIFICATION.
//
// An account that cannot be scanned is INDETERMINATE, never empty, and the run
// says so rather than reporting itself complete. A rate-limited scan claiming
// completeness is how a user concludes their files are gone when they are not.
func TestReconstructTreatsAnUnscannableAccountAsIndeterminate(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := randomBytes(t, 6<<20)
	f.uploadAs(t, f.user1, "throttled.bin", data)
	f.destroyDatabase(t)

	// Every drive now rate-limits, exactly as an exhausted retry does.
	for id := range f.backends {
		f.throttle(id)
	}

	report, err := f.svc.Reconstruct(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}

	if report.Complete {
		t.Error("a fully rate-limited run reported itself COMPLETE; a user would " +
			"conclude there was nothing to recover")
	}
	if len(report.IncompleteReasons) == 0 {
		t.Error("the run is incomplete but says nothing about why")
	}
	for _, scan := range report.Accounts {
		if scan.Scanned {
			t.Errorf("account %s reported as scanned while rate limited", scan.AccountID)
		}
		if scan.Reason == "" {
			t.Errorf("account %s failed to scan but carries no reason", scan.AccountID)
		}
		// THE REASON MUST BE THE RATE LIMIT, not merely non-empty. A wrapper
		// that failed to implement Lister would also produce an indeterminate
		// result — with the reason "this drive does not support listing" — and
		// the test would pass while never executing the rate-limit path at
		// all. That is exactly what happened before this assertion existed:
		// the mutation that should have broken this test did not, because the
		// test never reached the code the mutation changed.
		if !strings.Contains(scan.Reason, "rate limiting") {
			t.Errorf("account %s reason = %q, want the RATE LIMIT reason; "+
				"an indeterminate result for a different cause means this test "+
				"is not exercising throttling", scan.AccountID, scan.Reason)
		}
	}
	// And nothing was invented from a scan that saw nothing.
	if report.FilesRecovered != 0 {
		t.Errorf("recovered %d files from a scan that read nothing", report.FilesRecovered)
	}
}

// OWNERSHIP. Two Skein users on the same Google account see each other's
// manifest objects, because drive.file scope is per-OAuth-client. A user must
// not reconstruct another user's files. Session 4 established this isolation;
// it must not regress here.
func TestReconstructDoesNotRecoverAnotherUsersFiles(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	mine := randomBytes(t, 3<<20)
	theirs := randomBytes(t, 3<<20)
	myFile := f.uploadAs(t, f.user1, "mine.bin", mine)
	theirFile := f.uploadAs(t, f.user2, "theirs.bin", theirs)

	f.destroyDatabase(t)

	report, err := f.svc.Reconstruct(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}

	// Both users' manifests are visible on the shared drives...
	if report.ManifestsFound < 2 {
		t.Fatalf("found %d manifests; this test needs both users' manifests "+
			"visible to mean anything", report.ManifestsFound)
	}
	// ...but only one file is recovered.
	if report.FilesRecovered != 1 {
		t.Errorf("recovered %d files, want 1: user1's run recovered user2's files",
			report.FilesRecovered)
	}

	if _, gerr := f.svc.Get(ctx, f.user1, myFile.ID); gerr != nil {
		t.Errorf("user1's own file was not recovered: %v", gerr)
	}
	if _, gerr := f.svc.Get(ctx, f.user1, theirFile.ID); gerr == nil {
		t.Error("user1 RECOVERED AND CAN READ user2's file")
	}

	listed, lerr := f.svc.List(ctx, f.user1, files.ListParams{Limit: 50})
	if lerr != nil {
		t.Fatalf("List() = %v", lerr)
	}
	for _, l := range listed {
		if l.ID == theirFile.ID {
			t.Error("user2's file appears in user1's listing after reconstruction")
		}
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// REAL DISASTER RECOVERY: a fresh database, the same person, the same drives.
//
// THE SCENARIO THE OWNER ACTUALLY HIT, 2026-08-06. Database moved aside,
// re-registered the same address, reconnected both Drive accounts, ran a
// scan — and recovered NOTHING of 12 files. Registration mints a fresh random
// user id, so every manifest carried an id that no longer existed and the
// ownership filter skipped all of them.
//
// This is the case sidecar manifests exist for. Anything that works only while
// the original database survives is not disaster recovery.
func TestANewDatabaseRecoversTheSameAccountsFiles(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const email = "owner@example.com"
	f.dir.set(f.user1, email)

	// A library worth recovering: striped, and one file in a folder.
	folder, ferr := f.svc.CreateFolder(ctx, f.user1, nil, "Documents")
	if ferr != nil {
		t.Fatalf("CreateFolder() = %v", ferr)
	}
	fid := folder.ID
	original := randomBytes(t, 6<<20)
	if _, uerr := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.user1, Name: "important.bin", Size: int64(len(original)), FolderID: &fid,
	}, bytes.NewReader(original)); uerr != nil {
		t.Fatalf("Upload() = %v", uerr)
	}
	f.uploadAs(t, f.user1, "also-mine.bin", randomBytes(t, 3<<20))

	// ---- The database is lost. ----
	f.destroyDatabase(t)

	// The user re-registers the SAME address and gets a DIFFERENT id, exactly
	// as auth.Register does (uuid.New()). This single line is the whole bug.
	rebuiltUser := uuid.New()
	f.dir.set(rebuiltUser, email)

	report, err := f.svc.Reconstruct(ctx, rebuiltUser, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 2 {
		t.Fatalf("recovered %d of 2 files after a database rebuild.\n"+
			"manifests found: %d, for other users: %d\n"+
			"A user id cannot anchor identity across the loss of the database "+
			"that minted it.", report.FilesRecovered, report.ManifestsFound,
			report.ManifestsForOtherUsers)
	}

	// And the bytes come back, under the NEW user id.
	listed, lerr := f.svc.List(ctx, rebuiltUser, files.ListParams{Limit: 50})
	if lerr != nil {
		t.Fatalf("List() = %v", lerr)
	}
	var target uuid.UUID
	for _, l := range listed {
		if l.Name == "also-mine.bin" {
			target = l.ID
		}
	}
	folders, _ := f.svc.ListFolders(ctx, rebuiltUser)
	var haveDocuments bool
	for _, fl := range folders {
		if fl.Name == "Documents" {
			haveDocuments = true
		}
	}
	if !haveDocuments {
		t.Error("the folder was not recreated under the rebuilt account")
	}

	// The striped file, byte-for-byte.
	for _, l := range listed {
		if l.Name != "important.bin" {
			continue
		}
		content, oerr := f.svc.Open(ctx, rebuiltUser, l.ID, nil)
		if oerr != nil {
			t.Fatalf("Open() = %v", oerr)
		}
		got, rerr := io.ReadAll(content.Body)
		_ = content.Body.Close()
		if rerr != nil {
			t.Fatalf("read = %v", rerr)
		}
		if !bytes.Equal(got, original) {
			t.Error("the recovered file does not match the original byte-for-byte")
		}
	}
	if target == uuid.Nil {
		t.Error("also-mine.bin did not come back")
	}
}

// ISOLATION STILL HOLDS. Claiming by email must not become "anyone can claim
// anything" — a DIFFERENT address recovers nothing, even from the same drives.
//
// This is the counterweight to the test above. Relaxing an ownership check to
// enable recovery is exactly the kind of change that quietly removes isolation,
// so both directions are asserted together.
func TestADifferentAccountStillRecoversNothing(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	f.dir.set(f.user1, "owner@example.com")
	mine := f.uploadAs(t, f.user1, "mine.bin", randomBytes(t, 3<<20))
	f.destroyDatabase(t)

	// A different person on the same shared Google accounts.
	stranger := uuid.New()
	f.dir.set(stranger, "stranger@example.com")

	report, err := f.svc.Reconstruct(ctx, stranger, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.ManifestsFound == 0 {
		t.Fatal("no manifests were visible; this test proves nothing unless the " +
			"stranger can SEE them and is refused anyway")
	}
	if report.FilesRecovered != 0 {
		t.Errorf("a different account recovered %d files", report.FilesRecovered)
	}
	if report.ManifestsForOtherUsers == 0 {
		t.Error("the run did not report skipping manifests belonging to someone else; " +
			"a user recovering nothing cannot tell that from an empty drive")
	}
	if _, gerr := f.svc.Get(ctx, stranger, mine.ID); gerr == nil {
		t.Error("a different account can READ the original owner's file")
	}
}

// Email matching is case-insensitive, because users.email is UNIQUE NOCASE and
// the address someone retypes during a recovery will not always match the
// casing they registered with.
func TestEmailClaimingIsCaseInsensitive(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	f.dir.set(f.user1, "Owner@Example.COM")
	f.uploadAs(t, f.user1, "mixed-case.bin", randomBytes(t, 3<<20))
	f.destroyDatabase(t)

	rebuilt := uuid.New()
	f.dir.set(rebuilt, "owner@example.com")

	report, err := f.svc.Reconstruct(ctx, rebuilt, f.accounts)
	if err != nil {
		t.Fatalf("Reconstruct() = %v", err)
	}
	if report.FilesRecovered != 1 {
		t.Errorf("recovered %d of 1 file; a retyped address in different casing "+
			"must still claim its own manifests", report.FilesRecovered)
	}
}
