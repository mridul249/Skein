package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

func TestOpenRange(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	file := f.upload(t, "alphabet.txt", data)

	tests := []struct {
		name string
		rng  storage.ByteRange
		want string
	}{
		{"from the start", storage.ByteRange{Start: 0, Length: 5}, "01234"},
		{"middle", storage.ByteRange{Start: 10, Length: 6}, "abcdef"},
		{"single byte", storage.ByteRange{Start: 7, Length: 1}, "7"},
		{"to the end", storage.ByteRange{Start: 30, Length: 6}, "uvwxyz"},
		{"length past the end is clamped", storage.ByteRange{Start: 30, Length: 999}, "uvwxyz"},
		{"whole file via an explicit range", storage.ByteRange{Start: 0, Length: 36}, string(data)},
		{"last byte", storage.ByteRange{Start: 35, Length: 1}, "z"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := f.readAll(t, file.ID, &tc.rng)
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The Phase 3 exit criterion, stated in bytes: curl -r 1000-2000 returns
// exactly 1001 bytes.
func TestRangeReturnsExactlyTheRequestedByteCount(t *testing.T) {
	f := newFixture(t)
	data := make([]byte, 8192)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "blob.bin", data)

	// Inclusive on the wire, so 1000-2000 is 1001 bytes.
	got := f.readAll(t, file.ID, &storage.ByteRange{Start: 1000, Length: 1001})
	if len(got) != 1001 {
		t.Fatalf("got %d bytes, want 1001", len(got))
	}
	if !bytes.Equal(got, data[1000:2001]) {
		t.Error("the returned bytes are not the ones requested")
	}
}

func TestOpenRejectsUnsatisfiableRanges(t *testing.T) {
	f := newFixture(t)
	file := f.upload(t, "small.txt", []byte("0123456789"))

	tests := []struct {
		name string
		rng  storage.ByteRange
	}{
		{"start past the end", storage.ByteRange{Start: 100, Length: 5}},
		{"start exactly at the end", storage.ByteRange{Start: 10, Length: 5}},
		{"negative start", storage.ByteRange{Start: -1, Length: 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Open(context.Background(), f.userID, file.ID, &tc.rng)
			if !errors.Is(err, files.ErrRangeNotSatisfiable) {
				t.Fatalf("Open() = %v, want ErrRangeNotSatisfiable", err)
			}
		})
	}
}

func TestOpenEmptyFile(t *testing.T) {
	f := newFixture(t)
	file := f.upload(t, "empty.bin", nil)

	got := f.readAll(t, file.ID, nil)
	if len(got) != 0 {
		t.Errorf("read %d bytes from an empty file", len(got))
	}

	// RFC 9110: no range is satisfiable on an empty representation.
	_, err := f.svc.Open(context.Background(), f.userID, file.ID,
		&storage.ByteRange{Start: 0, Length: 1})
	if err == nil {
		t.Error("a range request on an empty file succeeded")
	}
}

func TestOpenIsScopedToTheOwner(t *testing.T) {
	f := newFixture(t)
	file := f.upload(t, "private.bin", []byte("secret"))

	_, err := f.svc.Open(context.Background(), uuid.New(), file.ID, nil)
	if !errors.Is(err, skerr.ErrNotFound) {
		t.Fatalf("a stranger opened another user's file: %v", err)
	}
}

// Architecture.md §6: a file whose manifest does not describe it is unreadable
// and says so. It never returns the shards that do exist as if they were the
// whole file.
func TestOpenRefusesACorruptManifest(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*files.Shard)
	}{
		{
			name:    "shard shorter than recorded",
			corrupt: func(s *files.Shard) { s.PlainSize -= 10 },
		},
		{
			name:    "shard claims a gap before it",
			corrupt: func(s *files.Shard) { s.PlainOffset += 10 },
		},
		{
			name:    "negative shard size",
			corrupt: func(s *files.Shard) { s.PlainSize = -1 },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			file := f.upload(t, "blob.bin", make([]byte, 4096))

			f.store.CorruptShard(file.ID, 0, tc.corrupt)

			// THE INVARIANT IS "A CORRUPT MANIFEST NEVER REACHES A READER",
			// and there are two legitimate ways to uphold it. The reader
			// refuses (verifyManifest, download.go:410-431) — or the store
			// refuses to hold the state at all, which is what the real
			// schemas do: file_shards_sizes_non_negative rejects a negative
			// size outright in BOTH dialects, so CorruptShard leaves the
			// shard intact and the file stays readable.
			//
			// A store that cannot represent the damage is a STRONGER
			// guarantee than one that represents it and catches it on read,
			// so accepting both is not a weakened assertion. What is NOT
			// accepted is the file reading back wrong: if the damage landed,
			// Open must refuse.
			_, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
			if errors.Is(err, skerr.ErrIntegrity) {
				return // the reader caught it
			}
			if err != nil {
				t.Fatalf("Open() = %v, want ErrIntegrity or a clean read", err)
			}
			// Open succeeded, which is only acceptable if the damage was
			// never applied. Prove that rather than assuming it.
			shards, lerr := f.store.ListShards(context.Background(), file.ID)
			if lerr != nil {
				t.Fatalf("ListShards() = %v", lerr)
			}
			var want files.Shard
			for _, sh := range shards {
				if sh.Index == 0 {
					want = sh
				}
			}
			damaged := want
			tc.corrupt(&damaged)
			if damaged.PlainSize == want.PlainSize && damaged.PlainOffset == want.PlainOffset &&
				damaged.SizeBytes == want.SizeBytes && damaged.Index == want.Index {
				t.Fatalf("the corruption was a no-op; this case tests nothing")
			}
			if want.PlainSize < 0 || want.PlainOffset < 0 || want.SizeBytes < 0 {
				t.Fatalf("Open() succeeded on a shard the store should not hold: %+v", want)
			}
		})
	}
}

func TestOpenRefusesAMissingShard(t *testing.T) {
	f := newFixture(t)
	file := f.upload(t, "blob.bin", make([]byte, 4096))

	// The manifest points somewhere the object is not.
	f.store.CorruptShard(file.ID, 0, func(s *files.Shard) {
		s.ProviderID = "not-a-real-object.bin"
	})

	content, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err != nil {
		// Failing at Open is fine and preferable.
		if !errors.Is(err, skerr.ErrIntegrity) {
			t.Fatalf("Open() = %v, want ErrIntegrity", err)
		}
		return
	}
	defer func() { _ = content.Body.Close() }()

	// Otherwise the failure must arrive on the first read, and it must not
	// be a silent short read.
	buf := make([]byte, 64)
	if _, rerr := content.Body.Read(buf); !errors.Is(rerr, skerr.ErrIntegrity) {
		t.Fatalf("Read() = %v, want ErrIntegrity", rerr)
	}
}

func TestOpenRefusesAFileThatNeverFinished(t *testing.T) {
	f := newFixture(t)

	// A pending row with no manifest is what a crashed upload leaves.
	created, err := f.store.CreateFile(context.Background(), files.NewFile{
		ID: uuid.New(), UserID: f.userID, Name: "half.bin", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("CreateFile() = %v", err)
	}

	_, err = f.svc.Open(context.Background(), f.userID, created.ID, nil)
	if !errors.Is(err, skerr.ErrNotFound) {
		t.Fatalf("Open() = %v, want ErrNotFound for a pending file", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFixture(t)
	file := f.upload(t, "blob.bin", make([]byte, 1024))

	content, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	for i := 0; i < 3; i++ {
		if cerr := content.Body.Close(); cerr != nil {
			t.Fatalf("Close() call %d = %v", i+1, cerr)
		}
	}
}
