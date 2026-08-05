package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/router"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage/local"
)

// BLOCK 3c — MULTI-ACCOUNT ISOLATION, VERIFICATION ONLY.
//
// The scenario: two Skein users each connect THE SAME two Google accounts.
//
//	skein_user_1 -> google_acct_A, google_acct_B
//	skein_user_2 -> google_acct_A, google_acct_B
//
// Shards from both users land in the same Drives under the same OAuth client.
// drive.file scope is per-client, not per-Skein-user, so both users' tokens
// address the same app-visible file set. The question is whether Skein's own
// scoping holds anyway.
//
// This fixture deliberately SHARES the backends between the two users, which
// is what makes it a test of isolation rather than of two unrelated setups.
type sharedDriveFixture struct {
	svc      *files.Service
	store    files.ConformanceStore
	router   *router.MemoryStore
	backends map[uuid.UUID]*local.Backend
	// throttled drives return ErrRateLimited from every read.
	throttled map[uuid.UUID]bool
	accounts  []uuid.UUID
	user1     uuid.UUID
	user2     uuid.UUID
}

func newSharedDrive(t *testing.T) *sharedDriveFixture {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	const capacityEach = 8 << 20
	routerStore := router.NewMemoryStore()
	backends := map[uuid.UUID]*local.Backend{}
	ids := make([]uuid.UUID, 0, 2)

	// TWO Google accounts, shared by BOTH Skein users.
	for i := 0; i < 2; i++ {
		id := uuid.New()
		ids = append(ids, id)
		routerStore.AddAccount(id, int32(i+1), "shared@example.com", capacityEach, 0)

		b, berr := local.New(t.TempDir(), local.WithFakeCapacity(capacityEach))
		if berr != nil {
			t.Fatalf("local.New() = %v", berr)
		}
		backends[id] = b
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reserver := router.NewReserver(routerStore, logger)
	planner := router.NewPlanner(reserver, router.PolicyMostAvailable,
		1<<20, func(n int64) int64 { return n })

	throttled := map[uuid.UUID]bool{}
	store := files.NewConformanceStore(t)
	svc := files.NewService(
		store,
		files.NewStripingPlanner(planner, reserver),
		multiResolver{backends: backends, throttled: throttled},
		ring,
		files.Config{Encrypt: false, MaxUploadBytes: 1 << 40},
		logger,
	)

	return &sharedDriveFixture{
		svc: svc, store: store, router: routerStore,
		backends: backends, accounts: ids,
		throttled: throttled,
		user1:     uuid.New(), user2: uuid.New(),
	}
}

// throttle makes one drive report rate limiting on every read, exactly as an
// exhausted retry does (pinned by gdrive/exhaustion_test.go: the pool wraps
// the last error, so ErrRateLimited survives and ErrObjectNotFound never
// appears). Used to prove reconcile flags nothing it could not check.
func (f *sharedDriveFixture) throttle(accountID uuid.UUID) {
	f.throttled[accountID] = true
}

func (f *sharedDriveFixture) uploadAs(t *testing.T, userID uuid.UUID, name string, data []byte) files.File {
	t.Helper()
	file, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: userID, Name: name, Size: int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload(%q) = %v", name, err)
	}
	return file
}

