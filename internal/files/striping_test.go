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

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/router"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/local"
)

// stripedFixture wires the real striping planner, the real reservation
// scheme and the real encryption over several small local backends, so the
// Phase 5 exit criteria are exercised end to end with no network.
type stripedFixture struct {
	svc      *files.Service
	store    *files.MemoryStore
	router   *router.MemoryStore
	backends map[uuid.UUID]*local.Backend
	accounts []uuid.UUID
	userID   uuid.UUID
}

// multiResolver routes each shard to the backend for its account.
type multiResolver struct {
	backends map[uuid.UUID]*local.Backend
}

func (m multiResolver) For(_ context.Context, _ uuid.UUID, accountID *uuid.UUID) (storage.Backend, error) {
	if accountID == nil {
		return nil, errors.New("no account for shard")
	}
	b, ok := m.backends[*accountID]
	if !ok {
		return nil, errors.New("drive not connected")
	}
	return b, nil
}

// newStriped builds n drives of the given capacity each.
func newStriped(t *testing.T, drives int, capacityEach int64, shardSize int64, encrypt bool) *stripedFixture {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	routerStore := router.NewMemoryStore()
	backends := map[uuid.UUID]*local.Backend{}
	ids := make([]uuid.UUID, 0, drives)

	for i := 0; i < drives; i++ {
		id := uuid.New()
		ids = append(ids, id)
		routerStore.AddAccount(id, int32(i+1), "drive@example.com", capacityEach, 0)

		b, berr := local.New(t.TempDir(), local.WithFakeCapacity(capacityEach))
		if berr != nil {
			t.Fatalf("local.New() = %v", berr)
		}
		backends[id] = b
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reserver := router.NewReserver(routerStore, logger)

	storedSize := func(n int64) int64 { return n }
	if encrypt {
		storedSize = skcrypto.StreamOverhead
	}
	planner := router.NewPlanner(reserver, router.PolicyMostAvailable, shardSize, storedSize)

	store := files.NewMemoryStore()
	svc := files.NewService(
		store,
		files.NewStripingPlanner(planner, reserver),
		multiResolver{backends},
		ring,
		files.Config{Encrypt: encrypt, MaxUploadBytes: 1 << 40},
		logger,
	)

	return &stripedFixture{
		svc:      svc,
		store:    store,
		router:   routerStore,
		backends: backends,
		accounts: ids,
		userID:   uuid.New(),
	}
}

func (f *stripedFixture) upload(t *testing.T, name string, data []byte) files.File {
	t.Helper()
	file, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: name, Size: int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload(%q) = %v", name, err)
	}
	return file
}

func (f *stripedFixture) read(t *testing.T, id uuid.UUID, rng *storage.ByteRange) []byte {
	t.Helper()
	content, err := f.svc.Open(context.Background(), f.userID, id, rng)
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
	return got
}

