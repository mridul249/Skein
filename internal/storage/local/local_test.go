package local_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/local"
)

func newBackend(t *testing.T, opts ...local.Option) *local.Backend {
	t.Helper()
	b, err := local.New(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("local.New() = %v", err)
	}
	return b
}

func put(t *testing.T, b *local.Backend, name string, data []byte) storage.ObjectRef {
	t.Helper()
	ref, err := b.Put(context.Background(), bytes.NewReader(data), storage.ObjectSpec{
		Name:        name,
		Size:        int64(len(data)),
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put() = %v", err)
	}
	return ref
}

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	}()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func TestPutGetRoundTrip(t *testing.T) {
	b := newBackend(t)
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	ref := put(t, b, "obj-1.bin", data)
	if ref.Size != int64(len(data)) {
		t.Errorf("ref.Size = %d, want %d", ref.Size, len(data))
	}

	rc, n, err := b.Get(context.Background(), ref, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("Get length = %d, want %d", n, len(data))
	}
	if got := readAll(t, rc); !bytes.Equal(got, data) {
		t.Error("round trip changed the bytes")
	}
}

// Rules.md §2.7: the declared size is a claim, verified server-side, and a
// mismatch leaves nothing behind.
func TestPutRejectsSizeMismatch(t *testing.T) {
	tests := []struct {
		name     string
		declared int64
		actual   int
	}{
		{"declared larger than sent", 100, 50},
		{"declared smaller than sent", 10, 50},
		{"declared zero, bytes sent", 0, 50},
		{"negative size", -1, 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend(t)
			_, err := b.Put(context.Background(),
				bytes.NewReader(make([]byte, tc.actual)),
				storage.ObjectSpec{Name: "obj.bin", Size: tc.declared})
			if !errors.Is(err, storage.ErrSizeMismatch) {
				t.Fatalf("Put() = %v, want ErrSizeMismatch", err)
			}
		})
	}
}

func TestPutLeavesNoPartialObjectOnFailure(t *testing.T) {
	dir := t.TempDir()
	b, err := local.New(dir)
	if err != nil {
		t.Fatalf("local.New() = %v", err)
	}

	_, err = b.Put(context.Background(), bytes.NewReader(make([]byte, 10)),
		storage.ObjectSpec{Name: "obj.bin", Size: 999})
	if err == nil {
		t.Fatal("Put() succeeded with a wrong declared size")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() = %v", err)
	}
	for _, e := range entries {
		t.Errorf("leftover file after a failed Put: %s", e.Name())
	}
}

