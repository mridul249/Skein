package files_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/router"
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/local"
)

// Reproduction harness for the reservation-leak incident: a 15 GB upload
// aborted at ~5% by navigating away, after which a retry was refused with
// "Needs 15.0 GB. 13.0 GB free across 2 drives." against 28 GB genuinely free.
// 28 - 15 = 13, so the whole reservation was still held.
//
// Two wrappers below exist because the in-memory doubles ignore ctx entirely,
// and a harness that ignores cancellation cannot show anything about
// cancellation. Postgres and Google Drive both fail immediately on a cancelled
// context, so the doubles have to as well or the test proves nothing.

// ctxStore fails like Postgres does: any call carrying a cancelled context
// returns that context's error instead of doing the work.
//
// Without this the memory store would happily release a reservation on a dead
// context and every test here would pass regardless of which context the
// cleanup path actually used.
type ctxStore struct {
	inner    *router.MemoryStore
	releases atomic.Int64
	// releaseCtxCancelled records whether Release was ever handed a context
	// that was already cancelled. That is the mistake to catch: cleanup that
	// runs on the request context is cleanup that never runs.
	releaseCtxCancelled atomic.Bool
}

func (s *ctxStore) Candidates(ctx context.Context, userID uuid.UUID) ([]router.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.inner.Candidates(ctx, userID)
}

func (s *ctxStore) Reserve(ctx context.Context, accountID uuid.UUID, bytes int64, uploadID uuid.UUID, expiresAt time.Time) (router.Reservation, error) {
	if err := ctx.Err(); err != nil {
		return router.Reservation{}, err
	}
	return s.inner.Reserve(ctx, accountID, bytes, uploadID, expiresAt)
}

func (s *ctxStore) Release(ctx context.Context, uploadID uuid.UUID) (int64, error) {
	s.releases.Add(1)
	if err := ctx.Err(); err != nil {
		s.releaseCtxCancelled.Store(true)
		return 0, err
	}
	return s.inner.Release(ctx, uploadID)
}

func (s *ctxStore) ReclaimExpired(ctx context.Context) (int, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	return s.inner.ReclaimExpired(ctx)
}

// ctxBackend fails a Put on a cancelled context, as a real provider call does,
// and records deletes so orphan cleanup can be observed.
type ctxBackend struct {
	inner   *local.Backend
	deletes atomic.Int64
	// deleteCtxCancelled records cleanup attempted on a dead context, which
	// would leave the object at the provider forever.
	deleteCtxCancelled atomic.Bool
}

func (b *ctxBackend) Put(ctx context.Context, r io.Reader, spec storage.ObjectSpec) (storage.ObjectRef, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectRef{}, err
	}
	return b.inner.Put(ctx, r, spec)
}

func (b *ctxBackend) Get(ctx context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, int64, error) {
	return b.inner.Get(ctx, ref, rng)
}

func (b *ctxBackend) Delete(ctx context.Context, ref storage.ObjectRef) error {
	b.deletes.Add(1)
	if err := ctx.Err(); err != nil {
		b.deleteCtxCancelled.Store(true)
		return err
	}
	return b.inner.Delete(ctx, ref)
}

func (b *ctxBackend) Quota(ctx context.Context) (storage.Quota, error) { return b.inner.Quota(ctx) }
func (b *ctxBackend) Kind() storage.Kind                               { return b.inner.Kind() }

// abortFixture is the striping fixture with the two context-honouring wrappers
// in place.
type abortFixture struct {
	svc      *files.Service
	store    files.ConformanceStore
	router   *router.MemoryStore
	ctxStore *ctxStore
	backends map[uuid.UUID]*ctxBackend
	accounts []uuid.UUID
	userID   uuid.UUID
}

type abortResolver struct {
	backends map[uuid.UUID]*ctxBackend
}

func (m abortResolver) For(_ context.Context, _ uuid.UUID, accountID *uuid.UUID) (storage.Backend, error) {
	if accountID == nil {
		return nil, errors.New("no account for shard")
	}
	b, ok := m.backends[*accountID]
	if !ok {
		return nil, errors.New("drive not connected")
	}
	return b, nil
}

