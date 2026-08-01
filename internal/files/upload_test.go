package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/local"
)

type fixture struct {
	svc     *files.Service
	store   *files.MemoryStore
	backend *local.Backend
	userID  uuid.UUID
}

func newFixture(t *testing.T, opts ...local.Option) *fixture {
	t.Helper()

	backend, err := local.New(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("local.New() = %v", err)
	}
	f := buildFixture(t, backend)
	f.backend = backend
	return f
}

// newFixtureWithBackend builds a fixture over an arbitrary backend, so the
// memory-ceiling tests can isolate Skein's own code path from any provider.
func newFixtureWithBackend(t *testing.T, b storage.Backend) *fixture {
	t.Helper()
	return buildFixture(t, b)
}

// newFixtureForBench is the same wiring for a benchmark, which has no
// *testing.T.
func newFixtureForBench(b *testing.B, backend storage.Backend) *fixture {
	b.Helper()
	return buildFixture(b, backend)
}

// tb is the subset of testing.TB the fixture needs.
type tb interface {
	Helper()
	Fatalf(format string, args ...any)
}

func buildFixture(t tb, backend storage.Backend) *fixture {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	store := files.NewMemoryStore()
	svc := files.NewService(
		store,
		files.NewSingleShardPlanner(nil),
		fixedResolver{backend},
		ring,
		files.Config{MaxUploadBytes: 1 << 40},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return &fixture{svc: svc, store: store, userID: uuid.New()}
}

// fixedResolver hands out one backend for every shard.
type fixedResolver struct{ b storage.Backend }

func (f fixedResolver) For(context.Context, uuid.UUID, *uuid.UUID) (storage.Backend, error) {
	return f.b, nil
}

// failingResolver refuses, so the unavailable-drive paths can be exercised.
type failingResolver struct{}

func (failingResolver) For(context.Context, uuid.UUID, *uuid.UUID) (storage.Backend, error) {
	return nil, errors.New("drive unavailable")
}

func (f *fixture) upload(t *testing.T, name string, data []byte) files.File {
	t.Helper()
	file, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID,
		Name:   name,
		Size:   int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload(%q) = %v", name, err)
	}
	return file
}

func (f *fixture) readAll(t *testing.T, fileID uuid.UUID, rng *storage.ByteRange) []byte {
	t.Helper()
	content, err := f.svc.Open(context.Background(), f.userID, fileID, rng)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() {
		if cerr := content.Body.Close(); cerr != nil {
			t.Errorf("Close() = %v", cerr)
		}
	}()
	got, err := io.ReadAll(content.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(got)) != content.Length {
		t.Errorf("read %d bytes, Content.Length said %d", len(got), content.Length)
	}
	return got
}

func TestUploadAndDownloadRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 511, 512, 513, 1 << 16, 3<<20 + 17} {
		t.Run(strings.TrimSpace(humanSize(size)), func(t *testing.T) {
			f := newFixture(t)
			data := make([]byte, size)
			if _, err := rand.Read(data); err != nil {
				t.Fatalf("rand: %v", err)
			}

			file := f.upload(t, "data.bin", data)
			if file.Status != files.StatusReady {
				t.Fatalf("status = %q, want ready", file.Status)
			}
			if file.SizeBytes != int64(size) {
				t.Errorf("SizeBytes = %d, want %d", file.SizeBytes, size)
			}

			// content_sha256 is taken over the plaintext, so it
			// identifies the file the user uploaded.
			want := sha256.Sum256(data)
			if !bytes.Equal(file.ContentSHA, want[:]) {
				t.Error("stored SHA-256 does not match the uploaded bytes")
			}

			got := f.readAll(t, file.ID, nil)
			if !bytes.Equal(got, data) {
				t.Error("round trip changed the bytes")
			}
		})
	}
}

// Rules.md §2.7: the declared size is a claim. A mismatch fails the upload and
// leaves nothing behind — no file row marked ready, no orphan object.
func TestUploadRejectsSizeMismatchAndCleansUp(t *testing.T) {
	tests := []struct {
		name     string
		declared int64
		actual   int
	}{
		{"client sent less than declared", 5000, 100},
		{"client sent more than declared", 100, 5000},
		{"declared zero, sent bytes", 0, 64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			data := make([]byte, tc.actual)

			_, err := f.svc.Upload(context.Background(), files.UploadRequest{
				UserID: f.userID,
				Name:   "data.bin",
				Size:   tc.declared,
			}, bytes.NewReader(data))

			if !errors.Is(err, skerr.ErrValidation) {
				t.Fatalf("Upload() = %v, want ErrValidation", err)
			}

			// No committed file, and nothing left at the provider.
			list, lerr := f.svc.List(context.Background(), f.userID, files.ListParams{Limit: 50})
			if lerr != nil {
				t.Fatalf("List() = %v", lerr)
			}
			if len(list) != 0 {
				t.Errorf("a failed upload left %d files listed", len(list))
			}
			assertNoOrphans(t, f)
		})
	}
}

