package files_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
)

// KNOWN ISSUE #50: an operation that means "the whole library" must visit every
// file, in every folder, past every page boundary.
//
// `ListFiles` takes a ListParams whose nil FolderID means **the root folder**,
// not "everywhere". Reconcile passed nil and therefore checked root files only
// — measured on the owner's real database, 2 of 20 — while reporting
// `complete: true`. Backfill was about to ship the identical defect.
//
// These tests pin the property for BOTH operations, because the bug is not in
// either of them: it is in reading a folder-scoped listing as a library-wide
// one, and any future whole-library operation can make the same mistake.

// libraryFixture builds a library that spans the root and a nested folder tree,
// returning every file id that must be visited.
//
// NESTING MATTERS. A single flat folder would be caught by a fix that only
// looked one level down; the bug is that the listing is scoped at all.
func buildSpanningLibrary(t *testing.T, f *sharedDriveFixture) map[uuid.UUID]string {
	t.Helper()
	ctx := context.Background()
	want := map[uuid.UUID]string{}

	add := func(name string, folder *uuid.UUID) {
		t.Helper()
		data := randomBytes(t, 1<<20)
		file, err := f.svc.Upload(ctx, files.UploadRequest{
			UserID: f.user1, Name: name, Size: int64(len(data)), FolderID: folder,
		}, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Upload(%q) = %v", name, err)
		}
		want[file.ID] = name
	}

	// Root.
	add("root-a.bin", nil)
	add("root-b.bin", nil)

	// One level down.
	docs, err := f.svc.CreateFolder(ctx, f.user1, nil, "Documents")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	docsID := docs.ID
	add("docs-a.bin", &docsID)
	add("docs-b.bin", &docsID)

	// Two levels down, so a one-level fix does not pass.
	nested, err := f.svc.CreateFolder(ctx, f.user1, &docsID, "Invoices")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	nestedID := nested.ID
	add("invoices-a.bin", &nestedID)

	return want
}

// THE PROPERTY, for reconcile.
//
// Reconcile is the operation that tells a user whether their files are intact.
// A file it never looked at is reported as nothing at all — not as damaged, not
// as unknown — while the run says `complete: true`. That is worse than a
// failure, because it reads as a clean bill of health.
func TestReconcileVisitsEveryFileInEveryFolder(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	want := buildSpanningLibrary(t, f)

	report, err := f.svc.Reconcile(ctx, f.user1)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}

	if report.FilesChecked != len(want) {
		t.Errorf("reconcile checked %d of %d files.\n"+
			"A file it never looked at is invisible to the damaged badge and to "+
			"PurgeDamaged, and the run still reports complete=%v.",
			report.FilesChecked, len(want), report.Complete)
	}
	if !report.Complete {
		t.Errorf("run reported incomplete: %v", report.IncompleteReasons)
	}
}

// THE SAME PROPERTY, for backfill. A file with no manifest is a file that
// cannot be recovered, and coverage counted over root-only files would tell the
// user their drives are a recovery source when they are not.
func TestBackfillVisitsEveryFileInEveryFolder(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	want := buildSpanningLibrary(t, f)
	stripManifests(t, f)

	report, err := f.svc.BackfillManifests(ctx, f.user1, f.accounts)
	if err != nil {
		t.Fatalf("BackfillManifests() = %v", err)
	}

	if report.Coverage.Files != len(want) {
		t.Errorf("backfill saw %d of %d files; coverage over a subset would tell "+
			"the user their drives are a recovery source when they are not",
			report.Coverage.Files, len(want))
	}

	// And a manifest really landed for each, checked at the provider rather
	// than taken from the report.
	for id, name := range want {
		wantName := files.ManifestName(id)
		var found bool
		for _, acct := range f.accounts {
			if _, ok := f.objects(t, acct)[wantName]; ok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no manifest was written for %q (%s); it is unrecoverable", name, id)
		}
	}
}

// PAGINATION. maxBulkFiles caps one listing; a library larger than that cap
// must still be visited in full.
//
// SILENT TRUNCATION IS THE SAME FAILURE SHAPE ONE LEVEL UP: an operation
// reporting itself complete over a subset. A whole-library read that stops at
// a page boundary is exactly as wrong as one that stops at the root folder,
// and it fails at the size where it matters most.
func TestAWholeLibraryReadSpansPageBoundaries(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	// Deliberately more than one page. Small files so the fixture's fake
	// capacity is not the constraint under test.
	const count = files.ListAllPageSize + 7
	want := map[uuid.UUID]bool{}
	for i := 0; i < count; i++ {
		data := randomBytes(t, 4096)
		file, err := f.svc.Upload(ctx, files.UploadRequest{
			UserID: f.user1, Name: fmt.Sprintf("page-%03d.bin", i), Size: int64(len(data)),
		}, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Upload(%d) = %v", i, err)
		}
		want[file.ID] = true
	}

	got, err := f.store.ListAllFiles(ctx, f.user1)
	if err != nil {
		t.Fatalf("ListAllFiles() = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListAllFiles returned %d of %d files; it truncates at a page "+
			"boundary and every caller inherits a silent subset", len(got), len(want))
	}

	seen := map[uuid.UUID]bool{}
	for _, file := range got {
		if seen[file.ID] {
			t.Errorf("file %s returned twice; paging is re-reading a row", file.ID)
		}
		seen[file.ID] = true
		if !want[file.ID] {
			t.Errorf("file %s was returned but never uploaded", file.ID)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("file %s was never returned", id)
		}
	}
}

// ListAllFiles spans folders as well as pages, asserted at the store level so
// the guarantee is on the primitive rather than only on its callers.
func TestListAllFilesSpansFoldersAndExcludesTrash(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	want := buildSpanningLibrary(t, f)

	// A trashed file must NOT appear: "the whole library" means live files.
	trashed := f.uploadAs(t, f.user1, "gone.bin", randomBytes(t, 1<<20))
	if err := f.svc.Trash(ctx, f.user1, trashed.ID); err != nil {
		t.Fatalf("Trash() = %v", err)
	}

	got, err := f.store.ListAllFiles(ctx, f.user1)
	if err != nil {
		t.Fatalf("ListAllFiles() = %v", err)
	}

	seen := map[uuid.UUID]bool{}
	for _, file := range got {
		seen[file.ID] = true
	}
	for id, name := range want {
		if !seen[id] {
			t.Errorf("%q (%s) is missing from ListAllFiles", name, id)
		}
	}
	if seen[trashed.ID] {
		t.Error("a trashed file appeared in ListAllFiles")
	}
	if len(got) != len(want) {
		t.Errorf("ListAllFiles returned %d files, want %d", len(got), len(want))
	}
}

// Ownership holds on the whole-library read, so one user's sweep cannot see
// another's files. Cheap to assert and it is the property multi-user isolation
// rests on.
func TestListAllFilesIsScopedToItsOwner(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	mine := f.uploadAs(t, f.user1, "mine.bin", randomBytes(t, 1<<20))
	theirs := f.uploadAs(t, f.user2, "theirs.bin", randomBytes(t, 1<<20))

	got, err := f.store.ListAllFiles(ctx, f.user1)
	if err != nil {
		t.Fatalf("ListAllFiles() = %v", err)
	}
	var sawMine, sawTheirs bool
	for _, file := range got {
		switch file.ID {
		case mine.ID:
			sawMine = true
		case theirs.ID:
			sawTheirs = true
		}
	}
	if !sawMine {
		t.Error("the caller's own file is missing")
	}
	if sawTheirs {
		t.Error("ListAllFiles returned another user's file")
	}
}
