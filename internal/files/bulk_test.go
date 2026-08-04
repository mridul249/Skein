package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
)

func (f *sharedDriveFixture) uploadN(t *testing.T, userID uuid.UUID, n int) []files.File {
	t.Helper()
	out := make([]files.File, 0, n)
	for i := 0; i < n; i++ {
		data := make([]byte, 1<<20)
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("rand: %v", err)
		}
		out = append(out, f.uploadAs(t, userID,
			"file-"+string(rune('a'+i))+".bin", data))
	}
	return out
}

// THE BUG THIS EXISTS TO PREVENT, found 2026-08-05 in review.
//
// BulkDelete originally called Service.Delete, which destroys shards
// permanently. The single-file route has always trashed by default and
// required ?permanent=true to destroy — so a multi-select Delete in the file
// list was silently a permanent erase with no way back.
//
// Bulk delete must land the files in the trash, recoverable, with their shards
// intact on the drives.
func TestBulkDeleteTrashesRatherThanDestroying(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	uploaded := f.uploadN(t, f.user1, 2)
	ids := []uuid.UUID{uploaded[0].ID, uploaded[1].ID}

	results, err := f.svc.BulkDelete(ctx, f.user1, ids)
	if err != nil {
		t.Fatalf("BulkDelete() = %v", err)
	}
	for _, r := range results {
		if !r.OK {
			t.Fatalf("file %s failed: %s", r.FileID, r.Error)
		}
	}

	// Gone from the listing...
	live, _ := f.svc.List(ctx, f.user1, files.ListParams{Limit: 100})
	if len(live) != 0 {
		t.Errorf("%d files still in the listing after a bulk delete", len(live))
	}

	// ...but in the trash, not destroyed.
	trashed, terr := f.svc.ListTrashed(ctx, f.user1, 100)
	if terr != nil {
		t.Fatalf("ListTrashed() = %v", terr)
	}
	if len(trashed) != 2 {
		t.Fatalf("%d files in the trash, want 2; bulk delete destroyed them", len(trashed))
	}

	// And recoverable: restore one and read it back.
	if rerr := f.svc.Restore(ctx, f.user1, ids[0]); rerr != nil {
		t.Fatalf("Restore() = %v; a bulk-deleted file must be recoverable", rerr)
	}
	restored, gerr := f.svc.Get(ctx, f.user1, ids[0])
	if gerr != nil {
		t.Fatalf("the restored file is unreadable: %v", gerr)
	}
	if len(restored.Shards) == 0 {
		t.Error("the restored file has no shards; they were destroyed")
	}
	content, oerr := f.svc.Open(ctx, f.user1, ids[0], nil)
	if oerr != nil {
		t.Fatalf("the restored file cannot be opened: %v", oerr)
	}
	_ = content.Body.Close()
}

// BulkPurge is the permanent one, and it really does destroy.
func TestBulkPurgeDestroysPermanently(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	uploaded := f.uploadN(t, f.user1, 2)
	ids := []uuid.UUID{uploaded[0].ID, uploaded[1].ID}

	if _, err := f.svc.BulkPurge(ctx, f.user1, ids); err != nil {
		t.Fatalf("BulkPurge() = %v", err)
	}

	if trashed, _ := f.svc.ListTrashed(ctx, f.user1, 100); len(trashed) != 0 {
		t.Errorf("%d files in the trash after a purge, want 0", len(trashed))
	}
	if _, err := f.svc.Get(ctx, f.user1, ids[0]); err == nil {
		t.Error("a purged file is still readable")
	}
}

func TestBulkDeleteRemovesEveryFile(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	uploaded := f.uploadN(t, f.user1, 4)
	ids := make([]uuid.UUID, 0, len(uploaded))
	for _, file := range uploaded {
		ids = append(ids, file.ID)
	}

	results, err := f.svc.BulkDelete(ctx, f.user1, ids)
	if err != nil {
		t.Fatalf("BulkDelete() = %v", err)
	}
	if len(results) != len(ids) {
		t.Fatalf("%d results for %d files", len(results), len(ids))
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("file %s failed: %s (%s)", r.FileID, r.Error, r.Code)
		}
	}

	remaining, _ := f.svc.List(ctx, f.user1, files.ListParams{Limit: 100})
	if len(remaining) != 0 {
		t.Errorf("%d files survived a bulk delete", len(remaining))
	}
}

// OWNERSHIP IS CHECKED PER FILE. A bulk request naming another user's file
// must not delete it — the ids come straight from the client, so this is the
// obvious way to turn a bulk endpoint into a cross-user delete.
func TestBulkDeleteEnforcesOwnershipPerFile(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	mine := f.uploadN(t, f.user1, 2)
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	theirs := f.uploadAs(t, f.user2, "user2-file.bin", data)

	// user1 asks to delete their own two AND user2's file.
	ids := []uuid.UUID{mine[0].ID, theirs.ID, mine[1].ID}
	results, err := f.svc.BulkDelete(ctx, f.user1, ids)
	if err != nil {
		t.Fatalf("BulkDelete() = %v", err)
	}

	byID := map[uuid.UUID]files.BulkResult{}
	for _, r := range results {
		byID[r.FileID] = r
	}
	if r := byID[theirs.ID]; r.OK {
		t.Error("bulk delete removed another user's file")
	} else if r.Code != "not_found" {
		t.Errorf("other user's file reported %q, want not_found", r.Code)
	}

	// And it is genuinely still there, for its owner.
	if _, gerr := f.svc.Get(ctx, f.user2, theirs.ID); gerr != nil {
		t.Errorf("user2's file was destroyed by user1's bulk delete: %v", gerr)
	}
	// The caller's own files still went.
	for _, id := range []uuid.UUID{mine[0].ID, mine[1].ID} {
		if !byID[id].OK {
			t.Errorf("caller's own file %s was not deleted", id)
		}
	}
}

