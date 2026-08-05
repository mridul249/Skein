package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
)

// Sidecar manifests: the record of what a shard belongs to, written beside the
// shards themselves so losing the database does not lose the map.
//
// FIXTURE HONESTY NOTE, and it is the reason the first test below exists at
// all. Session 5 Block 1 found the files conformance harness silently skipping
// a migration because it selected by filename substring, so every SQLite test
// ran against a stale table while reporting success. That is the fourth
// instance of a harness certifying something it was not exercising. So before
// asserting anything ABOUT manifests, this file asserts that the fixture
// actually drives the real upload path and that manifests really land at the
// provider — rather than trusting a green run to mean what it appears to.

// manifestObjects returns the manifest objects present on one backend, keyed by
// object name, read straight from the fake provider rather than from anything
// the service reports about itself.
func manifestObjects(t *testing.T, f *sharedDriveFixture, accountID uuid.UUID) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, body := range f.objects(t, accountID) {
		if files.IsManifestName(name) {
			out[name] = body
		}
	}
	return out
}

// THE FIXTURE CHECK. If this fails, every other test in this file is
// meaningless regardless of whether it passes.
//
// It asserts the negative first — no manifests before an upload — so a fixture
// that somehow pre-seeded them, or one whose object listing returns everything
// unconditionally, cannot make the positive assertions pass vacuously.
func TestTheFixtureActuallyExercisesTheManifestWritePath(t *testing.T) {
	f := newSharedDrive(t)

	for _, acct := range f.accounts {
		if got := manifestObjects(t, f, acct); len(got) != 0 {
			t.Fatalf("account %s already has %d manifest(s) before any upload; "+
				"the fixture is not starting clean", acct, len(got))
		}
	}

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "fixture-check.bin", data)

	var total int
	for _, acct := range f.accounts {
		total += len(manifestObjects(t, f, acct))
	}
	if total == 0 {
		t.Fatal("an upload through the real service produced NO manifest objects at " +
			"the provider; the write path is not being exercised and nothing " +
			"else in this file proves anything")
	}
	t.Logf("upload of %s produced %d manifest object(s) across %d account(s)",
		file.ID, total, len(f.accounts))
}

// ONE COPY PER PARTICIPATING ACCOUNT, NOT ONE TOTAL.
//
// The whole design rests on this: any single surviving drive must be enough to
// bootstrap discovery of every other. A single-copy scheme means losing that
// one account loses the map to everything else.
func TestAManifestLandsOnEveryAccountHoldingAShard(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	// Large enough to stripe across both drives in the fixture.
	data := make([]byte, 6<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "striped.bin", data)

	stored, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}

	participating := map[uuid.UUID]bool{}
	for _, sh := range stored.Shards {
		if sh.AccountID != nil {
			participating[*sh.AccountID] = true
		}
	}
	if len(participating) < 2 {
		t.Fatalf("the file used %d account(s); this test needs a striped file to "+
			"mean anything", len(participating))
	}

	want := files.ManifestName(file.ID)
	for acct := range participating {
		got := manifestObjects(t, f, acct)
		if _, ok := got[want]; !ok {
			t.Errorf("account %s holds a shard but has no manifest %q; "+
				"losing the other account would lose the map to this one",
				acct, want)
		}
	}
}