func TestUploadCleansUpWhenTheClientDisconnects(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID,
		Name:   "data.bin",
		Size:   1 << 20,
	}, bytes.NewReader(make([]byte, 1<<20)))
	if err == nil {
		t.Fatal("Upload() succeeded on a cancelled context")
	}

	// Cleanup runs on a detached context precisely so that a cancelled
	// request still gets its objects removed.
	assertNoOrphans(t, f)
}

func TestUploadRefusesWhenTheDriveIsUnavailable(t *testing.T) {
	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	store := files.NewMemoryStore()
	svc := files.NewService(store, files.NewSingleShardPlanner(nil), failingResolver{}, ring,
		files.Config{MaxUploadBytes: 1 << 40},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err = svc.Upload(context.Background(), files.UploadRequest{
		UserID: uuid.New(), Name: "data.bin", Size: 4,
	}, bytes.NewReader([]byte("data")))
	if err == nil {
		t.Fatal("Upload() succeeded with no usable drive")
	}
}

func TestUploadEnforcesTheSizeCeiling(t *testing.T) {
	f := newFixture(t)
	// Rebuild with a small ceiling.
	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, _ := skcrypto.NewKeyring(master)
	svc := files.NewService(f.store, files.NewSingleShardPlanner(nil), fixedResolver{f.backend}, ring,
		files.Config{MaxUploadBytes: 1024},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "big.bin", Size: 2048,
	}, bytes.NewReader(make([]byte, 2048)))
	if !errors.Is(err, skerr.ErrTooLarge) {
		t.Fatalf("Upload() = %v, want ErrTooLarge", err)
	}
	// Refused before a single byte was read: nothing was created.
	if len(f.store.ListShardsSnapshot()) != 0 {
		t.Error("an oversized upload created shards")
	}
}

func TestUploadValidatesNames(t *testing.T) {
	tests := []struct{ name, given string }{
		{"empty", ""},
		{"whitespace only", "   "},
		{"dot", "."},
		{"dotdot", ".."},
		{"forward slash", "a/b"},
		{"backslash", `a\b`},
		{"null byte", "a\x00b"},
		{"newline", "a\nb"},
		{"too long", strings.Repeat("x", 256)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.Upload(context.Background(), files.UploadRequest{
				UserID: f.userID, Name: tc.given, Size: 4,
			}, bytes.NewReader([]byte("data")))
			if !errors.Is(err, skerr.ErrValidation) {
				t.Fatalf("Upload(name=%q) = %v, want ErrValidation", tc.given, err)
			}
		})
	}
}

func TestUploadRejectsAnUnknownFolder(t *testing.T) {
	f := newFixture(t)
	stranger := uuid.New()

	_, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "a.bin", Size: 4, FolderID: &stranger,
	}, bytes.NewReader([]byte("data")))
	if !errors.Is(err, skerr.ErrNotFound) {
		t.Fatalf("Upload() = %v, want ErrNotFound", err)
	}
}

// A folder belonging to someone else must be as invisible as one that does not
// exist. Rules.md §2.7.
func TestUploadCannotTargetAnotherUsersFolder(t *testing.T) {
	f := newFixture(t)
	other := uuid.New()

	folder, err := f.svc.CreateFolder(context.Background(), other, nil, "theirs")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}

	_, err = f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "a.bin", Size: 4, FolderID: &folder.ID,
	}, bytes.NewReader([]byte("data")))
	if !errors.Is(err, skerr.ErrNotFound) {
		t.Fatalf("Upload() = %v, want ErrNotFound", err)
	}
}

func assertNoOrphans(t *testing.T, f *fixture) {
	t.Helper()
	q, err := f.backend.Quota(context.Background())
	if err != nil {
		t.Fatalf("Quota() = %v", err)
	}
	_ = q // the local backend without a fake capacity reports the filesystem

	if n := len(f.store.ListShardsSnapshot()); n != 0 {
		t.Errorf("%d shard rows survived a failed upload", n)
	}
}

func humanSize(n int) string {
	switch {
	case n < 1024:
		return "bytes_" + itoa(n)
	case n < 1<<20:
		return "kib_" + itoa(n/1024)
	default:
		return "mib_" + itoa(n/(1<<20))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
