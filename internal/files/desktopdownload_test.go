//go:build desktop

package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

func waitForState(t *testing.T, mgr *files.DownloadManager, owner uuid.UUID, id string, want files.DownloadState) files.DesktopDownload {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		dl, ok := mgr.Get(owner, id)
		if ok && dl.State == want {
			return dl
		}
		if ok && dl.State != files.DownloadRunning && dl.State != want {
			t.Fatalf("download reached %q, want %q (error: %s)", dl.State, want, dl.Err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	dl, _ := mgr.Get(owner, id)
	t.Fatalf("timed out waiting for %q; state is %q", want, dl.State)
	return files.DesktopDownload{}
}

// A real download writes the real bytes to disk.
func TestDesktopDownloadWritesTheFile(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "holiday.bin", data)

	dir := t.TempDir()
	dl, err := mgr.Start(ctx, f.user1, file.ID, dir)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}

	done := waitForState(t, mgr, f.user1, dl.ID, files.DownloadComplete)

	got, err := os.ReadFile(done.Path)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("the downloaded file does not match: %d bytes vs %d", len(got), len(data))
	}
	if done.Done != done.Total {
		t.Errorf("Done = %d, Total = %d", done.Done, done.Total)
	}
}

// DIGEST VERIFICATION MUST STAY ON THIS PATH.
//
// Asserted by test rather than by inspection, because the bulk-delete bug came
// from reusing a path without checking what it carried. This goes through
// Service.Open, which compares each shard's stored SHA-256 on a full read — so
// a corrupted shard must fail the download and leave no file.
func TestDesktopDownloadVerifiesShardDigests(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "corrupt.bin", data)

	// Flip a byte in the first shard's stored plaintext.
	sh := file.Shards[0]
	backend := f.backends[*sh.AccountID]
	rc, _, err := backend.Get(ctx, storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	stored, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("read shard: %v", rerr)
	}
	stored[100] ^= 0xff

	if derr := backend.Delete(ctx, storage.ObjectRef{ProviderID: sh.ProviderID}); derr != nil {
		t.Fatalf("Delete() = %v", derr)
	}
	if _, perr := backend.Put(ctx, bytes.NewReader(stored), storage.ObjectSpec{
		Name: sh.ProviderID, Size: int64(len(stored)),
	}); perr != nil {
		t.Fatalf("Put() = %v", perr)
	}

	dir := t.TempDir()
	dl, err := mgr.Start(ctx, f.user1, file.ID, dir)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	failed := waitForState(t, mgr, f.user1, dl.ID, files.DownloadFailed)

	if failed.Err == "" {
		t.Error("a corrupted download reported no error")
	}
	// And no file is left behind claiming to be the download.
	if _, serr := os.Stat(failed.Path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the corrupted download left a file at %s", failed.Path)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files left in the download directory after a failure", len(entries))
	}
}

// CANCEL STOPS THE TRANSFER AND REMOVES THE PARTIAL FILE.
func TestDesktopDownloadCancelRemovesThePartialFile(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 6<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "big.bin", data)

	dir := t.TempDir()
	dl, err := mgr.Start(ctx, f.user1, file.ID, dir)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}

	if cerr := mgr.Cancel(f.user1, dl.ID); cerr != nil {
		t.Fatalf("Cancel() = %v", cerr)
	}
	cancelled := waitForState(t, mgr, f.user1, dl.ID, files.DownloadCancelled)

	// The partial file is gone: a truncated file under the real name looks
	// like a download that worked.
	if _, serr := os.Stat(cancelled.Path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("cancel left a partial file at %s", cancelled.Path)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cancel left %v in the download directory", names)
	}
	// A cancellation is not an error.
	if cancelled.Err != "" {
		t.Errorf("cancel reported an error: %q", cancelled.Err)
	}
}

// A damaged file never starts, so it cannot leave a partial file at all.
func TestDesktopDownloadRefusesADamagedFile(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "damaged.bin", data)

	sh := file.Shards[0]
	backend := f.backends[*sh.AccountID]
	if derr := backend.Delete(ctx, storage.ObjectRef{ProviderID: sh.ProviderID}); derr != nil {
		t.Fatalf("Delete() = %v", derr)
	}

	dir := t.TempDir()
	_, err := mgr.Start(ctx, f.user1, file.ID, dir)
	if err == nil {
		t.Fatal("Start() = nil for a file with a missing shard")
	}
	if !errors.Is(err, skerr.ErrIntegrity) {
		t.Errorf("error = %v, want ErrIntegrity", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused download created %d files", len(entries))
	}
}

// An unwritable or missing directory fails BEFORE the transfer, not midway.
func TestDesktopDownloadValidatesTheTargetDirectoryUpFront(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "x.bin", data)

	t.Run("missing directory", func(t *testing.T) {
		_, err := mgr.Start(ctx, f.user1, file.ID, filepath.Join(t.TempDir(), "nope"))
		if err == nil {
			t.Fatal("Start() = nil for a nonexistent directory")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("error = %v, want it to name the missing folder", err)
		}
	})

	t.Run("unwritable directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permission bits do not apply")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		if _, err := mgr.Start(ctx, f.user1, file.ID, dir); err == nil {
			t.Fatal("Start() = nil for a read-only directory")
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		if _, err := mgr.Start(ctx, f.user1, file.ID, ""); err == nil {
			t.Fatal("Start() = nil with no directory configured")
		}
	})
}

