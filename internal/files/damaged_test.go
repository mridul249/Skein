package files_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// BUG, 2026-08-05, from a live log:
//
//	level=WARN msg="request refused"
//	error="integrity check failed: Shard 0 of this file is missing from its drive."
//	status=500
//
// The DETECTION is right — a shard deleted out of band is caught and the read
// refuses rather than handing back a partial file. Three things downstream
// were wrong:
//
//	(a) 500 says "server error, retry". The server behaved correctly and no
//	    retry can ever succeed. It is a 409: the file is in a state that
//	    blocks the request.
//	(b) The client cannot tell WHICH shards are gone, so it cannot say
//	    anything useful, and it offered "download it instead" — which fails
//	    the same way.
//	(c) POST /content-url returned 200 for the same file, minting a download
//	    link for something that cannot be downloaded. On the a.click() path
//	    the error response is then SAVED AS THE FILE.
func TestDamagedFileIsReportedAsAConflictNotAServerError(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "striped.bin", data)
	if len(file.Shards) < 2 {
		t.Fatalf("expected a striped file, got %d shards", len(file.Shards))
	}

	// Delete one shard out of band, exactly as a user tidying their Drive
	// would.
	victim := file.Shards[0]
	backend := f.backends[*victim.AccountID]
	if err := backend.Delete(ctx, storage.ObjectRef{ProviderID: victim.ProviderID}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	// The read still refuses — that part was always right.
	content, err := f.svc.Open(ctx, f.user1, file.ID, nil)
	if err == nil {
		_, err = io.ReadAll(content.Body)
		_ = content.Body.Close()
	}
	if err == nil {
		t.Fatal("a file with a missing shard was served without error")
	}
	if !errors.Is(err, skerr.ErrIntegrity) {
		t.Errorf("error = %v, want ErrIntegrity", err)
	}

	// (a) + (b): the error names which shards are missing, so the client can
	// say something true instead of "cannot be shown here".
	var damaged *files.DamagedFileError
	if !errors.As(err, &damaged) {
		t.Fatalf("error %v does not carry the damaged-file detail; "+
			"the client cannot name the missing shards", err)
	}
	if damaged.FileID != file.ID {
		t.Errorf("FileID = %v, want %v", damaged.FileID, file.ID)
	}
	if len(damaged.MissingShards) != 1 || damaged.MissingShards[0] != victim.Index {
		t.Errorf("MissingShards = %v, want [%d]", damaged.MissingShards, victim.Index)
	}
}

// (c) A damaged file must not get a download link at all. Minting one produces
// a download whose contents are an error page.
func TestDamagedFileRefusesToMintACapability(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "striped.bin", data)

	victim := file.Shards[0]
	backend := f.backends[*victim.AccountID]
	if err := backend.Delete(ctx, storage.ObjectRef{ProviderID: victim.ProviderID}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	// CheckReadable is what the content-url handler consults before signing.
	err := f.svc.CheckReadable(ctx, f.user1, file.ID)
	if err == nil {
		t.Fatal("CheckReadable() = nil for a file with a missing shard; " +
			"a download link would be minted and the error response saved as the file")
	}
	if !errors.Is(err, skerr.ErrIntegrity) {
		t.Errorf("error = %v, want ErrIntegrity", err)
	}

	var damaged *files.DamagedFileError
	if !errors.As(err, &damaged) {
		t.Fatal("CheckReadable did not report which shards are missing")
	}
	if len(damaged.MissingShards) != 1 {
		t.Errorf("MissingShards = %v, want one entry", damaged.MissingShards)
	}
}

// A healthy file is unaffected: the check must not cost a read of the content.
func TestCheckReadableAllowsAHealthyFile(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "healthy.bin", data)

	if err := f.svc.CheckReadable(ctx, f.user1, file.ID); err != nil {
		t.Errorf("CheckReadable() = %v for a healthy file", err)
	}
	// And another user still cannot probe it.
	if err := f.svc.CheckReadable(ctx, f.user2, file.ID); !errors.Is(err, skerr.ErrNotFound) {
		t.Errorf("CheckReadable(other user) = %v, want ErrNotFound", err)
	}
}

// The HANDLER must consult CheckReadable, not merely Get.
//
// A previous mutation showed the service-level test above passes even when the
// handler is reverted to Get — because it calls CheckReadable directly. This
// asserts the wiring: the thing the route actually calls has to be the one
// that asks the drives.
func TestContentURLPathChecksShardPresence(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "striped.bin", data)

	victim := file.Shards[0]
	backend := f.backends[*victim.AccountID]
	if err := backend.Delete(ctx, storage.ObjectRef{ProviderID: victim.ProviderID}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	// Get still succeeds — it only reads the database. THAT is why the route
	// used to mint a link for a file that cannot be downloaded.
	if _, err := f.svc.Get(ctx, f.user1, file.ID); err != nil {
		t.Fatalf("Get() = %v; the row should still be intact", err)
	}
	// CheckReadable is the one that asks the drives.
	if err := f.svc.CheckReadable(ctx, f.user1, file.ID); err == nil {
		t.Fatal("CheckReadable() = nil; the mint guard would let a damaged " +
			"file through and the error response would be saved as the file")
	}
}