// The headline claim, scaled down so it runs in a test: a file no single drive
// can hold, split across three, encrypted, read back byte-identical.
//
// The ratio is what matters, not the absolute size. Three 4 MiB drives and a
// 9 MiB file exercises exactly the same code path as three 15 GB accounts and
// a 30 GB archive, in a second rather than an hour.
func TestStripeAcrossDrivesAndReadBackIdentical(t *testing.T) {
	const (
		driveCapacity = 4 << 20
		shardSize     = 1 << 20
		fileSize      = 9 << 20
	)
	f := newStriped(t, 3, driveCapacity, shardSize, true)

	data := make([]byte, fileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(data)

	file := f.upload(t, "archive.tar.zst", data)

	if !file.IsStriped {
		t.Error("the file was not marked striped")
	}
	if !file.IsEncrypted {
		t.Error("the file was not marked encrypted")
	}
	if len(file.Shards) < 3 {
		t.Fatalf("shards = %d, want at least 3 for a file larger than one drive", len(file.Shards))
	}

	// It genuinely spans drives; no single one could have held it.
	perDrive := map[uuid.UUID]int64{}
	for _, sh := range file.Shards {
		if sh.AccountID == nil {
			t.Fatalf("shard %d has no drive", sh.Index)
		}
		perDrive[*sh.AccountID] += sh.SizeBytes
	}
	if len(perDrive) < 3 {
		t.Errorf("the file used %d drives, want 3", len(perDrive))
	}
	for id, n := range perDrive {
		if n > driveCapacity {
			t.Errorf("drive %s holds %d bytes, over its %d capacity", id, n, int64(driveCapacity))
		}
	}

	// Byte-identical round trip.
	got := f.read(t, file.ID, nil)
	if len(got) != fileSize {
		t.Fatalf("read %d bytes, want %d", len(got), fileSize)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("the checksum changed across a striped, encrypted round trip")
	}

	// And the stored checksum is over plaintext, so it matches the file the
	// user actually uploaded.
	if !bytes.Equal(file.ContentSHA, want[:]) {
		t.Error("content_sha256 does not match the uploaded plaintext")
	}
}

// Everything at rest is ciphertext. This is the programmatic form of
// "open a shard directly in Google Drive's web UI and see nothing".
func TestShardsAtRestAreCiphertext(t *testing.T) {
	f := newStriped(t, 2, 8<<20, 1<<20, true)

	// A recognisable, highly compressible plaintext: if any of it survived
	// to disk, the search below finds it.
	marker := []byte("SKEIN-PLAINTEXT-MARKER-0123456789")
	data := bytes.Repeat(marker, 40_000)

	file := f.upload(t, "secrets.txt", data)

	for _, sh := range file.Shards {
		backend := f.backends[*sh.AccountID]
		rc, _, err := backend.Get(context.Background(),
			storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}, nil)
		if err != nil {
			t.Fatalf("read shard %d at rest: %v", sh.Index, err)
		}
		stored, err := io.ReadAll(rc)
		if cerr := rc.Close(); cerr != nil {
			t.Errorf("Close() = %v", cerr)
		}
		if err != nil {
			t.Fatalf("read shard %d: %v", sh.Index, err)
		}

		if bytes.Contains(stored, marker) {
			t.Fatalf("shard %d contains plaintext at rest", sh.Index)
		}

		// It is a framed stream, with the version and key id in the clear
		// so a future rotation can identify it.
		if stored[0] != skcrypto.StreamV1 {
			t.Errorf("shard %d does not start with the stream version byte", sh.Index)
		}
		if int64(len(stored)) != skcrypto.StreamOverhead(sh.PlainSize) {
			t.Errorf("shard %d is %d bytes, want %d for %d plaintext",
				sh.Index, len(stored), skcrypto.StreamOverhead(sh.PlainSize), sh.PlainSize)
		}
	}
}

// Range requests over a striped, encrypted file. This is what makes scrubbing
// a striped video work rather than merely not crash.
func TestRangeReadsOnAStripedEncryptedFile(t *testing.T) {
	const (
		shardSize = 1 << 20
		fileSize  = 5 << 20
	)
	f := newStriped(t, 3, 8<<20, shardSize, true)

	data := make([]byte, fileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "video.mkv", data)

	tests := []struct {
		name   string
		start  int64
		length int64
	}{
		{"first byte", 0, 1},
		{"inside the first shard", 100, 4096},
		{"across a shard boundary", shardSize - 512, 1024},
		{"a whole middle shard", shardSize, shardSize},
		{"spanning three shards", shardSize - 10, 2*shardSize + 20},
		{"the tail", fileSize - 1000, 1000},
		{"last byte", fileSize - 1, 1},
		{"the exit criterion: 1000-2000", 1000, 1001},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := f.read(t, file.ID, &storage.ByteRange{Start: tc.start, Length: tc.length})
			if int64(len(got)) != tc.length {
				t.Fatalf("got %d bytes, want %d", len(got), tc.length)
			}
			want := data[tc.start : tc.start+tc.length]
			if !bytes.Equal(got, want) {
				t.Errorf("range [%d,%d) returned the wrong bytes", tc.start, tc.start+tc.length)
			}
		})
	}
}

