package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/accounts"
	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/router"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/local"
)

// Issue #19, now fixed and guarded here. Disconnecting a drive used to delete
// its connected_accounts row; file_shards.connected_account_id is ON DELETE SET
// NULL (00004_files.sql:64), so every shard on that drive lost its link.
// Reconnecting inserted a row with a new uuid and re-linked nothing, leaving the
// files unreadable permanently.
//
// Nothing is deleted at the provider — verified separately — so this was
// inaccessibility, not loss, and every re-link key survived on the shard row.
// That is what made a fix possible after the fact.
//
// The fix is a soft delete: Service.Disconnect sets status 'disabled' and
// clears the credentials instead of deleting, so the row id stays stable and
// the shard link is never nulled. These tests assert the whole round trip —
// disconnect, stay unreadable while disconnected, reconnect, read again.

// fkFilesStore models the one thing an in-memory double cannot otherwise show:
// ON DELETE SET NULL. The nulling is a database action, so a memstore test would
// pass trivially without it.
//
// It is driven by observed state rather than assumed: applyAccountDeletion is
// called only when the account row has actually gone from the accounts store, so
// a fix that stops deleting the row stops triggering it. That is what makes the
// inverted mutation check meaningful instead of circular.
type fkFilesStore struct {
	*files.MemoryStore
}

// applyAccountDeletion nulls connected_account_id on every shard pointing at a
// now-deleted account, exactly as the foreign key does in Postgres.
func (s fkFilesStore) applyAccountDeletion(t *testing.T, gone uuid.UUID) int {
	t.Helper()
	var n int
	for _, sh := range s.ListShardsSnapshot() {
		if sh.AccountID != nil && *sh.AccountID == gone {
			s.CorruptShard(sh.FileID, sh.Index, func(target *files.Shard) {
				target.AccountID = nil
			})
			n++
		}
	}
	return n
}

// acctResolver resolves a shard's backend the way production does, and no
// better. Resolver.For (resolver.go:40-75) calls Service.Backend, which is
// GetAccount followed by backendFor — so this mirrors both steps, including
// backendFor's disabled-status check.
//
// That status check is load-bearing rather than decorative. Disconnect no
// longer deletes the row (known issue #19), so "the account row exists" stopped
// implying "the drive is reachable", and refusing a disabled account is the
// only thing left that makes a disconnected drive fail closed. Dropping this
// branch here would let the test pass while production served files from a
// drive the user had disconnected.
type acctResolver struct {
	store    *accounts.MemoryStore
	backends map[uuid.UUID]*local.Backend
	userID   uuid.UUID
}

func (r acctResolver) For(ctx context.Context, userID uuid.UUID, accountID *uuid.UUID) (storage.Backend, error) {
	if accountID == nil {
		// resolver.go:43, with no fallback wired — which is how main.go:142
		// constructs it.
		return nil, skerr.Public(skerr.ErrUnavailable,
			"No drive is connected. Connect one in Settings.")
	}
	acct, err := r.store.GetAccount(ctx, userID, *accountID)
	if err != nil {
		return nil, err
	}
	// service.go backendFor: a disconnected drive keeps its row but must not
	// resolve to a usable backend.
	if acct.Status == accounts.StatusDisabled {
		return nil, skerr.Public(skerr.ErrUnavailable,
			"That drive is disconnected. Reconnect it in Settings to reach these files.")
	}
	b, ok := r.backends[*accountID]
	if !ok {
		return nil, errors.New("no backend for that account")
	}
	return b, nil
}

type disconnectFixture struct {
	svc      *files.Service
	files    fkFilesStore
	accounts *accounts.MemoryStore
	acctSvc  *accounts.Service
	backends map[uuid.UUID]*local.Backend
	ids      []uuid.UUID
	subs     []string
	userID   uuid.UUID
	ring     *skcrypto.Keyring
}