// The save path is a server-side write target and is treated as untrusted.
func TestResolveTargetRejectsTraversal(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"../escape.bin",
		"..",
		"foo/../../bar.bin",
	} {
		t.Run(name, func(t *testing.T) {
			target, err := files.ResolveTarget(dir, name)
			if err != nil {
				return // refusing outright is also correct
			}
			if filepath.Dir(target) != dir {
				t.Errorf("ResolveTarget(%q) = %q, which is outside %q",
					name, target, dir)
			}
		})
	}
}

// Collisions get a (2) suffix rather than overwriting, as browsers do.
func TestResolveTargetSuffixesCollisions(t *testing.T) {
	dir := t.TempDir()

	first, err := files.ResolveTarget(dir, "report.pdf")
	if err != nil {
		t.Fatalf("ResolveTarget() = %v", err)
	}
	if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	second, err := files.ResolveTarget(dir, "report.pdf")
	if err != nil {
		t.Fatalf("ResolveTarget() = %v", err)
	}
	if second == first {
		t.Fatal("the second download would overwrite the first")
	}
	if filepath.Base(second) != "report (2).pdf" {
		t.Errorf("second path = %q, want \"report (2).pdf\"", filepath.Base(second))
	}
}

// PEAK RSS MUST STAY FLAT. MANDATORY AND PERMANENT.
//
// #15's off-heap property used to hold STRUCTURALLY: the bytes never entered
// this process, so no amount of carelessness here could buffer a file. On the
// Go-side path it holds only BY DISCIPLINE — one io.CopyBuffer with a fixed
// buffer, no ReadAll, no bytes.Buffer. This test is the entirety of what
// replaces that structural guarantee.
//
// A file spanning at least three shards, and heap growth bounded well below
// the file size. Do not weaken the bound: an io.ReadAll slipped into the read
// path is exactly what it exists to catch, and that mistake would not be
// visible in any other test.
func TestDesktopDownloadPeakRSSIsFlat(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	// The fixture's shard size is 1 MiB, so 5 MiB is five shards.
	const size = 5 << 20
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "large.bin", data)
	if len(file.Shards) < 3 {
		t.Fatalf("expected at least 3 shards, got %d", len(file.Shards))
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	dir := t.TempDir()
	dl, err := mgr.Start(ctx, f.user1, file.ID, dir)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}

	// Sample while the transfer runs, so a buffered read is caught in the act
	// rather than after the garbage collector has tidied up.
	var peak uint64
	sampling := make(chan struct{})
	go func() {
		defer close(sampling)
		for {
			dlNow, ok := mgr.Get(f.user1, dl.ID)
			if !ok || dlNow.State != files.DownloadRunning {
				return
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	waitForState(t, mgr, f.user1, dl.ID, files.DownloadComplete)
	<-sampling

	growth := int64(peak) - int64(before.HeapAlloc)
	if growth < 0 {
		growth = 0
	}

	// Generous, because the fixture's local backend and the test harness
	// allocate too. The point is that growth is bounded by the BUFFER, not by
	// the file: an io.ReadAll of a 5 MiB file would blow straight past this.
	const budget = int64(2 << 20)
	if growth > budget {
		t.Errorf("heap grew %d bytes downloading a %d byte file (budget %d). "+
			"The download path is buffering rather than streaming — check for "+
			"io.ReadAll or a bytes.Buffer in the read path",
			growth, size, budget)
	}
	t.Logf("heap growth %d bytes for a %d byte file across %d shards",
		growth, size, len(file.Shards))
}

// A CANCELLED DOWNLOAD MUST REPORT "cancelled", NOT "failed".
//
// Found live 2026-08-05, not by test: cancelling a real Drive download
// reported `state: failed` with "check your connection". The provider's HTTP
// stack replaces a cancelled request's error with its own, so
// errors.Is(err, context.Canceled) is FALSE by the time the copy returns — the
// classification has to ask the download's context instead.
//
// This drives it through the real manager against a real (local) backend and
// asserts the user-facing outcome, which is what was wrong.
func TestCancelReportsCancelledNotFailed(t *testing.T) {
	f := newSharedDrive(t)
	mgr := files.NewDownloadManager(f.svc)
	ctx := context.Background()

	data := make([]byte, 8<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "cancelme.bin", data)

	dir := t.TempDir()
	dl, err := mgr.Start(ctx, f.user1, file.ID, dir)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if cerr := mgr.Cancel(f.user1, dl.ID); cerr != nil {
		t.Fatalf("Cancel() = %v", cerr)
	}

	final := waitForState(t, mgr, f.user1, dl.ID, files.DownloadCancelled)

	if final.State != files.DownloadCancelled {
		t.Errorf("state = %q, want cancelled", final.State)
	}
	// The message the user sees is the whole point of this test.
	if final.Err != "" {
		t.Errorf("a cancelled download reported an error message: %q. "+
			"Cancelling is not a failure and must not tell the user to check "+
			"their connection", final.Err)
	}
}
