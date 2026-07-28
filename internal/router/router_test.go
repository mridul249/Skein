package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/skerr"
)

const (
	gib      = int64(1) << 30
	fifteenG = 15 * gib
	shard256 = 256 << 20
)

func newReserver(t *testing.T) (*Reserver, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	return NewReserver(store, slog.New(slog.NewTextHandler(io.Discard, nil))), store
}

// noOverhead keeps the planner arithmetic readable in tests that are about
// placement rather than about encryption expansion.
func noOverhead(n int64) int64 { return n }

func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()

	// Room for exactly ten 1 GiB reservations.
	store.AddAccount(acct, 1, "a@example.com", 10*gib, 0)

	const attempts = 50
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Reserve(context.Background(), acct, gib, uuid.New())
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
				return
			}
			if !errors.Is(err, ErrNoCapacity) {
				t.Errorf("Reserve() = %v, want nil or ErrNoCapacity", err)
			}
		}()
	}
	wg.Wait()

	// This is the whole point of Architecture.md §5. Read-then-write would
	// let more than ten through here.
	if succeeded != 10 {
		t.Fatalf("%d reservations succeeded against 10 GiB of space, want exactly 10", succeeded)
	}
	if got := store.ReservedOn(acct); got != 10*gib {
		t.Errorf("reserved = %d, want %d", got, 10*gib)
	}
}

func TestReserveRefusesWhenFull(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", 100, 100)

	if _, err := r.Reserve(context.Background(), acct, 1, uuid.New()); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Reserve() = %v, want ErrNoCapacity", err)
	}
}

func TestReserveRejectsNonPositive(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", gib, 0)

	for _, n := range []int64{0, -1} {
		if _, err := r.Reserve(context.Background(), acct, n, uuid.New()); err == nil {
			t.Errorf("Reserve(%d) succeeded", n)
		}
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", 10*gib, 0)

	uploadID := uuid.New()
	if _, err := r.Reserve(context.Background(), acct, 4*gib, uploadID); err != nil {
		t.Fatalf("Reserve() = %v", err)
	}
	if got := store.ReservedOn(acct); got != 4*gib {
		t.Fatalf("reserved = %d, want %d", got, 4*gib)
	}

	for i := 0; i < 3; i++ {
		if err := r.Release(context.Background(), uploadID); err != nil {
			t.Fatalf("Release() call %d = %v", i+1, err)
		}
	}

	// A second release must not drive the counter negative, which would
	// silently inflate free space for every later upload.
	if got := store.ReservedOn(acct); got != 0 {
		t.Errorf("reserved after repeated release = %d, want 0", got)
	}
}

// The janitor. A killed process must not strand capacity forever.
func TestReclaimExpiredReleasesStrandedCapacity(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", 10*gib, 0)

	dead := uuid.New()
	alive := uuid.New()
	if _, err := r.Reserve(context.Background(), acct, 3*gib, dead); err != nil {
		t.Fatalf("Reserve() = %v", err)
	}
	if _, err := r.Reserve(context.Background(), acct, 2*gib, alive); err != nil {
		t.Fatalf("Reserve() = %v", err)
	}

	// The process holding `dead` was killed mid-upload.
	store.Expire(dead)

	if err := r.ReclaimExpired(context.Background()); err != nil {
		t.Fatalf("ReclaimExpired() = %v", err)
	}

	// Only the stale one is released; the live upload keeps its claim.
	if got := store.ReservedOn(acct); got != 2*gib {
		t.Errorf("reserved = %d, want %d", got, 2*gib)
	}
}

// The headline claim: a 30 GB file across three 15 GB accounts.
func TestPlanStripesAcrossAccounts(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	ids := make([]uuid.UUID, 3)
	for i := range ids {
		ids[i] = uuid.New()
		store.AddAccount(ids[i], int32(i+1), "drive@example.com", fifteenG, 0)
	}

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	const size = 30 * gib
	plan, err := p.Plan(context.Background(), userID, size)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}

	// Every byte is covered exactly once, in order, with no gaps.
	var offset int64
	for i, s := range plan.Shards {
		if s.PlainOffset != offset {
			t.Fatalf("shard %d starts at %d, want %d", i, s.PlainOffset, offset)
		}
		if s.PlainSize <= 0 {
			t.Fatalf("shard %d has size %d", i, s.PlainSize)
		}
		offset += s.PlainSize
	}
	if offset != size {
		t.Fatalf("the plan covers %d bytes, want %d", offset, size)
	}

	// It must actually be spread: a single account cannot hold 30 GB.
	used := map[uuid.UUID]int64{}
	for _, s := range plan.Shards {
		used[s.AccountID] += s.PlainSize
	}
	if len(used) < 3 {
		t.Errorf("the plan used %d drives, want all 3", len(used))
	}
	for id, n := range used {
		if n > fifteenG {
			t.Errorf("drive %s was assigned %d bytes, more than its %d capacity", id, n, fifteenG)
		}
	}

	// And the capacity is actually held, not merely intended.
	var reserved int64
	for _, id := range ids {
		reserved += store.ReservedOn(id)
	}
	if reserved != size {
		t.Errorf("reserved %d bytes for a %d byte file", reserved, size)
	}
}

