package files

import (
	"context"
	"io"
	"sync"
	"time"
)

// DownloadProgress is one progress sample, as emitted to the UI.
type DownloadProgress struct {
	TransferID string `json:"transfer_id"`
	FileID     string `json:"file_id"`
	Name       string `json:"name"`
	// Done is bytes transferred so far; Total is the file size, or 0 when
	// unknown.
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
	// BytesPerSec is a smoothed rate. Zero until enough time has passed to
	// measure one, rather than a wild figure derived from a few microseconds.
	BytesPerSec int64 `json:"bytes_per_sec"`
	// ETASeconds is remaining time at the current rate. -1 when it cannot be
	// estimated, which the UI must render as "--" rather than as 0.
	ETASeconds int64 `json:"eta_seconds"`
	// Done transfers emit exactly one final sample with Complete set.
	Complete bool `json:"complete"`
	// Err is a user-safe message, empty unless the transfer failed.
	Err string `json:"error,omitempty"`
}

// ProgressSink receives samples. The desktop binary supplies one backed by
// wails.EventsEmit; tests supply a recorder.
//
// Kept as a function rather than importing Wails here so this package builds
// on the server too, where Wails is not a dependency at all.
type ProgressSink func(DownloadProgress)

// progressInterval is the floor between emitted samples.
//
// A 256 KiB buffer on a fast link produces thousands of Read calls a second.
// Emitting per chunk floods the webview's event bridge and makes the UI
// jankier the faster the transfer goes. 150ms is inside the 100–250ms band
// that reads as smooth without swamping the bridge.
const progressInterval = 150 * time.Millisecond

// ProgressReader wraps a download stream and reports how far along it is.
//
// Cancellation is the caller's context, not a flag here: Read returns the
// context error as soon as it is cancelled, which propagates up through the
// copy loop and closes the underlying provider stream. That is what actually
// stops the transfer — hiding a card in the UI would leave the bytes flowing.
type ProgressReader struct {
	r      io.Reader
	ctx    context.Context
	sink   ProgressSink
	sample DownloadProgress

	mu        sync.Mutex
	done      int64
	started   time.Time
	lastEmit  time.Time
	lastDone  int64
	rate      float64
	finalised bool

	now func() time.Time
}

// NewProgressReader wraps r. total may be 0 when the size is unknown.
func NewProgressReader(ctx context.Context, r io.Reader, sink ProgressSink, meta DownloadProgress) *ProgressReader {
	now := time.Now
	return &ProgressReader{
		r:        r,
		ctx:      ctx,
		sink:     sink,
		sample:   meta,
		started:  now(),
		lastEmit: now(),
		now:      now,
	}
}

// Read implements io.Reader, counting bytes and emitting throttled samples.
func (p *ProgressReader) Read(b []byte) (int, error) {
	// Cancellation is checked before the read so a cancelled transfer stops
	// promptly rather than after one more provider round trip.
	if err := p.ctx.Err(); err != nil {
		p.finish(err)
		return 0, err
	}

	n, err := p.r.Read(b)
	if n > 0 {
		p.mu.Lock()
		p.done += int64(n)
		p.mu.Unlock()
		p.maybeEmit(false)
	}

	if err != nil {
		p.finish(err)
	}
	return n, err
}

// maybeEmit sends a sample if the throttle interval has elapsed, or if force
// is set (the terminal sample, which must never be dropped).
func (p *ProgressReader) maybeEmit(force bool) {
	p.mu.Lock()

	now := p.now()
	elapsed := now.Sub(p.lastEmit)
	if !force && elapsed < progressInterval {
		p.mu.Unlock()
		return
	}

	// Exponentially smoothed rate. An instantaneous figure swings wildly on a
	// bursty link and makes the ETA unreadable.
	if elapsed > 0 {
		instant := float64(p.done-p.lastDone) / elapsed.Seconds()
		if p.rate == 0 {
			p.rate = instant
		} else {
			p.rate = 0.7*p.rate + 0.3*instant
		}
	}
	p.lastEmit = now
	p.lastDone = p.done

	sample := p.sample
	sample.Done = p.done
	sample.BytesPerSec = int64(p.rate)

	// ETA only when there is a rate and a known total. -1 means "unknown",
	// which the UI renders as "--"; 0 would read as "finished".
	sample.ETASeconds = -1
	if p.rate > 0 && sample.Total > p.done {
		sample.ETASeconds = int64(float64(sample.Total-p.done) / p.rate)
	}
	sink := p.sink
	p.mu.Unlock()

	if sink != nil {
		sink(sample)
	}
}

// finish emits exactly one terminal sample, whatever path the transfer ended
// on. A UI that never receives a terminal event leaves a card spinning
// forever.
func (p *ProgressReader) finish(cause error) {
	p.mu.Lock()
	if p.finalised {
		p.mu.Unlock()
		return
	}
	p.finalised = true

	sample := p.sample
	sample.Done = p.done
	sample.BytesPerSec = int64(p.rate)
	sample.ETASeconds = 0

	switch {
	case cause == nil || cause == io.EOF:
		sample.Complete = true
	case p.ctx.Err() != nil:
		// Cancelled by the user. Not an error to report as a failure.
		sample.Err = "Cancelled."
	default:
		sample.Err = "The download failed. Check your connection and try again."
	}
	sink := p.sink
	p.mu.Unlock()

	if sink != nil {
		sink(sample)
	}
}

// Done reports bytes transferred so far.
func (p *ProgressReader) Done() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}