// sealedToken builds the access-token envelope every account row must carry:
// access_token_enc is NOT NULL in both real schemas, so nil is not a state any
// backend can hold.
func sealedToken(t *testing.T, ring *skcrypto.Keyring, userID uuid.UUID) []byte {
	t.Helper()
	enc, err := ring.SealString(skcrypto.InfoToken, userID[:], "access-token")
	if err != nil {
		t.Fatalf("SealString() = %v", err)
	}
	return enc
}

func newDisconnectFixture(t *testing.T) *disconnectFixture {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	userID := uuid.New()

	acctStore := accounts.NewMemoryStore()
	acctSvc := accounts.NewService(acctStore, ring, nil, logger)

	routerStore := router.NewMemoryStore()
	backends := map[uuid.UUID]*local.Backend{}
	ids := make([]uuid.UUID, 0, 2)
	subs := []string{"google-sub-one", "google-sub-two"}

	for i := 0; i < 2; i++ {
		stored, cerr := acctStore.CreateAccount(context.Background(), accounts.NewAccount{
			ID:                uuid.New(),
			UserID:            userID,
			Kind:              storage.KindGoogleDrive,
			ProviderAccountID: subs[i],
			Email:             subs[i] + "@example.com",
			AccessTokenEnc:    sealedToken(t, ring, userID),
			Ordinal:           int32(i + 1),
		})
		if cerr != nil {
			t.Fatalf("CreateAccount() = %v", cerr)
		}
		ids = append(ids, stored.ID)
		routerStore.AddAccount(stored.ID, int32(i+1), stored.Email, 32<<20, 0)

		b, berr := local.New(t.TempDir(), local.WithFakeCapacity(32<<20))
		if berr != nil {
			t.Fatalf("local.New() = %v", berr)
		}
		backends[stored.ID] = b
	}

	reserver := router.NewReserver(routerStore, logger)
	planner := router.NewPlanner(reserver, router.PolicyRoundRobin, 1<<20, skcrypto.StreamOverhead)

	fileStore := fkFilesStore{files.NewMemoryStore()}
	svc := files.NewService(
		fileStore,
		files.NewStripingPlanner(planner, reserver),
		acctResolver{store: acctStore, backends: backends, userID: userID},
		ring,
		files.Config{Encrypt: true, MaxUploadBytes: 1 << 40},
		logger,
	)

	return &disconnectFixture{
		svc: svc, files: fileStore, accounts: acctStore, acctSvc: acctSvc,
		backends: backends, ids: ids, subs: subs, userID: userID, ring: ring,
	}
}

func (f *disconnectFixture) download(t *testing.T, fileID uuid.UUID) ([]byte, error) {
	t.Helper()
	content, err := f.svc.Open(context.Background(), f.userID, fileID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = content.Body.Close() }()
	return io.ReadAll(content.Body)
}

// shardsOn counts shards still linked to an account, and how many are orphaned.
func (f *disconnectFixture) shardCensus(acct uuid.UUID) (linked, orphaned int) {
	for _, sh := range f.files.ListShardsSnapshot() {
		switch {
		case sh.AccountID == nil:
			orphaned++
		case *sh.AccountID == acct:
			linked++
		}
	}
	return linked, orphaned
}

