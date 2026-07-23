// Package local implements storage.Backend over the filesystem.
//
// It is two things at once: the test double the whole suite runs against, so
// no unit test touches the network, and a legitimate deployment option for
// someone who wants Skein's striping and encryption over disks they already
// own.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/mridul60214/skein/internal/storage"
)

// Backend stores objects as files under a root directory.
type Backend struct {
	root string

	// capacity, when non-zero, overrides the filesystem's real free space.
	// Tests use it to provoke the out-of-space paths without filling a disk.
	capacity int64

	// used tracks bytes written when capacity is set.
	used atomic.Int64
}

// Option configures a Backend.
type Option func(*Backend)

// WithFakeCapacity makes Quota report a fixed total instead of the real
// filesystem, so quota exhaustion and shard planning can be tested on a laptop.
func WithFakeCapacity(total int64) Option {
	return func(b *Backend) { b.capacity = total }
}

// New opens a local backend rooted at dir, creating it if needed.
func New(dir string, opts ...Option) (*Backend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	b := &Backend{root: abs}
	for _, o := range opts {
		o(b)
	}
	if b.capacity > 0 {
		if err := b.recountUsed(); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Kind identifies this implementation.
func (b *Backend) Kind() storage.Kind { return storage.KindLocal }

// Put streams r to a file, verifying the byte count against spec.Size.
//
// The write goes to a temporary file and is renamed into place only after the
// count matches, so a failed or truncated upload never leaves something that
// looks like a complete object.
func (b *Backend) Put(ctx context.Context, r io.Reader, spec storage.ObjectSpec) (storage.ObjectRef, error) {
	if spec.Size < 0 {
		return storage.ObjectRef{}, fmt.Errorf("%w: negative size", storage.ErrSizeMismatch)
	}
	name, err := b.safePath(spec.Name)
	if err != nil {
		return storage.ObjectRef{}, err
	}
	if b.capacity > 0 && b.used.Load()+spec.Size > b.capacity {
		return storage.ObjectRef{}, storage.ErrQuota
	}

	tmp, err := os.CreateTemp(b.root, ".upload-*")
	if err != nil {
		return storage.ObjectRef{}, fmt.Errorf("create temp object: %w", err)
	}
	tmpName := tmp.Name()
	// Cleanup runs on every error path. On success the file has already
	// been renamed away, so the remove is a harmless no-op.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// Fixed buffer, per Rules.md §2.1. Nothing here grows with file size.
	buf := make([]byte, storage.CopyBufferSize)
	written, err := io.CopyBuffer(tmp, &ctxReader{ctx: ctx, r: r}, buf)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return storage.ObjectRef{}, storage.ErrQuota
		}
		return storage.ObjectRef{}, fmt.Errorf("write object: %w", err)
	}

	// Rules.md §2.7: the declared size is a claim, checked here against
	// what actually arrived.
	if written != spec.Size {
		return storage.ObjectRef{}, fmt.Errorf("%w: declared %d, received %d",
			storage.ErrSizeMismatch, spec.Size, written)
	}
	if err := tmp.Sync(); err != nil {
		return storage.ObjectRef{}, fmt.Errorf("sync object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return storage.ObjectRef{}, fmt.Errorf("close object: %w", err)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return storage.ObjectRef{}, fmt.Errorf("commit object: %w", err)
	}
	if b.capacity > 0 {
		b.used.Add(written)
	}

	return storage.ObjectRef{ProviderID: filepath.Base(name), Size: written}, nil
}

// Get opens the object, optionally seeking to a range.
func (b *Backend) Get(_ context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, int64, error) {
	name, err := b.safePath(ref.ProviderID)
	if err != nil {
		return nil, 0, err
	}
	//nolint:gosec // G304: safePath has already confined name to the root.
	f, err := os.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, storage.ErrObjectNotFound
		}
		return nil, 0, fmt.Errorf("open object: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat object: %w", err)
	}
	size := info.Size()

	if rng == nil {
		return f, size, nil
	}

	if rng.Start < 0 || rng.Start >= size || rng.Length <= 0 {
		_ = f.Close()
		return nil, 0, storage.ErrRangeNotSat
	}
	if _, err := f.Seek(rng.Start, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("seek object: %w", err)
	}
	length := min(rng.Length, size-rng.Start)

	return readCloser{Reader: io.LimitReader(f, length), Closer: f}, length, nil
}

// Delete removes an object. A missing object is not an error.
func (b *Backend) Delete(_ context.Context, ref storage.ObjectRef) error {
	name, err := b.safePath(ref.ProviderID)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(name)
	if err := os.Remove(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("delete object: %w", err)
	}
	if b.capacity > 0 && statErr == nil {
		b.used.Add(-info.Size())
	}
	return nil
}

// Quota reports capacity. With a fake capacity set it reports that; otherwise
// it reports the real filesystem.
func (b *Backend) Quota(_ context.Context) (storage.Quota, error) {
	if b.capacity > 0 {
		return storage.Quota{TotalBytes: b.capacity, UsedBytes: b.used.Load()}, nil
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(b.root, &stat); err != nil {
		return storage.Quota{}, fmt.Errorf("statfs %s: %w", b.root, err)
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return storage.Quota{TotalBytes: total, UsedBytes: total - free}, nil
}

// safePath resolves an object name inside the root and refuses anything that
// escapes it. Object names are constructed by Skein rather than by users, but
// a traversal check here is one line and removes the whole class.
func (b *Backend) safePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, os.PathSeparator) ||
		name == "." || name == ".." || strings.Contains(name, "\x00") {
		return "", fmt.Errorf("%w: bad object name", storage.ErrObjectNotFound)
	}
	full := filepath.Join(b.root, name)
	if filepath.Dir(full) != b.root {
		return "", fmt.Errorf("%w: bad object name", storage.ErrObjectNotFound)
	}
	return full, nil
}

func (b *Backend) recountUsed() error {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		return fmt.Errorf("scan storage root: %w", err)
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // raced with a delete; it is not ours to account for
		}
		total += info.Size()
	}
	b.used.Store(total)
	return nil
}

// ctxReader makes a plain io.Reader cancellable, so a client disconnect stops
// the copy instead of writing the rest of a file nobody is waiting for.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Compile-time check that the double still implements the interface.
var _ storage.Backend = (*Backend)(nil)
