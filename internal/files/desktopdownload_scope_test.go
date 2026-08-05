//go:build desktop

package files_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/skerr"
)

// A DOWNLOAD BELONGS TO THE USER WHO STARTED IT.
//
// Cancel, Get, List and Subscribe originally took no userID at all: Start
// received one, checked it against the file, and then discarded it. Every
// subsequent operation keyed on the download id alone, and those ids are
// sequential ("dl-1", "dl-2", ...), so they are guessed rather than
// discovered. Registration is open, so this was reachable by anyone who could
// reach the port.
//
// The leak was not only informational. A second user could read another user's
// filename and absolute on-disk path, and could CANCEL their transfer.
//
// ErrNotFound, never a 403: a 403 confirms the transfer exists, which is the
// fact being protected. This mirrors what 3c established for files
// (multiuser_test.go) — Get, Open and Delete all answer ErrNotFound across
// users rather than admitting the row is there.
func TestDesktopDownloadsAreScopedToTheirOwner(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 512<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "user1-private-name.bin", data)

	dl, err := mgr.Start(ctx, f.user1, file.ID, t.TempDir())
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	// Let it settle so the assertions are not racing the transfer.
	waitForState(t, mgr, f.user1, dl.ID, files.DownloadComplete)

	t.Run("Get refuses across users", func(t *testing.T) {
		if got, ok := mgr.Get(f.user2, dl.ID); ok {
			t.Fatalf("user2 read user1's download: name=%q path=%q", got.Name, got.Path)
		}
	})

	t.Run("Get still works for the owner", func(t *testing.T) {
		if _, ok := mgr.Get(f.user1, dl.ID); !ok {
			t.Fatal("the owner cannot see their own download")
		}
	})

	t.Run("List refuses across users", func(t *testing.T) {
		if got := mgr.List(f.user2); len(got) != 0 {
			t.Fatalf("user2 listed %d of user1's downloads; first is name=%q path=%q",
				len(got), got[0].Name, got[0].Path)
		}
	})

	t.Run("List still returns the owner's own", func(t *testing.T) {
		if got := mgr.List(f.user1); len(got) != 1 {
			t.Fatalf("the owner listed %d downloads, want 1", len(got))
		}
	})

	t.Run("Subscribe refuses across users", func(t *testing.T) {
		ch, unsub, ok := mgr.Subscribe(f.user2, dl.ID)
		if ok {
			if unsub != nil {
				unsub()
			}
			t.Fatal("user2 subscribed to user1's download; SSE is a long-lived " +
				"connection, so this is a continuous feed of another user's activity")
		}
		if ch != nil {
			t.Error("Subscribe returned a channel alongside ok=false")
		}
	})

	t.Run("Subscribe still works for the owner", func(t *testing.T) {
		ch, unsub, ok := mgr.Subscribe(f.user1, dl.ID)
		if !ok {
			t.Fatal("the owner cannot subscribe to their own download")
		}
		defer unsub()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("the owner's subscription delivered no current snapshot")
		}
	})

	t.Run("Cancel refuses across users", func(t *testing.T) {
		err := mgr.Cancel(f.user2, dl.ID)
		if !errors.Is(err, skerr.ErrNotFound) {
			t.Fatalf("user2 cancelling user1's download = %v, want ErrNotFound", err)
		}
	})
}

// Cancelling across users must not stop the transfer. The test above asserts
// the ERROR; this asserts the EFFECT, because an implementation that returned
// ErrNotFound after already calling entry.cancel() would satisfy the first and
// still hand a stranger a kill switch.
func TestACrossUserCancelDoesNotStopTheTransfer(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "big.bin", data)

	dl, err := mgr.Start(ctx, f.user1, file.ID, t.TempDir())
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}

	// Fire the hostile cancel immediately, while the transfer is in flight.
	if cerr := mgr.Cancel(f.user2, dl.ID); !errors.Is(cerr, skerr.ErrNotFound) {
		t.Fatalf("cross-user Cancel() = %v, want ErrNotFound", cerr)
	}

	// It must still finish.
	done := waitForState(t, mgr, f.user1, dl.ID, files.DownloadComplete)
	if done.Done != done.Total {
		t.Fatalf("transfer ended at %d/%d bytes; a stranger's cancel interrupted it",
			done.Done, done.Total)
	}
}
