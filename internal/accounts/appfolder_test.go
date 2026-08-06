package accounts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/storage/gdrive"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
)

// fakeDrive counts folder creates, which is the number the exit criteria are
// actually about: ten concurrent first-uploads must produce one folder, not
// ten.
type fakeDrive struct {
	mu sync.Mutex

	creates  atomic.Int32
	lists    atomic.Int32
	readmes  atomic.Int32
	folderID string

	// listDelay lets a test widen the window between "list found nothing"
	// and "create", which is exactly where the race lives.
	listDelay func()
}

func (f *fakeDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "mimeType"):
		f.lists.Add(1)
		if f.listDelay != nil {
			f.listDelay()
		}
		f.mu.Lock()
		id := f.folderID
		f.mu.Unlock()

		if id == "" {
			_, _ = io.WriteString(w, `{"files":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]string{{"id": id, "createdTime": "2026-01-01T00:00:00Z"}},
		})

	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/"):
		// The README multipart upload.
		f.readmes.Add(1)
		_, _ = io.WriteString(w, `{"id":"readme-1"}`)

	case r.Method == http.MethodPost:
		n := f.creates.Add(1)
		f.mu.Lock()
		if f.folderID == "" {
			f.folderID = "folder-1"
		}
		id := f.folderID
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		_ = n

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// client routes every Drive call at the fake.
func (f *fakeDrive) client(t *testing.T) *http.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &http.Client{Transport: &rewrite{base: srv.URL}}
}

type rewrite struct{ base string }

func (rt *rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target := rt.base + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	u, err := req.URL.Parse(target)
	if err != nil {
		return nil, err
	}
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func newFolderService(t *testing.T) (*Service, conformanceStore, StoredAccount) {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	store := newConformanceStore(t)
	svc := NewService(store, ring, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A sealed envelope rather than nil: access_token_enc is NOT NULL in both
	// real schemas, so an account without one is not a state any backend can
	// hold. Sealed the same way the service does it, keyed on the owner.
	userID := uuid.New()
	accessEnc, err := ring.SealString(skcrypto.InfoToken, userID[:], "access-token")
	if err != nil {
		t.Fatalf("SealString() = %v", err)
	}

	acct, err := store.CreateAccount(context.Background(), NewAccount{
		ID:                uuid.New(),
		UserID:            userID,
		Kind:              "gdrive",
		ProviderAccountID: "sub-1",
		Email:             "drive@example.com",
		AccessTokenEnc:    accessEnc,
		Ordinal:           1,
	})
	if err != nil {
		t.Fatalf("CreateAccount() = %v", err)
	}
	return svc, store, acct
}

// The exit criterion: ten concurrent first-uploads against a fresh account
// produce exactly one folder.
//
// Without the singleflight and the conditional write, all ten find nothing,
// all ten create, and a file's shards end up split across folders nobody can
// tell apart.
func TestConcurrentFirstUploadsCreateOneFolder(t *testing.T) {
	fake := &fakeDrive{}
	// Widen the list -> create window so an unsynchronised implementation
	// would reliably lose rather than passing by luck.
	var gate sync.WaitGroup
	gate.Add(1)
	fake.listDelay = func() { gate.Wait() }

	svc, store, acct := newFolderService(t)
	client := fake.client(t)

	const callers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []string
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := svc.ensureAppFolder(context.Background(), acct, client)
			if err != nil {
				t.Errorf("ensureAppFolder() = %v", err)
				return
			}
			mu.Lock()
			results = append(results, id)
			mu.Unlock()
		}()
	}
	gate.Done()
	wg.Wait()

	if got := fake.creates.Load(); got != 1 {
		t.Fatalf("%d folders were created, want exactly 1", got)
	}
	if len(results) != callers {
		t.Fatalf("%d callers returned a folder, want %d", len(results), callers)
	}
	for _, id := range results {
		if id != results[0] {
			t.Fatalf("callers disagreed on the folder: %q vs %q", id, results[0])
		}
	}

	// And it is persisted, so the next process does not create another.
	stored, err := store.GetAppFolderID(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("GetAppFolderID() = %v", err)
	}
	if stored != results[0] {
		t.Errorf("stored folder = %q, want %q", stored, results[0])
	}

	// The README is written once, with the folder.
	if got := fake.readmes.Load(); got != 1 {
		t.Errorf("README written %d times, want 1", got)
	}
}

// An account that already has a folder must not touch Drive at all.
func TestEnsureAppFolderIsANoOpWhenAlreadySet(t *testing.T) {
	fake := &fakeDrive{}
	svc, store, acct := newFolderService(t)

	if _, err := store.SetAppFolderID(context.Background(), acct.ID, "existing-folder"); err != nil {
		t.Fatalf("SetAppFolderID() = %v", err)
	}
	acct.AppFolderID = "existing-folder"

	id, err := svc.ensureAppFolder(context.Background(), acct, fake.client(t))
	if err != nil {
		t.Fatalf("ensureAppFolder() = %v", err)
	}
	if id != "existing-folder" {
		t.Errorf("folder = %q, want existing-folder", id)
	}
	if fake.lists.Load() != 0 || fake.creates.Load() != 0 {
		t.Errorf("a known folder still called Drive: %d lists, %d creates",
			fake.lists.Load(), fake.creates.Load())
	}
}

// A folder created by another process — so not in this process's store and not
// created here — is adopted rather than duplicated.
func TestEnsureAppFolderAdoptsAnExistingDriveFolder(t *testing.T) {
	fake := &fakeDrive{folderID: "made-elsewhere"}
	svc, store, acct := newFolderService(t)

	id, err := svc.ensureAppFolder(context.Background(), acct, fake.client(t))
	if err != nil {
		t.Fatalf("ensureAppFolder() = %v", err)
	}
	if id != "made-elsewhere" {
		t.Errorf("folder = %q, want the one already on Drive", id)
	}
	if got := fake.creates.Load(); got != 0 {
		t.Errorf("%d folders created despite one already existing", got)
	}
	stored, _ := store.GetAppFolderID(context.Background(), acct.ID)
	if stored != "made-elsewhere" {
		t.Errorf("stored = %q, want made-elsewhere", stored)
	}
	// No README: the folder was not created here, so it already has one.
	if got := fake.readmes.Load(); got != 0 {
		t.Errorf("README written %d times for an adopted folder, want 0", got)
	}
}

// The store write is conditional. If another process persists a folder between
// this one's read and its write, the winner's id is used.
func TestEnsureAppFolderYieldsToTheStoreWinner(t *testing.T) {
	fake := &fakeDrive{}
	svc, store, acct := newFolderService(t)

	// Simulate the other process winning, by pre-setting the column after
	// the caller's in-memory copy was taken.
	if _, err := store.SetAppFolderID(context.Background(), acct.ID, "winner"); err != nil {
		t.Fatalf("SetAppFolderID() = %v", err)
	}
	// acct still carries the empty value the caller loaded earlier.
	acct.AppFolderID = ""

	id, err := svc.ensureAppFolder(context.Background(), acct, fake.client(t))
	if err != nil {
		t.Fatalf("ensureAppFolder() = %v", err)
	}
	if id != "winner" {
		t.Errorf("folder = %q, want the persisted winner", id)
	}
	if fake.creates.Load() != 0 {
		t.Error("a folder was created despite one already being persisted")
	}
}

// ADOPTION AFTER THE PER-USER FOLDER CHANGE (2026-08-05).
//
// Two rules, and the tension between them is the whole design:
//
//   - An EXISTING single-user install must keep working. Its folder is named
//     the bare "Skein", and re-probing must adopt it rather than stranding
//     every shard already inside it.
//   - A SECOND user on the same Google account must NOT inherit the first
//     user's folder, which is what the bare name caused (creates=1).
//
// Resolved by making the bare name adoptable only when no per-user folder
// exists. The first user through adopts "Skein"; the second finds no folder of
// their own AND finds "Skein" already claimed, so they create their own.
func TestExistingBareSkeinFolderIsStillAdopted(t *testing.T) {
	fake := &fakeDrive{folderID: "made-before-the-change"}
	svc, store, acct := newFolderService(t)

	id, err := svc.ensureAppFolder(context.Background(), acct, fake.client(t))
	if err != nil {
		t.Fatalf("ensureAppFolder() = %v", err)
	}
	if id != "made-before-the-change" {
		t.Errorf("folder = %q, want the pre-existing bare Skein folder", id)
	}
	if got := fake.creates.Load(); got != 0 {
		t.Errorf("%d folders created; an existing install must adopt, not migrate", got)
	}
	stored, _ := store.GetAppFolderID(context.Background(), acct.ID)
	if stored != "made-before-the-change" {
		t.Errorf("stored = %q, want the adopted folder", stored)
	}
}

// THE FIX ITSELF. Two Skein users, one Google account, no stored folder for
// either. They must end up in different folders.
func TestSecondUserDoesNotInheritTheFirstUsersFolder(t *testing.T) {
	fake := &perNameDrive{folders: map[string]string{}}
	svc, store, ring := newTestService(t, false)
	client := fake.client(t)

	mkAccount := func(userID uuid.UUID, sub string) StoredAccount {
		enc, err := ring.SealString(skcrypto.InfoToken, userID[:], "access-token")
		if err != nil {
			t.Fatalf("SealString() = %v", err)
		}
		acct, err := store.CreateAccount(context.Background(), NewAccount{
			ID: uuid.New(), UserID: userID, Kind: "gdrive",
			ProviderAccountID: sub, Email: "shared@example.com",
			AccessTokenEnc: enc, Ordinal: 1,
		})
		if err != nil {
			t.Fatalf("CreateAccount() = %v", err)
		}
		return acct
	}

	// Same provider_account_id: the SAME Google account, connected twice.
	// connected_accounts is UNIQUE (user_id, kind, provider_account_id), so
	// two users produce two rows.
	user1, user2 := uuid.New(), uuid.New()
	acct1 := mkAccount(user1, "the-same-google-account")
	acct2 := mkAccount(user2, "the-same-google-account")

	folder1, err := svc.ensureAppFolder(context.Background(), acct1, client)
	if err != nil {
		t.Fatalf("user1 ensureAppFolder() = %v", err)
	}
	folder2, err := svc.ensureAppFolder(context.Background(), acct2, client)
	if err != nil {
		t.Fatalf("user2 ensureAppFolder() = %v", err)
	}

	if folder1 == folder2 {
		t.Fatalf("both users resolved to folder %q; the second user inherited "+
			"the first user's folder and can see their shard objects", folder1)
	}
	if got := fake.creates.Load(); got != 2 {
		t.Errorf("creates = %d, want 2 (one folder per user)", got)
	}

	// And the names are the per-user form, not the bare one.
	for _, name := range fake.created {
		if name == gdrive.AppFolderName {
			t.Errorf("a bare %q folder was created; the name must be per-user",
				gdrive.AppFolderName)
		}
	}
}

// perNameDrive is fakeDrive with per-NAME folder tracking, which is what the
// per-user naming needs: the plain fake has a single folderID and cannot tell
// two folder names apart.
type perNameDrive struct {
	mu      sync.Mutex
	folders map[string]string
	created []string
	creates atomic.Int32
	lists   atomic.Int32
}

func (f *perNameDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "mimeType"):
		f.lists.Add(1)
		name := nameFromQuery(r.URL.Query().Get("q"))
		f.mu.Lock()
		id := f.folders[name]
		f.mu.Unlock()
		if id == "" {
			_, _ = io.WriteString(w, `{"files":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]string{{"id": id, "createdTime": "2026-01-01T00:00:00Z"}},
		})

	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/"):
		_, _ = io.WriteString(w, `{"id":"readme-1"}`)

	case r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := f.creates.Add(1)
		f.mu.Lock()
		id := fmt.Sprintf("folder-%d", n)
		f.folders[body.Name] = id
		f.created = append(f.created, body.Name)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *perNameDrive) client(t *testing.T) *http.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &http.Client{Transport: &rewrite{base: srv.URL}}
}