func newAbortFixture(t *testing.T, drives int, capacityEach, shardSize int64) *abortFixture {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	mem := router.NewMemoryStore()
	wrapped := &ctxStore{inner: mem}
	backends := map[uuid.UUID]*ctxBackend{}
	ids := make([]uuid.UUID, 0, drives)

	for i := 0; i < drives; i++ {
		id := uuid.New()
		ids = append(ids, id)
		mem.AddAccount(id, int32(i+1), "drive@example.com", capacityEach, 0)

		b, berr := local.New(t.TempDir(), local.WithFakeCapacity(capacityEach))
		if berr != nil {
			t.Fatalf("local.New() = %v", berr)
		}
		backends[id] = &ctxBackend{inner: b}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reserver := router.NewReserver(wrapped, logger)
	planner := router.NewPlanner(reserver, router.PolicyRoundRobin, shardSize, skcrypto.StreamOverhead)

	store := files.NewConformanceStore(t)
	svc := files.NewService(
		store,
		files.NewStripingPlanner(planner, reserver),
		abortResolver{backends},
		ring,
		files.Config{Encrypt: true, MaxUploadBytes: 1 << 40},
		logger,
	)

	return &abortFixture{
		svc:      svc,
		store:    store,
		router:   mem,
		ctxStore: wrapped,
		backends: backends,
		accounts: ids,
		userID:   uuid.New(),
	}
}

func (f *abortFixture) reservedTotal() int64 {
	var total int64
	for _, id := range f.accounts {
		total += f.router.ReservedOn(id)
	}
	return total
}

func (f *abortFixture) pooledFree(t *testing.T) int64 {
	t.Helper()
	cands, err := f.router.Candidates(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("Candidates() = %v", err)
	}
	return router.PooledFree(cands)
}

func (f *abortFixture) providerUsed(t *testing.T) int64 {
	t.Helper()
	var total int64
	for _, b := range f.backends {
		q, err := b.Quota(context.Background())
		if err != nil {
			t.Fatalf("Quota() = %v", err)
		}
		total += q.UsedBytes
	}
	return total
}

// cancelAfter cancels the upload's context partway through the body, the way
// closing a tab does. It keeps the reader honest afterwards: a disconnected
// client's body also stops yielding, so subsequent reads report the context
// error rather than politely returning EOF.
type cancelAfter struct {
	r      io.Reader
	after  int64
	cancel context.CancelFunc
	ctx    context.Context
	read   int64
	fired  bool
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.fired {
		if err := c.ctx.Err(); err != nil {
			return 0, err
		}
	}
	if c.read >= c.after && !c.fired {
		c.fired = true
		c.cancel()
		return 0, c.ctx.Err()
	}
	if remaining := c.after - c.read; int64(len(p)) > remaining && remaining > 0 {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

const (
	abortShardSize = 1 << 20
	// Twenty shards, aborted after one. The incident's shape: a large upload
	// whose whole reservation is taken up front and abandoned near the start.
	abortFileSize = 20 << 20
	abortAfter    = 1 << 20
)

// startAbortedUpload runs an upload that is cancelled after abortAfter bytes and
// returns the error it failed with.
func (f *abortFixture) startAbortedUpload(t *testing.T, name string) error {
	t.Helper()

	data := make([]byte, abortFileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := &cancelAfter{
		r:      newRepeatReader(data),
		after:  abortAfter,
		cancel: cancel,
		ctx:    ctx,
	}

	_, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID, Name: name, Size: abortFileSize,
	}, body)
	if err == nil {
		t.Fatal("Upload() succeeded despite its context being cancelled mid-stream")
	}
	return err
}

func newRepeatReader(data []byte) io.Reader { return &repeatReader{data: data} }

type repeatReader struct {
	data []byte
	pos  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// Known issue: the reservation held by an upload aborted through context
// cancellation must be given back. The whole plan is reserved before the first
// byte is written, so a 15 GB upload abandoned at 5% holds 15 GB until
// something releases it.
func TestAbortedUploadReleasesItsReservation(t *testing.T) {
	f := newAbortFixture(t, 2, 64<<20, abortShardSize)

	before := f.pooledFree(t)
	if err := f.startAbortedUpload(t, "aborted.bin"); err == nil {
		t.Fatal("expected the upload to fail")
	}

	if got := f.reservedTotal(); got != 0 {
		t.Errorf("%d bytes still reserved after an aborted upload, want 0", got)
	}
	if after := f.pooledFree(t); after != before {
		t.Errorf("pooled free space is %d after the abort, was %d before: %d bytes leaked",
			after, before, before-after)
	}
	if f.ctxStore.releases.Load() == 0 {
		t.Error("Release was never called on the abort path")
	}
	if f.ctxStore.releaseCtxCancelled.Load() {
		t.Error("Release was called with an already-cancelled context, so it could only fail")
	}
}

// Known issue: shards already committed to the provider when an upload aborts
// must be deleted, or they are orphans that consume quota forever and skew
// every later free-space calculation.
func TestAbortedUploadLeavesNoOrphanShards(t *testing.T) {
	f := newAbortFixture(t, 2, 64<<20, abortShardSize)

	if err := f.startAbortedUpload(t, "orphans.bin"); err == nil {
		t.Fatal("expected the upload to fail")
	}

	if used := f.providerUsed(t); used != 0 {
		t.Errorf("%d bytes of orphaned shards remain at the providers, want 0", used)
	}
	if n := len(f.store.ListShardsSnapshot()); n != 0 {
		t.Errorf("%d shard rows survived an aborted upload", n)
	}
	for id, b := range f.backends {
		if b.deleteCtxCancelled.Load() {
			t.Errorf("drive %s was sent a Delete on an already-cancelled context, "+
				"so the orphan could not have been removed", id)
		}
	}
}

// Known issue: free space reported to the user must be identical before and
// after an aborted upload. This is the number the incident's error message was
// computed from.
func TestAbortedUploadDoesNotChangeReportedFreeSpace(t *testing.T) {
	f := newAbortFixture(t, 2, 64<<20, abortShardSize)

	before := f.pooledFree(t)
	beforeUsed := f.providerUsed(t)

	if err := f.startAbortedUpload(t, "freespace.bin"); err == nil {
		t.Fatal("expected the upload to fail")
	}

	if after := f.pooledFree(t); after != before {
		t.Errorf("reported free space changed across an aborted upload: %d -> %d", before, after)
	}
	if after := f.providerUsed(t); after != beforeUsed {
		t.Errorf("provider used bytes changed across an aborted upload: %d -> %d", beforeUsed, after)
	}

	// And a retry of the same size must now be accepted. This is the exact
	// user-visible symptom: the retry was refused with a smaller free figure.
	data := make([]byte, abortFileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := f.svc.Upload(context.Background(), files.UploadRequest{
		UserID: f.userID, Name: "retry.bin", Size: abortFileSize,
	}, newRepeatReader(data)); err != nil {
		t.Errorf("retrying the same upload after an abort failed: %v", err)
	}
}

// Known issue: the itemised reservation ledger and the per-account counter must
// agree. If the abort path decremented the counter but left the rows, the
// janitor would later decrement a second time; if it deleted the rows but not
// the counter, the capacity is stranded until someone edits the database.
//
// PARTIAL ASSERTION — read this before trusting it. It can only catch
// divergence in rows that are already past expiry, because the janitor ignores
// live ones and the upload id is generated inside Upload, so the test cannot
// reach it to backdate it. A release that credited the counter but left an
// unexpired row would pass here and only surface 30 minutes later as a double
// credit.
//
// Closing that gap needs a `HeldBytes(uploadID)` accessor on router.MemoryStore,
// deliberately not added: it is in the package being rewritten, and the rewrite
// should decide whether the counter and the row ledger stay as two things at all.
func TestAbortedUploadLeavesTheTwoLedgersInAgreement(t *testing.T) {
	f := newAbortFixture(t, 2, 64<<20, abortShardSize)

	if err := f.startAbortedUpload(t, "ledgers.bin"); err == nil {
		t.Fatal("expected the upload to fail")
	}

	reservedAfterAbort := f.reservedTotal()

	count, bytes, err := f.router.ReclaimExpired(context.Background())
	if err != nil {
		t.Fatalf("ReclaimExpired() = %v", err)
	}
	if count != 0 || bytes != 0 {
		t.Errorf("the janitor reclaimed %d reservations totalling %d bytes after an abort "+
			"that should already have released everything: the row ledger and the counter disagree",
			count, bytes)
	}
	if got := f.reservedTotal(); got != reservedAfterAbort {
		t.Errorf("reclaiming moved the counter from %d to %d, so rows outlived the release",
			reservedAfterAbort, got)
	}
}

// Known issue #2 of the four: expires_at exists on reservations, but something
// has to sweep it. Without a reaper a killed process strands capacity
// permanently.
func TestExpiredReservationsAreReclaimed(t *testing.T) {
	f := newAbortFixture(t, 2, 64<<20, abortShardSize)

	// Reserve directly and strand it, which is what a killed process leaves.
	reserver := router.NewReserver(f.router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	uploadID := uuid.New()
	if _, err := reserver.Reserve(context.Background(), f.accounts[0], 8<<20, uploadID); err != nil {
		t.Fatalf("Reserve() = %v", err)
	}
	if got := f.reservedTotal(); got != 8<<20 {
		t.Fatalf("reserved = %d, want %d", got, int64(8<<20))
	}

	// Before expiry the janitor must leave it alone: a live slow upload still
	// needs its capacity.
	count, _, err := f.router.ReclaimExpired(context.Background())
	if err != nil {
		t.Fatalf("ReclaimExpired() = %v", err)
	}
	if count != 0 {
		t.Errorf("the janitor reclaimed %d unexpired reservations", count)
	}
	if got := f.reservedTotal(); got != 8<<20 {
		t.Errorf("reserved = %d after reclaiming nothing, want %d", got, int64(8<<20))
	}

	// Past expiry it must be swept.
	f.router.Expire(uploadID)
	count, bytes, err := f.router.ReclaimExpired(context.Background())
	if err != nil {
		t.Fatalf("ReclaimExpired() = %v", err)
	}
	if count != 1 {
		t.Errorf("the janitor reclaimed %d expired reservations, want 1", count)
	}
	if bytes != 8<<20 {
		t.Errorf("the janitor reclaimed %d bytes, want %d", bytes, int64(8<<20))
	}
	if got := f.reservedTotal(); got != 0 {
		t.Errorf("%d bytes still reserved after the janitor ran, want 0", got)
	}
}
