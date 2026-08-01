package files_test

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/storage"
)

// memoryCeilingBytes is the promise this project makes.
//
// Phases.md §3: "upload 2 GiB, assert peak HeapAlloc stays under 150 MB. This
// test may never be relaxed." If a change makes this fail, the change is
// wrong — raising the number is not a fix, it is an admission that the product
// thesis no longer holds.
const memoryCeilingBytes = 150 << 20

// uploadSizeBytes is the file size the ceiling is measured against.
const uploadSizeBytes = 2 << 30 // 2 GiB

// zeroReader produces bytes without allocating. Using bytes.Repeat here would
// mean the test itself held 2 GiB, which would prove nothing about the code
// under test.
type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > z.remaining {
		n = z.remaining
	}
	// p is reused by io.Copy; it does not need clearing, and clearing it
	// would only measure memset speed.
	z.remaining -= n
	return int(n), nil
}

// discardBackend accepts bytes and throws them away.
//
// The ceiling is a property of Skein's own code path — multipart reader,
// TeeReader, shard writer, io.CopyBuffer — not of whichever provider is
// underneath. Writing 2 GiB to a temp file would measure disk, and pointing at
// a real provider would measure the network. This isolates the thing being
// asserted.
type discardBackend struct{}

func (discardBackend) Put(ctx context.Context, r io.Reader, spec storage.ObjectSpec) (storage.ObjectRef, error) {
	buf := make([]byte, storage.CopyBufferSize)
	n, err := io.CopyBuffer(io.Discard, r, buf)
	if err != nil {
		return storage.ObjectRef{}, err
	}
	if n != spec.Size {
		return storage.ObjectRef{}, storage.ErrSizeMismatch
	}
	return storage.ObjectRef{ProviderID: spec.Name, Size: n}, nil
}

func (discardBackend) Get(context.Context, storage.ObjectRef, *storage.ByteRange) (io.ReadCloser, int64, error) {
	return io.NopCloser(io.LimitReader(&zeroReader{remaining: 0}, 0)), 0, nil
}
func (discardBackend) Delete(context.Context, storage.ObjectRef) error { return nil }
func (discardBackend) Quota(context.Context) (storage.Quota, error) {
	return storage.Quota{TotalBytes: 1 << 50}, nil
}
func (discardBackend) Kind() storage.Kind { return storage.KindLocal }

// TestUploadHoldsConstantMemory is the product thesis as an assertion.
//
// It streams 2 GiB through the real upload path and checks that peak HeapAlloc
// never approaches the file size. A regression that reintroduces buffering —
// an io.ReadAll, a bytes.Buffer accumulating the body, a []byte sized from
// Content-Length — fails here rather than in production on someone's 512 MB
// VPS.
func TestUploadHoldsConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the 2 GiB memory ceiling test in -short mode")
	}

	f := newFixtureWithBackend(t, discardBackend{})

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// Sampled from a goroutine during the upload: peak matters, and reading
	// only at the end would miss a spike that has already been collected.
	done := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		var highest uint64
		var m runtime.MemStats
		// ReadMemStats stops the world, so sampling in a tight loop would
		// make this test measure its own instrumentation. A millisecond
		// still gives thousands of samples across the transfer, which is
		// far more than enough to catch a buffer proportional to a 2 GiB
		// body.
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > highest {
					highest = m.HeapAlloc
				}
				peak <- highest
				return
			case <-ticker.C:
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > highest {
					highest = m.HeapAlloc
				}
			}
		}
	}()

	file, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID,
		Name:   "huge.bin",
		Size:   uploadSizeBytes,
	}, &zeroReader{remaining: uploadSizeBytes})

	close(done)
	highest := <-peak

	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}
	if file.SizeBytes != uploadSizeBytes {
		t.Fatalf("SizeBytes = %d, want %d", file.SizeBytes, int64(uploadSizeBytes))
	}

	t.Logf("uploaded %d bytes; peak HeapAlloc %d bytes (%.1f MiB), ceiling %d MiB",
		int64(uploadSizeBytes), highest, float64(highest)/(1<<20), memoryCeilingBytes>>20)

	if highest > memoryCeilingBytes {
		t.Fatalf("peak HeapAlloc was %.1f MiB, over the %d MiB ceiling. "+
			"Something on the upload path is buffering; do not raise this number.",
			float64(highest)/(1<<20), memoryCeilingBytes>>20)
	}
}

// TestDownloadHoldsConstantMemory is the same promise for the read path.
func TestDownloadHoldsConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the memory ceiling test in -short mode")
	}

	f := newFixture(t)
	const size = 256 << 20 // 256 MiB is enough to catch a full-body buffer

	file, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "big.bin", Size: size,
	}, &zeroReader{remaining: size})
	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}

	runtime.GC()

	content, err := f.svc.Open(context.Background(), f.userID, file.ID, nil)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = content.Body.Close() }()

	var highest uint64
	var m runtime.MemStats
	buf := make([]byte, storage.CopyBufferSize)
	var total int64

	for {
		n, rerr := content.Body.Read(buf)
		total += int64(n)
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > highest {
			highest = m.HeapAlloc
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("Read() = %v", rerr)
		}
	}

	if total != size {
		t.Fatalf("read %d bytes, want %d", total, int64(size))
	}
	t.Logf("read %d bytes; peak HeapAlloc %.1f MiB", total, float64(highest)/(1<<20))

	if highest > memoryCeilingBytes {
		t.Fatalf("peak HeapAlloc was %.1f MiB while reading %d MiB; the download path is buffering",
			float64(highest)/(1<<20), size>>20)
	}
}

// BenchmarkUploadThroughput measures the streaming path with allocations
// reported, so a regression that starts allocating per megabyte is visible as
// a number rather than only as a ceiling breach.
func BenchmarkUploadThroughput(b *testing.B) {
	f := newFixtureForBench(b, discardBackend{})
	const size = 64 << 20

	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := f.svc.Upload(context.Background(), files.UploadRequest{
			UserID: f.userID,
			Name:   "bench.bin",
			Size:   size,
		}, &zeroReader{remaining: size})
		if err != nil {
			b.Fatalf("Upload() = %v", err)
		}
	}
}