// Never start an upload that cannot finish. Architecture.md §6.
func TestPlanFailsEarlyWhenThePoolIsTooSmall(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	a, b := uuid.New(), uuid.New()
	store.AddAccount(a, 1, "a@example.com", 5*gib, 0)
	store.AddAccount(b, 2, "b@example.com", 5*gib, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	_, err := p.Plan(context.Background(), userID, 30*gib)
	if !errors.Is(err, skerr.ErrQuotaExceeded) {
		t.Fatalf("Plan() = %v, want ErrQuotaExceeded", err)
	}

	// The message has to say what is needed and what is available, per
	// Design.md §7 — not "Insufficient storage available."
	var pub *skerr.PublicError
	if !errors.As(err, &pub) {
		t.Fatal("expected a PublicError")
	}
	for _, want := range []string{"Needs", "free", "drives"} {
		if !contains(pub.Message, want) {
			t.Errorf("message %q is missing %q", pub.Message, want)
		}
	}

	// Nothing was reserved: a refused plan must leave no trace.
	if store.ReservedOn(a) != 0 || store.ReservedOn(b) != 0 {
		t.Error("a refused plan left capacity reserved")
	}
}

func TestPlanWithNoDrives(t *testing.T) {
	r, _ := newReserver(t)
	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	_, err := p.Plan(context.Background(), uuid.New(), gib)
	if !errors.Is(err, skerr.ErrQuotaExceeded) {
		t.Fatalf("Plan() = %v, want ErrQuotaExceeded", err)
	}
	var pub *skerr.PublicError
	if !errors.As(err, &pub) || !contains(pub.Message, "Connect one") {
		t.Errorf("message = %v, want it to tell the user to connect a drive", err)
	}
}

func TestPlanSingleShardWhenItFits(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", fifteenG, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	// Smaller than one shard: exactly one shard, no striping.
	plan, err := p.Plan(context.Background(), uuid.New(), 1<<20)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	if len(plan.Shards) != 1 {
		t.Fatalf("shards = %d, want 1", len(plan.Shards))
	}
	if plan.Shards[0].PlainSize != 1<<20 {
		t.Errorf("shard size = %d, want %d", plan.Shards[0].PlainSize, 1<<20)
	}
}

func TestPlanZeroByteFileStillGetsAShard(t *testing.T) {
	r, store := newReserver(t)
	store.AddAccount(uuid.New(), 1, "a@example.com", gib, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	plan, err := p.Plan(context.Background(), uuid.New(), 0)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	// The manifest is never empty, so the read path has no special case.
	if len(plan.Shards) != 1 {
		t.Fatalf("shards = %d, want 1", len(plan.Shards))
	}
	if plan.Shards[0].PlainSize != 0 {
		t.Errorf("shard size = %d, want 0", plan.Shards[0].PlainSize)
	}
}

func TestPlanAccountsForEncryptionOverhead(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()

	// Just enough room for the plaintext, but not for the ciphertext.
	const plain = int64(10) << 20
	store.AddAccount(acct, 1, "a@example.com", plain, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, skcrypto.StreamOverhead)

	// A planner that ignored expansion would happily place this and then
	// run the drive out of space at the final frame.
	if _, err := p.Plan(context.Background(), uuid.New(), plain); !errors.Is(err, skerr.ErrQuotaExceeded) {
		t.Fatalf("Plan() = %v, want ErrQuotaExceeded; the plan ignored ciphertext expansion", err)
	}
}

func TestPlanReservesCiphertextSize(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", gib, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, skcrypto.StreamOverhead)

	const plain = int64(1) << 20
	if _, err := p.Plan(context.Background(), uuid.New(), plain); err != nil {
		t.Fatalf("Plan() = %v", err)
	}

	want := skcrypto.StreamOverhead(plain)
	if got := store.ReservedOn(acct); got != want {
		t.Errorf("reserved %d bytes, want %d (the ciphertext size, not the plaintext)", got, want)
	}
}

func TestPolicies(t *testing.T) {
	t.Run("most available picks the emptiest", func(t *testing.T) {
		r, store := newReserver(t)
		full := uuid.New()
		empty := uuid.New()
		store.AddAccount(full, 1, "full@example.com", 10*gib, 9*gib)
		store.AddAccount(empty, 2, "empty@example.com", 10*gib, 0)

		p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)
		plan, err := p.Plan(context.Background(), uuid.New(), 1<<20)
		if err != nil {
			t.Fatalf("Plan() = %v", err)
		}
		if plan.Shards[0].AccountID != empty {
			t.Error("most-available did not choose the drive with the most free space")
		}
	})

	t.Run("priority follows connection order", func(t *testing.T) {
		r, store := newReserver(t)
		first := uuid.New()
		second := uuid.New()
		store.AddAccount(first, 1, "first@example.com", 10*gib, 9*gib)
		store.AddAccount(second, 2, "second@example.com", 10*gib, 0)

		p := NewPlanner(r, PolicyPriority, shard256, noOverhead)
		plan, err := p.Plan(context.Background(), uuid.New(), 1<<20)
		if err != nil {
			t.Fatalf("Plan() = %v", err)
		}
		// Ordinal 1 wins even though it has less room.
		if plan.Shards[0].AccountID != first {
			t.Error("priority did not choose the first-connected drive")
		}
	})

	t.Run("round robin moves between uploads", func(t *testing.T) {
		r, store := newReserver(t)
		a, b, c := uuid.New(), uuid.New(), uuid.New()
		store.AddAccount(a, 1, "a@example.com", 100*gib, 0)
		store.AddAccount(b, 2, "b@example.com", 100*gib, 0)
		store.AddAccount(c, 3, "c@example.com", 100*gib, 0)

		p := NewPlanner(r, PolicyRoundRobin, shard256, noOverhead)

		seen := map[uuid.UUID]bool{}
		for i := 0; i < 3; i++ {
			plan, err := p.Plan(context.Background(), uuid.New(), 1<<20)
			if err != nil {
				t.Fatalf("Plan() = %v", err)
			}
			seen[plan.Shards[0].AccountID] = true
		}
		if len(seen) != 3 {
			t.Errorf("round-robin used %d drives over 3 uploads, want 3", len(seen))
		}
	})
}

// Two large concurrent uploads against a pool that can only hold one: exactly
// one succeeds, and the loser leaves nothing reserved.
func TestConcurrentPlansCannotOversubscribe(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", 10*gib, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Plan(context.Background(), uuid.New(), 8*gib); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("%d concurrent 8 GiB plans succeeded against 10 GiB, want 1", okCount)
	}
	if got := store.ReservedOn(acct); got != 8*gib {
		t.Errorf("reserved = %d, want %d; the failed plan did not unwind", got, 8*gib)
	}
}

func TestPooledFree(t *testing.T) {
	candidates := []Candidate{
		{Total: 100, Used: 20, Reserved: 10}, // 70
		{Total: 100, Used: 100},              // 0
		{Total: 100, Used: 50, Reserved: 60}, // clamped to 0, not -10
	}
	if got := PooledFree(candidates); got != 70 {
		t.Errorf("PooledFree() = %d, want 70", got)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{30 * gib, "30.0 GB"},
	}
	for _, tc := range tests {
		if got := HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}())
}