// Never start an upload that cannot finish.
func TestUploadRefusedWhenThePoolIsTooSmall(t *testing.T) {
	f := newStriped(t, 2, 1<<20, 1<<20, true)

	_, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "huge.bin", Size: 10 << 20,
	}, bytes.NewReader(make([]byte, 10<<20)))

	if !errors.Is(err, skerr.ErrQuotaExceeded) {
		t.Fatalf("Upload() = %v, want ErrQuotaExceeded", err)
	}

	// Nothing was written and nothing is still reserved.
	for _, id := range f.accounts {
		if got := f.router.ReservedOn(id); got != 0 {
			t.Errorf("drive %s still holds %d reserved bytes after a refused upload", id, got)
		}
	}
	if n := len(f.store.ListShardsSnapshot()); n != 0 {
		t.Errorf("a refused upload created %d shards", n)
	}
}

// A successful upload must not leave capacity reserved: the bytes are used
// now, not promised.
func TestReservationsAreReleasedAfterAnUpload(t *testing.T) {
	f := newStriped(t, 3, 8<<20, 1<<20, true)

	file := f.upload(t, "data.bin", make([]byte, 4<<20))
	if len(file.Shards) < 2 {
		t.Fatalf("shards = %d, want the file to be striped", len(file.Shards))
	}

	for _, id := range f.accounts {
		if got := f.router.ReservedOn(id); got != 0 {
			t.Errorf("drive %s still holds %d reserved bytes after a committed upload", id, got)
		}
	}
}

// A failed upload releases its reservations and deletes every shard it wrote.
func TestFailedStripedUploadLeavesNothingBehind(t *testing.T) {
	f := newStriped(t, 3, 8<<20, 1<<20, true)

	// Declare more than is actually sent: the byte-count check fails after
	// several shards have already been written.
	_, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "truncated.bin", Size: 5 << 20,
	}, bytes.NewReader(make([]byte, 2<<20)))
	if err == nil {
		t.Fatal("Upload() succeeded with a short body")
	}

	for _, id := range f.accounts {
		if got := f.router.ReservedOn(id); got != 0 {
			t.Errorf("drive %s still holds %d reserved bytes after a failed upload", id, got)
		}
	}
	if n := len(f.store.ListShardsSnapshot()); n != 0 {
		t.Errorf("%d shard rows survived a failed upload", n)
	}

	// And no orphan objects at any provider: every byte written was removed.
	for id, b := range f.backends {
		q, qerr := b.Quota(context.Background())
		if qerr != nil {
			t.Fatalf("Quota() = %v", qerr)
		}
		if q.UsedBytes != 0 {
			t.Errorf("drive %s holds %d orphaned bytes after a failed upload", id, q.UsedBytes)
		}
	}
}

// Corrupting a shard at rest must be caught, not decrypted into plausible
// garbage. This is what the per-frame AEAD buys.
func TestCorruptShardIsDetectedOnRead(t *testing.T) {
	f := newStriped(t, 2, 8<<20, 1<<20, true)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "data.bin", data)

	// Flip a byte inside the first shard's ciphertext, past the header.
	sh := file.Shards[0]
	backend := f.backends[*sh.AccountID]
	rc, _, err := backend.Get(context.Background(),
		storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	stored, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Errorf("Close() = %v", cerr)
	}
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}

	stored[skcrypto.StreamHeaderLen+100] ^= 0xff
	if derr := backend.Delete(context.Background(),
		storage.ObjectRef{ProviderID: sh.ProviderID}); derr != nil {
		t.Fatalf("Delete() = %v", derr)
	}
	if _, perr := backend.Put(context.Background(), bytes.NewReader(stored), storage.ObjectSpec{
		Name: sh.ProviderID, Size: int64(len(stored)),
	}); perr != nil {
		t.Fatalf("Put() = %v", perr)
	}

	content, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = content.Body.Close() }()

	// The read must fail rather than hand back damaged plaintext.
	if _, rerr := io.ReadAll(content.Body); rerr == nil {
		t.Fatal("a corrupted shard decrypted without error")
	}
}