// PARTIAL FAILURE IS PER FILE, not one aggregate status. A client has to know
// which rows to drop from the view.
func TestBulkDeleteReportsPerFileResults(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	uploaded := f.uploadN(t, f.user1, 2)
	missing := uuid.New() // never existed

	ids := []uuid.UUID{uploaded[0].ID, missing, uploaded[1].ID}
	results, err := f.svc.BulkDelete(ctx, f.user1, ids)
	if err != nil {
		t.Fatalf("BulkDelete() = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("%d results, want 3", len(results))
	}

	var ok, failed int
	for _, r := range results {
		if r.OK {
			ok++
			continue
		}
		failed++
		if r.Error == "" || r.Code == "" {
			t.Errorf("failed result %s carries no reason", r.FileID)
		}
	}
	if ok != 2 || failed != 1 {
		t.Errorf("ok=%d failed=%d, want 2 and 1", ok, failed)
	}
}

// CARRIED OVER FROM 3c: the user's filename never reaches the provider, and
// bulk delete must not regress that by putting names in results.
func TestBulkDeleteResultsNeverCarryFilenames(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	const secret = "quarterly-layoffs-confidential"
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, secret+".bin", data)

	results, err := f.svc.BulkDelete(ctx, f.user1,
		[]uuid.UUID{file.ID, uuid.New()})
	if err != nil {
		t.Fatalf("BulkDelete() = %v", err)
	}
	for _, r := range results {
		if strings.Contains(r.Error, secret) || strings.Contains(r.Code, secret) {
			t.Errorf("a bulk result leaked the filename: %+v", r)
		}
	}
}

func TestBulkDeleteRejectsEmptyAndOversizedRequests(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	if _, err := f.svc.BulkDelete(ctx, f.user1, nil); err == nil {
		t.Error("BulkDelete(nil) = nil, want a validation error")
	}

	too := make([]uuid.UUID, 500)
	for i := range too {
		too[i] = uuid.New()
	}
	if _, err := f.svc.BulkDelete(ctx, f.user1, too); err == nil {
		t.Error("BulkDelete(500 ids) = nil, want a validation error")
	}
}

// A repeated id must not produce a spurious "not found" for a file this same
// call just deleted.
func TestBulkDeleteDeduplicatesIDs(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	uploaded := f.uploadN(t, f.user1, 1)
	id := uploaded[0].ID

	results, err := f.svc.BulkDelete(ctx, f.user1, []uuid.UUID{id, id, id})
	if err != nil {
		t.Fatalf("BulkDelete() = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("%d results for one repeated id, want 1", len(results))
	}
	if !results[0].OK {
		t.Errorf("the deduplicated delete failed: %s", results[0].Error)
	}
}

func TestEmptyTrashDeletesOnlyTrashedFiles(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	uploaded := f.uploadN(t, f.user1, 3)

	// Trash two, leave one live.
	for _, file := range uploaded[:2] {
		if err := f.svc.Trash(ctx, f.user1, file.ID); err != nil {
			t.Fatalf("Trash() = %v", err)
		}
	}

	results, err := f.svc.EmptyTrash(ctx, f.user1)
	if err != nil {
		t.Fatalf("EmptyTrash() = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("%d results, want 2 trashed files", len(results))
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("trashed file %s not purged: %s", r.FileID, r.Error)
		}
	}

	// The live file is untouched.
	live, _ := f.svc.List(ctx, f.user1, files.ListParams{Limit: 100})
	if len(live) != 1 || live[0].ID != uploaded[2].ID {
		t.Errorf("empty trash affected live files: %v", names(live))
	}
	// And the trash is empty.
	if after, _ := f.svc.ListTrashed(ctx, f.user1, 100); len(after) != 0 {
		t.Errorf("%d files still in the trash", len(after))
	}
}

// Empty trash is scoped to the caller: it must not touch another user's
// trashed files, even though both users' shards share the same drives.
func TestEmptyTrashIsScopedToTheCaller(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	mine := f.uploadAs(t, f.user1, "mine.bin", data)
	theirs := f.uploadAs(t, f.user2, "theirs.bin", data)

	if err := f.svc.Trash(ctx, f.user1, mine.ID); err != nil {
		t.Fatalf("Trash(user1) = %v", err)
	}
	if err := f.svc.Trash(ctx, f.user2, theirs.ID); err != nil {
		t.Fatalf("Trash(user2) = %v", err)
	}

	if _, err := f.svc.EmptyTrash(ctx, f.user1); err != nil {
		t.Fatalf("EmptyTrash() = %v", err)
	}

	// user2's trashed file survives.
	after, err := f.svc.ListTrashed(ctx, f.user2, 100)
	if err != nil {
		t.Fatalf("ListTrashed(user2) = %v", err)
	}
	if len(after) != 1 || after[0].ID != theirs.ID {
		t.Errorf("user1 emptying their trash removed user2's trashed file")
	}
}

func TestEmptyTrashOnAnEmptyTrashIsANoOp(t *testing.T) {
	f := newSharedDrive(t)
	results, err := f.svc.EmptyTrash(context.Background(), f.user1)
	if err != nil {
		t.Fatalf("EmptyTrash() = %v", err)
	}
	if len(results) != 0 {
		t.Errorf("%d results for an empty trash", len(results))
	}
}

var _ = bytes.Equal