func TestDisconnectThenReconnectRestoresAccess(t *testing.T) {
	f := newDisconnectFixture(t)
	ctx := context.Background()

	// 1. Upload a striped file across both drives and confirm it reads back.
	const size = 3 << 20 // 3 MiB at 1 MiB shards -> 3 shards, alternating
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(data)

	file, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID, Name: "striped.bin", Size: size,
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}
	if len(file.Shards) < 2 {
		t.Fatalf("shards = %d, want the file striped across both drives", len(file.Shards))
	}

	got, err := f.download(t, file.ID)
	if err != nil {
		t.Fatalf("download before disconnect = %v", err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("checksum mismatch before any disconnect")
	}

	victim := f.ids[0]
	linkedBefore, _ := f.shardCensus(victim)
	if linkedBefore == 0 {
		t.Fatalf("no shards landed on drive %s; the test proves nothing", victim)
	}

	// 2. Disconnect through the real service method, not by touching rows.
	if derr := f.acctSvc.Disconnect(ctx, f.userID, victim); derr != nil {
		t.Fatalf("Disconnect() = %v", derr)
	}

	// The database's ON DELETE SET NULL fires only if the row actually went.
	// A soft delete leaves it in place and this does nothing.
	if _, gerr := f.accounts.GetAccount(ctx, f.userID, victim); errors.Is(gerr, skerr.ErrNotFound) {
		nulled := f.files.applyAccountDeletion(t, victim)
		t.Logf("account row was DELETED; ON DELETE SET NULL orphaned %d shard(s)", nulled)
	} else {
		t.Logf("account row survived disconnect (soft delete); no shards orphaned")
	}

	// 3. Shards must still know which drive holds them. This is the defect:
	//    today they do not, and the link is unrecoverable from the database.
	linkedAfter, orphaned := f.shardCensus(victim)
	if orphaned > 0 {
		t.Errorf("%d shard(s) lost their drive link on disconnect; "+
			"connected_account_id is NULL and nothing records which drive held them",
			orphaned)
	}
	if linkedAfter != linkedBefore {
		t.Errorf("shards linked to drive %s went from %d to %d across a disconnect",
			victim, linkedBefore, linkedAfter)
	}

	// 4. While disconnected the download must fail, and say something useful.
	//    This is correct behaviour and has to stay true after the fix.
	if _, derr := f.download(t, file.ID); derr == nil {
		t.Error("the file still downloaded while one of its drives was disconnected")
	} else if !errors.Is(derr, skerr.ErrUnavailable) && !errors.Is(derr, skerr.ErrNotFound) {
		t.Errorf("download while disconnected = %v, want a clear unavailable/not-found error", derr)
	}

	// 5. Provider object ids must survive, or nothing can be re-linked even by
	//    hand. This is what makes #19 inaccessibility rather than loss.
	for _, sh := range f.files.ListShardsSnapshot() {
		if sh.ProviderID == "" {
			t.Errorf("shard %d lost its provider object id; the data is unrecoverable", sh.Index)
		}
	}

	// 6. Reconnect the same provider identity. THE HEADLINE ASSERTION: access
	//    must come back. Today linkGoogleAccount cannot find the deleted row, so
	//    it inserts a new one with a new uuid and re-links nothing.
	reconnected, rerr := f.accounts.CreateAccount(ctx, accounts.NewAccount{
		ID:                uuid.New(),
		UserID:            f.userID,
		Kind:              storage.KindGoogleDrive,
		ProviderAccountID: f.subs[0], // same Google identity
		Email:             f.subs[0] + "@example.com",
		AccessTokenEnc:    sealedToken(t, f.ring, f.userID),
		Ordinal:           1,
	})
	if rerr != nil {
		// A surviving row makes this a duplicate, which is the constraint doing
		// its job: the fix reuses the row rather than inserting.
		existing, gerr := f.accounts.GetAccountByProviderID(ctx, f.userID,
			storage.KindGoogleDrive, f.subs[0])
		if gerr != nil {
			t.Fatalf("reconnect: CreateAccount = %v, and no existing row either: %v", rerr, gerr)
		}
		if _, uerr := f.accounts.UpdateAccountTokens(ctx, accounts.TokenUpdate{
			ID: existing.ID, UserID: f.userID,
			Email: existing.Email, DisplayName: existing.DisplayName,
		}); uerr != nil {
			t.Fatalf("reconnect: UpdateAccountTokens = %v", uerr)
		}
		reconnected = existing
	} else {
		// A new row means a new id, so the backend map has to learn it or the
		// test would fail for a bookkeeping reason rather than the real one.
		f.backends[reconnected.ID] = f.backends[victim]
		t.Logf("reconnect created a NEW account row: %s (was %s)",
			reconnected.ID, victim)
	}

	got, err = f.download(t, file.ID)
	if err != nil {
		t.Errorf("download after reconnecting the same Google identity = %v; "+
			"reconnecting does not restore access", err)
		return
	}
	if sha256.Sum256(got) != want {
		t.Error("checksum mismatch after reconnect")
	}
}