// The manifest must describe the shard layout EXACTLY as the database does.
// A manifest that disagrees with file_shards is worse than none: a
// reconstruction from it would produce a file whose bytes are in the wrong
// order, silently.
func TestAManifestMatchesTheShardRowsExactly(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 6<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "compare.bin", data)

	stored, err := f.svc.Get(ctx, f.user1, file.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}

	sealed := findAnyManifest(t, f, file.ID)
	m, err := files.OpenManifest(f.ring, file.ID, sealed)
	if err != nil {
		t.Fatalf("OpenManifest() = %v", err)
	}

	if m.Version != files.ManifestVersion {
		t.Errorf("version = %d, want %d", m.Version, files.ManifestVersion)
	}
	if m.FileID != stored.ID {
		t.Errorf("file_id = %s, want %s", m.FileID, stored.ID)
	}
	if m.UserID != f.user1 {
		t.Errorf("user_id = %s, want %s", m.UserID, f.user1)
	}
	if m.FileName != stored.Name {
		t.Errorf("file_name = %q, want %q", m.FileName, stored.Name)
	}
	if m.PlainSizeBytes != stored.SizeBytes {
		t.Errorf("plain_size_bytes = %d, want %d", m.PlainSizeBytes, stored.SizeBytes)
	}

	if len(m.Shards) != len(stored.Shards) {
		t.Fatalf("manifest has %d shards, the database has %d",
			len(m.Shards), len(stored.Shards))
	}
	for i, sh := range stored.Shards {
		ms := m.Shards[i]
		if ms.Index != sh.Index {
			t.Errorf("shard %d: index %d, want %d", i, ms.Index, sh.Index)
		}
		if ms.ProviderObjectID != sh.ProviderID {
			t.Errorf("shard %d: provider_object_id %q, want %q",
				i, ms.ProviderObjectID, sh.ProviderID)
		}
		// Both sizes, per issue #9: ciphertext and plaintext are different
		// numbers and a reconstruction needs both.
		if ms.CiphertextSize != sh.SizeBytes {
			t.Errorf("shard %d: ciphertext_size_bytes %d, want %d",
				i, ms.CiphertextSize, sh.SizeBytes)
		}
		if ms.PlainSize != sh.PlainSize {
			t.Errorf("shard %d: plain_size_bytes %d, want %d",
				i, ms.PlainSize, sh.PlainSize)
		}
		if ms.PlainOffset != sh.PlainOffset {
			t.Errorf("shard %d: plain_offset %d, want %d",
				i, ms.PlainOffset, sh.PlainOffset)
		}
		if want := hex.EncodeToString(sh.SHA256); ms.SHA256 != want {
			t.Errorf("shard %d: sha256 %q, want %q", i, ms.SHA256, want)
		}
		if (ms.AccountID == nil) != (sh.AccountID == nil) {
			t.Errorf("shard %d: account_id nil-ness differs from the row", i)
		}
	}
}

// A manifest at rest is ciphertext, and is unreadable without the master key.
// It names every provider object of a file and which drive each sits on — a
// plaintext one would hand an inventory of the user's storage to anyone who
// could list the folder.
func TestAManifestIsUnreadableWithoutTheMasterKey(t *testing.T) {
	f := newSharedDrive(t)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "secret-name.bin", data)
	sealed := findAnyManifest(t, f, file.ID)

	// Nothing legible at rest: not the filename, not a provider object id.
	if strings.Contains(string(sealed), "secret-name.bin") {
		t.Error("the manifest carries the file name in plaintext at rest")
	}
	if strings.Contains(string(sealed), file.ID.String()) {
		t.Error("the manifest carries the file id in plaintext in its BODY")
	}

	// A different master key cannot open it.
	other := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(other); err != nil {
		t.Fatalf("rand: %v", err)
	}
	wrong, err := skcrypto.NewKeyring(other)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	if _, oerr := files.OpenManifest(wrong, file.ID, sealed); oerr == nil {
		t.Fatal("a DIFFERENT master key opened the manifest")
	}

	// And the right one does, so the assertion above is not passing because
	// the manifest is simply broken.
	if _, oerr := files.OpenManifest(f.ring, file.ID, sealed); oerr != nil {
		t.Fatalf("the correct key could not open the manifest: %v", oerr)
	}
}

// A manifest is bound to its file id, and the binding has TWO layers. Both are
// asserted, because they fail in different ways and only one of them is a check
// anyone could delete by accident.
//
// Layer 1 is CRYPTOGRAPHIC and cannot be removed without changing the key
// derivation: the file id is the HKDF salt, so a manifest renamed onto another
// file derives a different key and does not decrypt at all. Verified rather
// than assumed — the error is "crypto: decryption failed", not a field
// comparison.
//
// Layer 2 is the explicit FileID comparison in OpenManifest, which layer 1
// makes unreachable through the rename path. It guards a DIFFERENT case: a
// manifest whose sealed body names one file while being stored under another,
// which is what a bug in a future writer would produce. It is not redundant; it
// is a consistency check on our own output.
func TestAManifestIsBoundToItsFileID(t *testing.T) {
	f := newSharedDrive(t)

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.uploadAs(t, f.user1, "bound.bin", data)
	sealed := findAnyManifest(t, f, file.ID)

	// Layer 1: renaming onto another file cannot decrypt.
	_, err := files.OpenManifest(f.ring, uuid.New(), sealed)
	if err == nil {
		t.Fatal("a manifest opened under a DIFFERENT file id; it is not bound to its file")
	}
	if !strings.Contains(err.Error(), "decryption failed") {
		t.Errorf("error = %v; expected the failure to come from the KEY DERIVATION "+
			"(the file id is the HKDF salt), not from a later field comparison", err)
	}

	// Layer 2: a body that disagrees with the id it was stored under is
	// refused. Built by hand, because no correct writer can produce it.
	sealedUnder, claimsToBe := uuid.New(), uuid.New()
	inconsistent, serr := sealManifestUnder(t, f.ring, sealedUnder, files.Manifest{
		Version: files.ManifestVersion,
		FileID:  claimsToBe,
		UserID:  f.user1,
		Shards:  []files.ManifestShard{},
	})
	if serr != nil {
		t.Fatalf("build an inconsistent manifest: %v", serr)
	}
	_, oerr := files.OpenManifest(f.ring, sealedUnder, inconsistent)
	if oerr == nil {
		t.Fatal("a manifest whose body names a DIFFERENT file than it was stored " +
			"under was accepted")
	}
	if !strings.Contains(oerr.Error(), "contents describe file") {
		t.Errorf("error = %v; expected the FileID consistency check to reject it", oerr)
	}
}