// nameFromQuery pulls the folder name out of a Drive q= expression of the form
// `name = 'Skein (a1b2c3d4)' and mimeType = ...`.
func nameFromQuery(q string) string {
	const prefix = "name = '"
	i := strings.Index(q, prefix)
	if i < 0 {
		return ""
	}
	rest := q[i+len(prefix):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return strings.ReplaceAll(rest[:j], `\'`, `'`)
}

// THE WRITE SetAppFolderID DELIBERATELY REFUSES.
//
// The single-shot conditional UPDATE is right for first use — it is what makes
// two racing processes converge on one folder — and it is exactly wrong for
// recovery, whose whole job is correcting a folder id that is already set and
// already points at the wrong place. This pins that the two writes differ, in
// both dialects: the conformance harness re-runs this package against SQLite.
func TestRebindAppFolderOverwritesAValueSetAppFolderIDWouldRefuse(t *testing.T) {
	_, store, acct := newFolderService(t)
	ctx := context.Background()

	if _, err := store.SetAppFolderID(ctx, acct.ID, "wrong-folder"); err != nil {
		t.Fatalf("SetAppFolderID() = %v", err)
	}

	// The premise: the single-shot write will not correct this.
	if got, err := store.SetAppFolderID(ctx, acct.ID, "right-folder"); err == nil && got == "right-folder" {
		t.Fatal("SetAppFolderID overwrote an existing folder id; the single-shot " +
			"guarantee is gone and the rebind below is testing nothing")
	}
	if got, err := store.GetAppFolderID(ctx, acct.ID); err != nil || got != "wrong-folder" {
		t.Fatalf("GetAppFolderID() = %q, %v; want the original still in place", got, err)
	}

	if err := store.RebindAppFolderID(ctx, acct.ID, "right-folder"); err != nil {
		t.Fatalf("RebindAppFolderID() = %v", err)
	}
	got, err := store.GetAppFolderID(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAppFolderID() = %v", err)
	}
	if got != "right-folder" {
		t.Errorf("folder = %q after a rebind, want right-folder", got)
	}
}

// The rebind must drop Resolver's cached backend. That backend captured the
// OLD folder id when it was built, so without the invalidation every upload
// for the rest of the process keeps writing to the folder just corrected —
// the cache would silently undo the fix.
func TestRebindAppFolderInvalidatesTheCachedBackend(t *testing.T) {
	svc, _, acct := newFolderService(t)

	var invalidated []uuid.UUID
	svc.OnAccountInvalidated(func(id uuid.UUID) {
		invalidated = append(invalidated, id)
	})

	if err := svc.RebindAppFolder(context.Background(), acct.ID, "right-folder"); err != nil {
		t.Fatalf("RebindAppFolder() = %v", err)
	}
	if len(invalidated) != 1 || invalidated[0] != acct.ID {
		t.Errorf("invalidated %v, want exactly [%s]; a stale backend keeps the old folder id",
			invalidated, acct.ID)
	}
}

// An empty folder id is a bug upstream, not a value to persist. Writing it
// would unbind the account and send the next upload to Drive root.
func TestRebindAppFolderRejectsAnEmptyFolder(t *testing.T) {
	svc, store, acct := newFolderService(t)
	ctx := context.Background()

	if _, err := store.SetAppFolderID(ctx, acct.ID, "good-folder"); err != nil {
		t.Fatalf("SetAppFolderID() = %v", err)
	}
	if err := svc.RebindAppFolder(ctx, acct.ID, ""); err == nil {
		t.Error("RebindAppFolder(\"\") = nil, want an error")
	}
	if got, _ := store.GetAppFolderID(ctx, acct.ID); got != "good-folder" {
		t.Errorf("folder = %q after a rejected rebind, want it untouched", got)
	}
}