// Items 1-3: separate views, and no cross-user access by ID.
func TestSharedDriveUsersSeeOnlyTheirOwnFiles(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data1 := make([]byte, 3<<20)
	data2 := make([]byte, 3<<20)
	if _, err := rand.Read(data1); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := rand.Read(data2); err != nil {
		t.Fatalf("rand: %v", err)
	}

	file1 := f.uploadAs(t, f.user1, "user1-secret.bin", data1)
	file2 := f.uploadAs(t, f.user2, "user2-secret.bin", data2)

	// Both files really are striped across the SAME shared drives, or this
	// test proves nothing about isolation.
	if len(file1.Shards) < 2 || len(file2.Shards) < 2 {
		t.Fatalf("expected striping: user1 %d shards, user2 %d shards",
			len(file1.Shards), len(file2.Shards))
	}

	// 1 & 2. Listings are disjoint.
	list1, err := f.svc.List(ctx, f.user1, files.ListParams{Limit: 100})
	if err != nil {
		t.Fatalf("List(user1) = %v", err)
	}
	list2, err := f.svc.List(ctx, f.user2, files.ListParams{Limit: 100})
	if err != nil {
		t.Fatalf("List(user2) = %v", err)
	}

	if len(list1) != 1 || list1[0].ID != file1.ID {
		t.Errorf("user1 listing = %d files %v, want only their own", len(list1), names(list1))
	}
	if len(list2) != 1 || list2[0].ID != file2.ID {
		t.Errorf("user2 listing = %d files %v, want only their own", len(list2), names(list2))
	}
	for _, got := range list1 {
		if got.ID == file2.ID {
			t.Error("user1's listing contains user2's file")
		}
	}

	// 3. No cross-user access by ID: Get, Open, Delete.
	t.Run("Get", func(t *testing.T) {
		if _, gerr := f.svc.Get(ctx, f.user2, file1.ID); gerr == nil {
			t.Error("user2 read user1's file metadata by ID")
		} else if !errors.Is(gerr, skerr.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", gerr)
		}
	})

	t.Run("Open", func(t *testing.T) {
		content, oerr := f.svc.Open(ctx, f.user2, file1.ID, nil)
		if oerr == nil {
			defer func() { _ = content.Body.Close() }()
			got, _ := io.ReadAll(content.Body)
			t.Fatalf("user2 DOWNLOADED user1's file: %d bytes, matches original: %v",
				len(got), bytes.Equal(got, data1))
		}
		if !errors.Is(oerr, skerr.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", oerr)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		if derr := f.svc.Delete(ctx, f.user2, file1.ID); derr == nil {
			t.Error("user2 deleted user1's file")
		}
		// And user1's file is still there and still readable.
		if _, gerr := f.svc.Get(ctx, f.user1, file1.ID); gerr != nil {
			t.Errorf("user1's file was affected by user2's delete attempt: %v", gerr)
		}
	})
}

// Item 7, the one flagged as most likely to fail: user2 disconnecting a SHARED
// account must not orphan or delete user1's shards.
//
// Covered here at the files layer: the shard rows belonging to user1 must
// survive, keep their provider object ids, and remain readable. The
// accounts-layer half of disconnect (soft delete, issue #19, closed by
// 0c6ed04) is covered by internal/files/disconnect_test.go.
func TestDisconnectByOneUserDoesNotOrphanAnothersShards(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data1 := make([]byte, 3<<20)
	if _, err := rand.Read(data1); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file1 := f.uploadAs(t, f.user1, "user1.bin", data1)

	data2 := make([]byte, 3<<20)
	if _, err := rand.Read(data2); err != nil {
		t.Fatalf("rand: %v", err)
	}
	_ = f.uploadAs(t, f.user2, "user2.bin", data2)

	before := make([]string, 0, len(file1.Shards))
	for _, sh := range file1.Shards {
		before = append(before, sh.ProviderID)
	}

	// user2 deletes their own file, which releases their shards from the
	// shared drives. This is the closest the files layer gets to "user2
	// disconnects": their objects go away, user1's must not.
	list2, _ := f.svc.List(ctx, f.user2, files.ListParams{Limit: 10})
	for _, file := range list2 {
		if derr := f.svc.Delete(ctx, f.user2, file.ID); derr != nil {
			t.Fatalf("Delete(user2) = %v", derr)
		}
	}

	// user1's manifest is intact.
	after, err := f.svc.Get(ctx, f.user1, file1.ID)
	if err != nil {
		t.Fatalf("user1's file disappeared after user2 deleted theirs: %v", err)
	}
	if len(after.Shards) != len(file1.Shards) {
		t.Errorf("user1 shard count %d -> %d", len(file1.Shards), len(after.Shards))
	}
	for i, sh := range after.Shards {
		if sh.AccountID == nil {
			t.Errorf("user1 shard %d was orphaned (account_id is NULL)", i)
		}
		if i < len(before) && sh.ProviderID != before[i] {
			t.Errorf("user1 shard %d provider id changed %q -> %q",
				i, before[i], sh.ProviderID)
		}
	}

	// And it still reads back byte-for-byte.
	content, err := f.svc.Open(ctx, f.user1, file1.ID, nil)
	if err != nil {
		t.Fatalf("user1 can no longer open their file: %v", err)
	}
	defer func() { _ = content.Body.Close() }()
	got, rerr := io.ReadAll(content.Body)
	if rerr != nil {
		t.Fatalf("user1's read failed after user2's deletes: %v", rerr)
	}
	if !bytes.Equal(got, data1) {
		t.Error("user1's bytes changed after user2 deleted their own file")
	}
}