func TestPutIsCancelledByContext(t *testing.T) {
	b := newBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Put(ctx, bytes.NewReader(make([]byte, 1<<20)),
		storage.ObjectSpec{Name: "obj.bin", Size: 1 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put() = %v, want context.Canceled", err)
	}
}

func TestGetRange(t *testing.T) {
	b := newBackend(t)
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	ref := put(t, b, "obj.bin", data)

	tests := []struct {
		name   string
		rng    storage.ByteRange
		want   string
		errIs  error
		length int64
	}{
		{name: "from the start", rng: storage.ByteRange{Start: 0, Length: 5}, want: "01234", length: 5},
		{name: "middle", rng: storage.ByteRange{Start: 10, Length: 6}, want: "abcdef", length: 6},
		{name: "to the end", rng: storage.ByteRange{Start: 30, Length: 6}, want: "uvwxyz", length: 6},
		{name: "length past the end is clamped", rng: storage.ByteRange{Start: 30, Length: 1000}, want: "uvwxyz", length: 6},
		{name: "start past the end", rng: storage.ByteRange{Start: 100, Length: 5}, errIs: storage.ErrRangeNotSat},
		{name: "negative start", rng: storage.ByteRange{Start: -1, Length: 5}, errIs: storage.ErrRangeNotSat},
		{name: "zero length", rng: storage.ByteRange{Start: 0, Length: 0}, errIs: storage.ErrRangeNotSat},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, n, err := b.Get(context.Background(), ref, &tc.rng)
			if tc.errIs != nil {
				if !errors.Is(err, tc.errIs) {
					t.Fatalf("Get() = %v, want %v", err, tc.errIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get() = %v", err)
			}
			if n != tc.length {
				t.Errorf("length = %d, want %d", n, tc.length)
			}
			if got := string(readAll(t, rc)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetMissingObject(t *testing.T) {
	b := newBackend(t)
	_, _, err := b.Get(context.Background(), storage.ObjectRef{ProviderID: "nope.bin"}, nil)
	if !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("Get() = %v, want ErrObjectNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	b := newBackend(t)
	ref := put(t, b, "obj.bin", []byte("hello"))

	for i := 0; i < 3; i++ {
		if err := b.Delete(context.Background(), ref); err != nil {
			t.Fatalf("Delete() call %d = %v", i+1, err)
		}
	}
	if _, _, err := b.Get(context.Background(), ref, nil); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("Get() after Delete = %v, want ErrObjectNotFound", err)
	}
}

// Object names are generated by Skein, not by users, but a traversal check
// costs one line and closes the class outright.
func TestObjectNamesCannotEscapeTheRoot(t *testing.T) {
	dir := t.TempDir()
	b, err := local.New(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("local.New() = %v", err)
	}

	bad := []string{
		"", ".", "..", "../escape", "sub/obj.bin",
		"/etc/passwd", "a\x00b", strings.Repeat("../", 8) + "etc/passwd",
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := b.Put(context.Background(), bytes.NewReader(nil),
				storage.ObjectSpec{Name: name, Size: 0})
			if err == nil {
				t.Fatalf("Put(%q) succeeded", name)
			}
			if _, _, err := b.Get(context.Background(),
				storage.ObjectRef{ProviderID: name}, nil); err == nil {
				t.Fatalf("Get(%q) succeeded", name)
			}
		})
	}
}

func TestQuotaWithFakeCapacity(t *testing.T) {
	const capacity = 1000
	b := newBackend(t, local.WithFakeCapacity(capacity))

	q, err := b.Quota(context.Background())
	if err != nil {
		t.Fatalf("Quota() = %v", err)
	}
	if q.TotalBytes != capacity || q.UsedBytes != 0 {
		t.Fatalf("Quota() = %+v, want total %d used 0", q, capacity)
	}
	if q.FreeBytes() != capacity {
		t.Errorf("FreeBytes() = %d, want %d", q.FreeBytes(), capacity)
	}

	ref := put(t, b, "obj.bin", make([]byte, 600))
	q, err = b.Quota(context.Background())
	if err != nil {
		t.Fatalf("Quota() = %v", err)
	}
	if q.UsedBytes != 600 {
		t.Errorf("UsedBytes = %d, want 600", q.UsedBytes)
	}

	// A write that would exceed capacity is refused, not truncated.
	_, err = b.Put(context.Background(), bytes.NewReader(make([]byte, 500)),
		storage.ObjectSpec{Name: "obj2.bin", Size: 500})
	if !errors.Is(err, storage.ErrQuota) {
		t.Fatalf("Put() over capacity = %v, want ErrQuota", err)
	}

	if err := b.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	q, err = b.Quota(context.Background())
	if err != nil {
		t.Fatalf("Quota() = %v", err)
	}
	if q.UsedBytes != 0 {
		t.Errorf("UsedBytes after delete = %d, want 0", q.UsedBytes)
	}
}

func TestQuotaFreeBytesNeverNegative(t *testing.T) {
	q := storage.Quota{TotalBytes: 100, UsedBytes: 250}
	if got := q.FreeBytes(); got != 0 {
		t.Errorf("FreeBytes() = %d, want 0", got)
	}
}

func TestNameForShard(t *testing.T) {
	got := storage.NameForShard("abc-123", 7)
	if got != "skein-abc-123-0007.bin" {
		t.Errorf("NameForShard() = %q", got)
	}
	// Ordering matters for reassembly, so the index must be zero-padded.
	if storage.NameForShard("f", 2) >= storage.NameForShard("f", 10) {
		t.Error("shard names do not sort by index")
	}
}

func TestKind(t *testing.T) {
	if got := newBackend(t).Kind(); got != storage.KindLocal {
		t.Errorf("Kind() = %q, want %q", got, storage.KindLocal)
	}
}
