package files_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
)

// FilesOnAccount AT THE STORE LEVEL, ACROSS BOTH BACKENDS.
//
// The disconnect-refusal tests run against an in-memory double, so running them
// under SKEIN_FILES_TEST_BACKEND=sqlite proves nothing about the SQLite store —
// newDisconnectFixture builds NewMemoryStore directly. This is what actually
// exercises the query the desktop build runs, via NewConformanceStore, which
// does honour the switch.
//
// The query backs a REFUSAL that guards against data loss, so being wrong in
// one dialect and right in the other is exactly the failure worth catching.
func TestFilesOnAccountCountsWhatBlocksADisconnect(t *testing.T) {
	store := files.NewConformanceStore(t)
	ctx := context.Background()

	userID := uuid.New()
	other := uuid.New()
	driveA, driveB := uuid.New(), uuid.New()

	mk := func(owner uuid.UUID, name string, accounts ...uuid.UUID) uuid.UUID {
		t.Helper()
		f, err := store.CreateFile(ctx, files.NewFile{
			ID: uuid.New(), UserID: owner, Name: name, SizeBytes: 10, IsStriped: len(accounts) > 1,
		})
		if err != nil {
			t.Fatalf("CreateFile(%q) = %v", name, err)
		}
		for i, acct := range accounts {
			a := acct
			if _, err := store.InsertReconstructedShard(ctx, files.NewShard{
				ID: uuid.New(), FileID: f.ID, Index: int32(i), AccountID: &a,
				ProviderID: name + "-obj", SizeBytes: 5, PlainSize: 5,
			}); err != nil {
				t.Fatalf("InsertReconstructedShard(%q) = %v", name, err)
			}
		}
		return f.ID
	}

	// Striped across both drives: blocks either.
	mk(userID, "striped.bin", driveA, driveB)
	// Only on A.
	mk(userID, "only-a.bin", driveA)
	// Another user's file on A must NOT count: it is not this user's to be
	// warned about, and naming it would leak a filename across accounts.
	mk(other, "not-yours.bin", driveA)

	names, total, err := store.FilesOnAccount(ctx, userID, driveA, 10)
	if err != nil {
		t.Fatalf("FilesOnAccount(driveA) = %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (striped.bin and only-a.bin)", total)
	}
	for _, n := range names {
		if n == "not-yours.bin" {
			t.Error("another user's file was named as blocking this user's disconnect")
		}
	}

	// Drive B holds only the striped file.
	_, totalB, err := store.FilesOnAccount(ctx, userID, driveB, 10)
	if err != nil {
		t.Fatalf("FilesOnAccount(driveB) = %v", err)
	}
	if totalB != 1 {
		t.Errorf("driveB total = %d, want 1; a striped file must block BOTH drives", totalB)
	}

	// A drive nobody uses blocks nothing.
	_, totalC, err := store.FilesOnAccount(ctx, userID, uuid.New(), 10)
	if err != nil {
		t.Fatalf("FilesOnAccount(unused) = %v", err)
	}
	if totalC != 0 {
		t.Errorf("unused drive total = %d, want 0", totalC)
	}
}

// limit bounds the NAMES, never the count. A refusal that said "3 files" while
// listing 3 of 900 would understate the problem.
func TestFilesOnAccountLimitsNamesNotTheTotal(t *testing.T) {
	store := files.NewConformanceStore(t)
	ctx := context.Background()

	userID := uuid.New()
	drive := uuid.New()
	for i := 0; i < 7; i++ {
		f, err := store.CreateFile(ctx, files.NewFile{
			ID: uuid.New(), UserID: userID, Name: string(rune('a'+i)) + ".bin", SizeBytes: 10,
		})
		if err != nil {
			t.Fatalf("CreateFile = %v", err)
		}
		d := drive
		if _, err := store.InsertReconstructedShard(ctx, files.NewShard{
			ID: uuid.New(), FileID: f.ID, Index: 0, AccountID: &d,
			ProviderID: "obj", SizeBytes: 5, PlainSize: 5,
		}); err != nil {
			t.Fatalf("InsertReconstructedShard = %v", err)
		}
	}

	names, total, err := store.FilesOnAccount(ctx, userID, drive, 3)
	if err != nil {
		t.Fatalf("FilesOnAccount = %v", err)
	}
	if total != 7 {
		t.Errorf("total = %d, want 7; the limit must not reduce the count", total)
	}
	if len(names) != 3 {
		t.Errorf("names = %d, want 3", len(names))
	}
}
