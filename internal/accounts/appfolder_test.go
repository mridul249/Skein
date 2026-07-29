package accounts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul60214/skein/internal/crypto"
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

func newFolderService(t *testing.T) (*Service, *MemoryStore, StoredAccount) {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	store := NewMemoryStore()
	svc := NewService(store, ring, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	acct, err := store.CreateAccount(context.Background(), NewAccount{
		ID:                uuid.New(),
		UserID:            uuid.New(),
		Kind:              "gdrive",
		ProviderAccountID: "sub-1",
		Email:             "drive@example.com",
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