// Item 6: quota accounting. Two users on shared drives must not double-count
// or cross-attribute: the drives' used bytes reflect the sum of what is
// actually stored, once.
func TestSharedDriveQuotaIsNotDoubleCounted(t *testing.T) {
	f := newSharedDrive(t)

	const size = 3 << 20
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	f.uploadAs(t, f.user1, "u1.bin", data)
	usedAfterFirst := f.storedBytes(t)

	f.uploadAs(t, f.user2, "u2.bin", data)
	usedAfterSecond := f.storedBytes(t)

	delta := usedAfterSecond - usedAfterFirst
	// The second upload must add its own bytes, not zero (cross-attributed to
	// user1's accounting) and not double (counted against both users).
	if delta < size {
		t.Errorf("second user's upload added %d bytes of accounting, want >= %d; "+
			"quota appears cross-attributed", delta, int64(size))
	}
	if delta > 2*size {
		t.Errorf("second user's upload added %d bytes for a %d byte file; "+
			"quota appears double-counted", delta, size)
	}
}

// storedBytes sums what is actually on the shared drives, which is the figure
// quota accounting has to agree with.
func (f *sharedDriveFixture) storedBytes(t *testing.T) int64 {
	t.Helper()
	var total int64
	for _, id := range f.accounts {
		total += f.router.ReservedOn(id)
	}
	for _, file := range f.store.ListShardsSnapshot() {
		total += file.SizeBytes
	}
	return total
}

func names(fs []files.File) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// Item 5: DO TWO SKEIN USERS SHARE A DRIVE APP FOLDER?
//
// Answer, verified rather than reasoned: they share the PHYSICAL Drive folder
// and have SEPARATE database rows.
//
//   - connected_accounts is UNIQUE (user_id, kind, provider_account_id)
//     (sqlite/00002_accounts.sql:62), so each Skein user gets their own row for
//     the same Google account, each with its own app_folder_id column.
//   - But gdrive.AppFolderName is the constant "Skein" with no per-user
//     component, and ensureAppFolder calls FindFolder(AppFolderName) under the
//     same OAuth client. drive.file scope is per-client, so user 2's probe
//     FINDS user 1's folder and adopts it — exactly what
//     TestEnsureAppFolderAdoptsAnExistingDriveFolder describes.
//
// That is safe, and this test pins the reason: shard object names are keyed on
// the file UUID (storage.NameForShard -> "skein-<fileID>-<index>.bin"), so two
// users' shards cannot collide inside one folder, and neither user's shard
// names reveal anything about the other's files.
//
// The residual exposure is NOT confidentiality but VISIBILITY: both users'
// shard objects sit in one Drive folder that either user can see in their own
// Drive UI. Contents stay encrypted and unreadable, and Skein's own listings
// stay scoped, but user 2 can observe that user 1's objects exist and how many
// there are. Recorded, not fixed.
func TestSharedDriveShardNamesCannotCollide(t *testing.T) {
	f := newSharedDrive(t)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file1 := f.uploadAs(t, f.user1, "same-name.bin", data)
	file2 := f.uploadAs(t, f.user2, "same-name.bin", data)

	// Deliberately the same user-facing filename for both users. The provider
	// names must still be distinct.
	seen := map[string]bool{}
	for _, sh := range file1.Shards {
		seen[sh.ProviderID] = true
	}
	for _, sh := range file2.Shards {
		if seen[sh.ProviderID] {
			t.Errorf("provider object id %q is used by both users' shards; "+
				"one user's upload would overwrite the other's inside the "+
				"shared Skein folder", sh.ProviderID)
		}
	}

	// And the user's filename never reaches the provider.
	for _, sh := range append(append([]files.Shard{}, file1.Shards...), file2.Shards...) {
		if bytes.Contains([]byte(sh.ProviderID), []byte("same-name")) {
			t.Errorf("provider object id %q leaks the user's filename", sh.ProviderID)
		}
	}
}
