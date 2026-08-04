package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mridul249/Skein/internal/files"
)

// recorder collects samples.
type recorder struct {
	mu      sync.Mutex
	samples []files.DownloadProgress
}

func (r *recorder) sink(p files.DownloadProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, p)
}

func (r *recorder) all() []files.DownloadProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]files.DownloadProgress(nil), r.samples...)
}

// THE THROTTLE IS THE POINT. A 256 KiB buffer on a fast link produces
// thousands of Read calls a second; emitting per chunk floods the webview's
// event bridge and makes the UI jankier the faster the transfer goes.
//
// 4 MiB through a 4 KiB buffer is 1024 reads. Without throttling that is 1024
// events.
func TestProgressEmissionIsThrottledNotPerChunk(t *testing.T) {
	const size = 4 << 20
	rec := &recorder{}

	pr := files.NewProgressReader(context.Background(),
		bytes.NewReader(make([]byte, size)), rec.sink,
		files.DownloadProgress{TransferID: "t1", Total: size})

	buf := make([]byte, 4096)
	reads := 0
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			reads++
		}
		if err != nil {
			break
		}
	}

	samples := rec.all()
	if reads < 500 {
		t.Fatalf("only %d reads; the test is not exercising the throttle", reads)
	}
	if len(samples) >= reads {
		t.Errorf("%d samples for %d reads; emission is per-chunk, not throttled",
			len(samples), reads)
	}
	// A fast in-memory transfer should collapse to very few samples.
	if len(samples) > 20 {
		t.Errorf("%d samples for a single fast transfer; the throttle is too loose",
			len(samples))
	}
}

// Exactly one terminal sample, whatever happens. A UI that never receives one
// leaves a card spinning forever.
func TestProgressAlwaysEndsWithExactlyOneTerminalSample(t *testing.T) {
	const size = 512 << 10
	rec := &recorder{}

	pr := files.NewProgressReader(context.Background(),
		bytes.NewReader(make([]byte, size)), rec.sink,
		files.DownloadProgress{TransferID: "t1", Total: size})

	if _, err := io.Copy(io.Discard, pr); err != nil {
		t.Fatalf("Copy() = %v", err)
	}

	samples := rec.all()
	if len(samples) == 0 {
		t.Fatal("no samples emitted at all")
	}

	terminal := 0
	for _, s := range samples {
		if s.Complete || s.Err != "" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Errorf("%d terminal samples, want exactly 1", terminal)
	}

	last := samples[len(samples)-1]
	if !last.Complete {
		t.Errorf("last sample is not Complete: %+v", last)
	}
	if last.Done != size {
		t.Errorf("final Done = %d, want %d", last.Done, size)
	}
	if last.Err != "" {
		t.Errorf("a successful transfer reported an error: %q", last.Err)
	}
}

// CANCEL MUST ACTUALLY STOP THE TRANSFER, not merely hide a card. The reader
// stops returning bytes and reports the context error, which propagates up the
// copy loop and closes the provider stream.
func TestCancelStopsTheTransfer(t *testing.T) {
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())

	// A reader that would otherwise never end.
	endless := &countingEndlessReader{}
	pr := files.NewProgressReader(ctx, endless, rec.sink,
		files.DownloadProgress{TransferID: "t1", Total: 0})

	buf := make([]byte, 4096)
	for i := 0; i < 10; i++ {
		if _, err := pr.Read(buf); err != nil {
			t.Fatalf("early read failed: %v", err)
		}
	}
	readsBeforeCancel := endless.count()

	cancel()

	// The very next read must refuse.
	if _, err := pr.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read() after cancel = %v, want context.Canceled", err)
	}
	// And the underlying stream is not touched again — this is the difference
	// between cancelling and hiding.
	for i := 0; i < 5; i++ {
		_, _ = pr.Read(buf)
	}
	if after := endless.count(); after != readsBeforeCancel {
		t.Errorf("underlying reader advanced %d -> %d after cancellation; "+
			"the transfer did not actually stop", readsBeforeCancel, after)
	}

	// The UI is told, and it is not reported as a failure.
	samples := rec.all()
	last := samples[len(samples)-1]
	if last.Err != "Cancelled." {
		t.Errorf("terminal sample after cancel = %+v, want a cancellation", last)
	}
	if last.Complete {
		t.Error("a cancelled transfer was reported as Complete")
	}
}

// A mid-stream failure surfaces as a user-safe error, and the provider's own
// message does not leak into the UI.
func TestProgressReportsAMidStreamFailure(t *testing.T) {
	rec := &recorder{}
	failing := io.MultiReader(
		bytes.NewReader(make([]byte, 8192)),
		&erroringReader{err: errors.New("tls: connection reset by peer at 10.0.0.1")},
	)

	pr := files.NewProgressReader(context.Background(), failing, rec.sink,
		files.DownloadProgress{TransferID: "t1", Total: 1 << 20})

	if _, err := io.Copy(io.Discard, pr); err == nil {
		t.Fatal("Copy() = nil, want the underlying failure")
	}

	samples := rec.all()
	if len(samples) == 0 {
		t.Fatal("no samples emitted")
	}
	last := samples[len(samples)-1]
	if last.Err == "" {
		t.Error("a failed transfer emitted no error")
	}
	if last.Complete {
		t.Error("a failed transfer was reported Complete")
	}
	if bytes.Contains([]byte(last.Err), []byte("10.0.0.1")) {
		t.Errorf("the error leaked provider detail to the UI: %q", last.Err)
	}
}

// ETA is -1 when it cannot be estimated. Zero would read as "finished".
func TestProgressReportsUnknownETAAsMinusOne(t *testing.T) {
	rec := &recorder{}
	// Total 0 means the size is unknown.
	pr := files.NewProgressReader(context.Background(),
		bytes.NewReader(make([]byte, 256<<10)), rec.sink,
		files.DownloadProgress{TransferID: "t1", Total: 0})

	if _, err := io.Copy(io.Discard, pr); err != nil {
		t.Fatalf("Copy() = %v", err)
	}
	for _, s := range rec.all() {
		if s.Complete {
			continue
		}
		if s.ETASeconds > 0 {
			t.Errorf("ETA = %d with an unknown total; want -1", s.ETASeconds)
		}
	}
}

// countingEndlessReader never ends, and records how often it was read.
type countingEndlessReader struct {
	mu sync.Mutex
	n  int
}

func (r *countingEndlessReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	time.Sleep(time.Millisecond)
	return len(p), nil
}

func (r *countingEndlessReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

type erroringReader struct{ err error }

func (r *erroringReader) Read([]byte) (int, error) { return 0, r.err }