// THE SAME PROPERTY WITHOUT ENCRYPTION, WHICH IS WHERE IT WAS MISSING.
//
// An encrypted shard is protected by AEAD: a flipped byte fails the Poly1305
// tag and the read errors out, which the test above proves. An UNENCRYPTED
// shard had no integrity check at all — file_shards.sha256 is computed and
// stored on upload (upload.go) and was never compared on read, so corruption
// surfaced as wrong bytes handed to the user with a 200.
//
// TestStripingWithoutEncryption proves that path is live, so this is not a
// hypothetical configuration.
func TestCorruptShardIsDetectedOnReadWithoutEncryption(t *testing.T) {
	f := newStriped(t, 2, 8<<20, 1<<20, false)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "plain.bin", data)
	if file.IsEncrypted {
		t.Fatal("fixture built an encrypted file; this test must exercise the plaintext path")
	}

	// Corrupt one byte of stored plaintext. No header to skip past.
	sh := file.Shards[0]
	backend := f.backends[*sh.AccountID]
	rc, _, err := backend.Get(context.Background(),
		storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	stored, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Errorf("Close() = %v", cerr)
	}
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}

	stored[100] ^= 0xff
	if derr := backend.Delete(context.Background(),
		storage.ObjectRef{ProviderID: sh.ProviderID}); derr != nil {
		t.Fatalf("Delete() = %v", derr)
	}
	if _, perr := backend.Put(context.Background(), bytes.NewReader(stored), storage.ObjectSpec{
		Name: sh.ProviderID, Size: int64(len(stored)),
	}); perr != nil {
		t.Fatalf("Put() = %v", perr)
	}

	content, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = content.Body.Close() }()

	got, rerr := io.ReadAll(content.Body)
	if rerr == nil {
		t.Fatalf("a corrupted unencrypted shard was served without error; "+
			"%d bytes returned, %d of which differ from the original",
			len(got), countDiff(got, data))
	}
	if !errors.Is(rerr, skerr.ErrIntegrity) {
		t.Errorf("error = %v, want ErrIntegrity", rerr)
	}
}

// countDiff reports how many bytes differ, so a failure says how much damage
// was handed to the caller rather than merely that it happened.
func countDiff(a, b []byte) int {
	n := 0
	for i := range a {
		if i < len(b) && a[i] != b[i] {
			n++
		}
	}
	return n
}

// The unencrypted path still works, so a development deployment with
// SKEIN_ENCRYPTION_ENABLED=false is not silently broken.
func TestStripingWithoutEncryption(t *testing.T) {
	f := newStriped(t, 3, 4<<20, 1<<20, false)

	data := make([]byte, 6<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "plain.bin", data)

	if file.IsEncrypted {
		t.Error("the file is marked encrypted with encryption disabled")
	}
	if got := f.read(t, file.ID, nil); !bytes.Equal(got, data) {
		t.Error("the unencrypted striped round trip changed the bytes")
	}
}

// Per-shard checksums are recorded, so a corrupt shard localises to one drive
// rather than to "somewhere in a 30 GB file".
func TestPerShardChecksumsAreRecorded(t *testing.T) {
	f := newStriped(t, 3, 8<<20, 1<<20, true)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "data.bin", data)

	var offset int64
	for _, sh := range file.Shards {
		if len(sh.SHA256) != sha256.Size {
			t.Fatalf("shard %d has no checksum", sh.Index)
		}
		want := sha256.Sum256(data[offset : offset+sh.PlainSize])
		if !bytes.Equal(sh.SHA256, want[:]) {
			t.Errorf("shard %d checksum does not match its plaintext slice", sh.Index)
		}
		offset += sh.PlainSize
	}
	if offset != int64(len(data)) {
		t.Errorf("shards cover %d bytes, want %d", offset, len(data))
	}
}

