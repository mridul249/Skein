package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/mridul249/Skein/internal/accounts"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/skerr"
)

// DISCONNECT MUST REFUSE, NOT DESTROY.
//
// The obvious feature request — "removing a drive should remove its files" — is
// the one thing that must not be built. A file striped across drives A and B is
// destroyed by removing A. Deleting B's shards as a consequence means an action
// worded "unlink an account" permanently destroys data on a drive the user did
// not touch, and no confirmation dialog makes that safe.
//
// So the operation establishes its own precondition and refuses when it cannot
// meet it: if any file has a shard here, say so, name the files, and do
// nothing. Deliberate removal-with-files is a different operation with a
// different confirmation naming the exact files destroyed — known issue #22,
// and not this one.
//
// Today Disconnect soft-deletes unconditionally, so the first two tests below
// fail on the tree as it stands.

// A striped file makes EITHER drive undisconnectable, and that is the point:
// the file depends on both.
func TestDisconnectIsRefusedWhileAFileDependsOnTheDrive(t *testing.T) {
	f := newDisconnectFixture(t)
	ctx := context.Background()

	data := make([]byte, 3<<20) // 3 shards at 1 MiB, round-robin across both
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID, Name: "striped.bin", Size: int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}
	if len(file.Shards) < 2 {
		t.Fatalf("shards = %d; the file is not striped and this test proves nothing",
			len(file.Shards))
	}

	for i, victim := range f.ids {
		linked, _ := f.shardCensus(victim)
		if linked == 0 {
			t.Fatalf("drive %d holds no shards; the fixture is not striping", i)
		}

		derr := f.acctSvc.Disconnect(ctx, f.userID, victim)
		if derr == nil {
			t.Fatalf("drive %d: Disconnect() = nil while %d shard(s) of %q live there. "+
				"Disconnecting leaves those shards unreachable and the file "+
				"undownloadable, with nothing said about it.", i, linked, file.Name)
		}
		if !errors.Is(derr, skerr.ErrConflict) {
			t.Errorf("drive %d: Disconnect() = %v, want a conflict the UI can act on", i, derr)
		}
		// The message has to name the file, or the user cannot act on it.
		if !strings.Contains(derr.Error(), file.Name) {
			t.Errorf("drive %d: refusal %q does not name the file blocking it", i, derr)
		}

		// AND IT MUST HAVE CHANGED NOTHING.
		stored, gerr := f.accounts.GetAccount(ctx, f.userID, victim)
		if gerr != nil {
			t.Fatalf("GetAccount() = %v", gerr)
		}
		if stored.Status != accounts.StatusActive {
			t.Errorf("drive %d: status = %q after a REFUSED disconnect, want %q; "+
				"a refusal that half-applies is worse than one that does not refuse",
				i, stored.Status, accounts.StatusActive)
		}
		if len(stored.AccessTokenEnc) == 0 {
			t.Errorf("drive %d: credentials were cleared by a refused disconnect", i)
		}
	}

	// The file still reads back, which is the property the refusal protects.
	got, derr := f.download(t, file.ID)
	if derr != nil {
		t.Fatalf("download after two refused disconnects = %v", derr)
	}
	if !bytes.Equal(got, data) {
		t.Error("the file did not survive the refused disconnects intact")
	}
}

// AND THE REFUSAL MUST LIFT. A guard that cannot be satisfied is a drive the
// user can never remove.
func TestDisconnectSucceedsOnceTheFilesAreGone(t *testing.T) {
	f := newDisconnectFixture(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID, Name: "striped.bin", Size: int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}

	victim := f.ids[0]
	if derr := f.acctSvc.Disconnect(ctx, f.userID, victim); derr == nil {
		t.Fatal("precondition: the disconnect was not refused")
	}

	// Delete the file for real — shards removed from the drives and rows gone.
	if derr := f.svc.Delete(ctx, f.userID, file.ID); derr != nil {
		t.Fatalf("Delete() = %v", derr)
	}

	if derr := f.acctSvc.Disconnect(ctx, f.userID, victim); derr != nil {
		t.Fatalf("Disconnect() after deleting the only file = %v; "+
			"a guard that cannot be satisfied makes the drive unremovable", derr)
	}

	stored, gerr := f.accounts.GetAccount(ctx, f.userID, victim)
	if gerr != nil {
		t.Fatalf("GetAccount() = %v", gerr)
	}
	if stored.Status != accounts.StatusDisabled {
		t.Errorf("status = %q, want %q", stored.Status, accounts.StatusDisabled)
	}
}

// A DRIVE NOBODY DEPENDS ON DISCONNECTS EXACTLY AS BEFORE. The guard must cost
// nothing in the ordinary case.
func TestDisconnectOfAnUnusedDriveIsUnchanged(t *testing.T) {
	f := newDisconnectFixture(t)
	ctx := context.Background()

	// Nothing uploaded at all.
	for i, victim := range f.ids {
		if derr := f.acctSvc.Disconnect(ctx, f.userID, victim); derr != nil {
			t.Fatalf("drive %d: Disconnect() with no files = %v", i, derr)
		}
	}
}

// A TRASHED FILE STILL DEPENDS ON THE DRIVE. Trash is reversible: its shards
// are still on the drive and restoring it must still work. Treating trashed
// files as absent would let a disconnect quietly destroy a restorable file.
func TestATrashedFileStillBlocksDisconnect(t *testing.T) {
	f := newDisconnectFixture(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID, Name: "trashed.bin", Size: int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}
	if terr := f.svc.Trash(ctx, f.userID, file.ID); terr != nil {
		t.Fatalf("Trash() = %v", terr)
	}

	if derr := f.acctSvc.Disconnect(ctx, f.userID, f.ids[0]); derr == nil {
		t.Error("a trashed file did not block the disconnect; its shards are still " +
			"on the drive and Restore is supposed to bring it back, so removing " +
			"the drive would silently destroy a recoverable file")
	}
}