// An unknown format version is REFUSED, not partially understood.
//
// A reader that meets a version it does not know and proceeds anyway is how a
// "successful" reconstruction silently produces wrong byte offsets — the
// fields it does understand look fine, and the ones it does not are simply
// absent. Refusing is the only safe reading, and it must stay covered: the
// check guards a format change that has not happened yet, so nothing else in
// the suite would notice its removal.
func TestAManifestFromAFutureVersionIsRefused(t *testing.T) {
	f := newSharedDrive(t)

	fileID := uuid.New()
	sealed, err := sealManifestUnder(t, f.ring, fileID, files.Manifest{
		Version: files.ManifestVersion + 1,
		FileID:  fileID,
		UserID:  f.user1,
		Shards:  []files.ManifestShard{},
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	_, oerr := files.OpenManifest(f.ring, fileID, sealed)
	if oerr == nil {
		t.Fatal("a manifest from a FUTURE format version was accepted; this build " +
			"would interpret fields it does not understand")
	}
	if !strings.Contains(oerr.Error(), "not supported by this build") {
		t.Errorf("error = %v; expected the version check to reject it", oerr)
	}
}

// sealManifestUnder seals a manifest under an arbitrary salt, so a test can
// construct the body-vs-name mismatch that no correct writer produces.
func sealManifestUnder(t *testing.T, ring *skcrypto.Keyring, salt uuid.UUID, m files.Manifest) ([]byte, error) {
	t.Helper()
	plain, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return ring.Seal(skcrypto.InfoManifest, salt[:], plain)
}

// THE FAILURE-BEHAVIOUR REQUIREMENT.
//
// A manifest write failure must not fail the upload. The manifest is a
// redundancy layer, and letting it break the primary path inverts the entire
// point of adding it: a user would lose the ability to store a file because
// the thing protecting them from losing files did not work.
func TestAManifestWriteFailureLeavesTheUploadSuccessfulAndReadable(t *testing.T) {
	f := newSharedDrive(t)
	ctx := context.Background()

	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	// Every drive refuses writes of the manifest object specifically, so the
	// shards land and only the manifest fails.
	f.failManifestWrites()

	file, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.user1, Name: "survives.bin", Size: int64(len(data)),
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("a manifest write failure FAILED THE UPLOAD: %v", err)
	}

	// The file is committed...
	stored, gerr := f.svc.Get(ctx, f.user1, file.ID)
	if gerr != nil {
		t.Fatalf("Get() after a failed manifest write = %v", gerr)
	}
	if stored.Status != files.StatusReady {
		t.Errorf("status = %q, want %q", stored.Status, files.StatusReady)
	}

	// ...and readable, byte for byte.
	content, oerr := f.svc.Open(ctx, f.user1, file.ID, nil)
	if oerr != nil {
		t.Fatalf("Open() after a failed manifest write = %v", oerr)
	}
	defer func() { _ = content.Body.Close() }()
	got, rerr := io.ReadAll(content.Body)
	if rerr != nil {
		t.Fatalf("read after a failed manifest write = %v", rerr)
	}
	if !bytes.Equal(got, data) {
		t.Error("the file did not read back byte-for-byte after a failed manifest write")
	}

	// And no manifest was written, so the test is not passing because the
	// failure injection did nothing.
	var total int
	for _, acct := range f.accounts {
		total += len(manifestObjects(t, f, acct))
	}
	if total != 0 {
		t.Errorf("%d manifest(s) were written despite the injected failure; "+
			"the failure was not actually injected", total)
	}
}

// findAnyManifest returns one file's sealed manifest from whichever account
// holds a copy.
func findAnyManifest(t *testing.T, f *sharedDriveFixture, fileID uuid.UUID) []byte {
	t.Helper()
	want := files.ManifestName(fileID)
	for _, acct := range f.accounts {
		if body, ok := manifestObjects(t, f, acct)[want]; ok {
			return body
		}
	}
	t.Fatalf("no manifest %q on any account", want)
	return nil
}