// Range reads: the digest covers a whole shard, so a partial read cannot
// verify it. That is DEFINED behaviour, not an oversight — see
// readSegment.wholeShard. This test pins both halves: a range read still
// succeeds, and a full read of the same corrupt file still fails.
//
// The exposure this documents: a plaintext RANGE read is not digest-verified.
// Encrypted files stay AEAD-protected on every path, ranges included, so the
// gap exists only for SKEIN_ENCRYPTION_ENABLED=false plus a range request.
func TestRangeReadSkipsWholeShardDigestVerification(t *testing.T) {
	f := newStriped(t, 2, 8<<20, 1<<20, false)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "plain.bin", data)

	// Corrupt a byte late in the first shard.
	sh := file.Shards[0]
	backend := f.backends[*sh.AccountID]
	rc, _, err := backend.Get(context.Background(),
		storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	corruptAt := len(stored) - 10
	stored[corruptAt] ^= 0xff
	if derr := backend.Delete(context.Background(),
		storage.ObjectRef{ProviderID: sh.ProviderID}); derr != nil {
		t.Fatalf("Delete() = %v", derr)
	}
	if _, perr := backend.Put(context.Background(), bytes.NewReader(stored), storage.ObjectSpec{
		Name: sh.ProviderID, Size: int64(len(stored)),
	}); perr != nil {
		t.Fatalf("Put() = %v", perr)
	}

	// A range covering only the first 1 KiB is served: it does not span the
	// shard, so there is no whole-shard digest to compare it against.
	content, err := f.svc.Open(context.Background(), f.userID, file.ID,
		&storage.ByteRange{Start: 0, Length: 1024})
	if err != nil {
		t.Fatalf("Open(range) = %v", err)
	}
	got, rerr := io.ReadAll(content.Body)
	_ = content.Body.Close()
	if rerr != nil {
		t.Fatalf("a partial read failed: %v; ranges are not digest-verified "+
			"and this one does not touch the corrupted byte", rerr)
	}
	if !bytes.Equal(got, data[:1024]) {
		t.Error("the range read returned the wrong bytes")
	}

	// The full read of the same file still fails, which is the guarantee that
	// matters: corruption cannot be laundered by reading the whole file.
	full, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err != nil {
		t.Fatalf("Open(full) = %v", err)
	}
	defer func() { _ = full.Body.Close() }()
	if _, ferr := io.ReadAll(full.Body); ferr == nil {
		t.Error("the full read of a corrupted file succeeded")
	}
}

// PRD.md:139 requires refusing partial reassembly. A file whose shards no
// longer cover its recorded size must not be served at all — not served short,
// and not served with a gap silently skipped.
//
// Driven through Open rather than the unexported checker, so it asserts what a
// caller actually gets.
func TestPartialReassemblyIsRefused(t *testing.T) {
	f := newStriped(t, 2, 8<<20, 1<<20, false)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "plain.bin", data)
	if len(file.Shards) < 2 {
		t.Fatalf("fixture produced %d shards; need at least 2", len(file.Shards))
	}

	// Punch a hole in the manifest: shrink one shard so the recorded shards
	// no longer cover the file's size.
	f.store.CorruptShard(file.ID, 0, func(sh *files.Shard) {
		sh.PlainSize -= 4096
	})

	content, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err == nil {
		defer func() { _ = content.Body.Close() }()
		got, rerr := io.ReadAll(content.Body)
		t.Fatalf("a file with an incomplete shard map was served: "+
			"%d bytes, read error %v; partial reassembly must be refused",
			len(got), rerr)
	}
	if !errors.Is(err, skerr.ErrIntegrity) {
		t.Errorf("error = %v, want ErrIntegrity", err)
	}
}
