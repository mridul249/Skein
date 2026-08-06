package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/skerr"
)

const (
	gib      = int64(1) << 30
	fifteenG = 15 * gib
	shard256 = 256 << 20
)

func newReserver(t *testing.T) (*Reserver, conformanceStore) {
	t.Helper()
	store := newConformanceStore(t)
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

// Two large concurrent uploads against a pool that can only hold one.
//
// The guarantee is safety, not fairness: capacity is never oversubscribed, and
// whoever loses unwinds completely. Both succeeding would mean a drive was
// promised bytes twice, and that is the failure this whole scheme exists to
// prevent.
//
// Liveness is deliberately *not* asserted. A plan reserves shard by shard, so
// two uploads can interleave, both partially fill the pool, and both roll back
// — measured at roughly 5% of trials with two 8 GiB uploads against 10 GiB.
// Nothing is corrupted and nothing is stranded; the user retries. Guaranteeing
// a winner needs whole-plan reservation in one transaction, which is a change
// to the planner the owner intends to rewrite. Recorded as known issue #7.
//
// Run this with -count=50 or more. A single run passes by luck either way; the
// invariant only shows up under repetition.
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

	// The safety property. Two winners means 16 GiB was promised out of 10.
	if okCount > 1 {
		t.Fatalf("%d concurrent 8 GiB plans succeeded against 10 GiB; capacity was oversubscribed", okCount)
	}

	// Whatever happened, the reservation ledger has to agree with it. A
	// winner holds exactly its 8 GiB; if nobody won, nothing is held.
	reserved := store.ReservedOn(acct)
	want := int64(okCount) * 8 * gib
	if reserved != want {
		t.Errorf("reserved = %d with %d winner(s), want %d; a plan did not unwind cleanly",
			reserved, okCount, want)
	}
	if reserved > 10*gib {
		t.Errorf("reserved %d bytes against a %d byte drive", reserved, 10*gib)
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

// The round-robin cursor is shared by every concurrent upload. It was a plain
// int, which -race flags the moment two plans overlap, so it is now atomic.
// The cursor is a spread heuristic rather than a correctness invariant —
// interleaved increments are fine, an unsynchronised read/write is not.
func TestRoundRobinPlannerIsRaceFree(t *testing.T) {
	r, store := newReserver(t)
	for i := 0; i < 3; i++ {
		store.AddAccount(uuid.New(), int32(i+1), "drive@example.com", 100*gib, 0)
	}
	p := NewPlanner(r, PolicyRoundRobin, shard256, noOverhead)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Plan(context.Background(), uuid.New(), 1<<20); err != nil {
				t.Errorf("Plan() = %v", err)
			}
		}()
	}
	wg.Wait()
}

// Round-robin has to spread consecutive shards of one upload across drives,
// not just consecutive uploads. Otherwise a striped file lands entirely on one
// drive and the policy does nothing.
func TestRoundRobinSpreadsShardsWithinOneUpload(t *testing.T) {
	r, store := newReserver(t)
	ids := make([]uuid.UUID, 3)
	for i := range ids {
		ids[i] = uuid.New()
		store.AddAccount(ids[i], int32(i+1), "drive@example.com", 100*gib, 0)
	}
	p := NewPlanner(r, PolicyRoundRobin, shard256, noOverhead)

	// Four shards at 256 MiB.
	plan, err := p.Plan(context.Background(), uuid.New(), 4*shard256)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	if len(plan.Shards) != 4 {
		t.Fatalf("shards = %d, want 4", len(plan.Shards))
	}

	used := map[uuid.UUID]int{}
	for _, s := range plan.Shards {
		used[s.AccountID]++
	}
	if len(used) < 2 {
		t.Errorf("round-robin put all %d shards on %d drive(s); it must alternate",
			len(plan.Shards), len(used))
	}
}

// THE OWNER'S 400 MB FILE, REPRODUCED (2026-08-06).
//
// Two 15 GB drives, both nearly empty, unevenly used. A 400 MB file becomes a
// 256 MB shard and a 144 MB one — and BOTH landed on the same drive, twice in
// a row. Nothing was broken by it: the layout is legal, every byte is stored,
// and the file downloads. But it is not what striping is for.
//
// The cause is that most-available re-sorts after each shard and the leader
// does not change: a 256 MB debit against a 500 MB gap leaves the same drive
// in front, so it takes the next shard too, and the next. The emptiest drive
// absorbs an entire file while its neighbour sits idle — the drives level out
// only across many uploads, never within one.
//
// A striped file should touch more than one drive when more than one can hold
// a shard. That gives parallel transfer, it spreads the failure surface, and
// it is what the user is told the feature does.
func TestAStripedFileSpreadsAcrossDrivesEvenWhenOneCouldHoldItAll(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	// The owner's numbers: two drives with plenty of room and a ~500 MB gap
	// between them, so the emptier one stays ahead after a 256 MB debit.
	a, b := uuid.New(), uuid.New()
	store.AddAccount(a, 1, "a@example.com", fifteenG, 2205<<20) // ~12795 MB free
	store.AddAccount(b, 2, "b@example.com", fifteenG, 1699<<20) // ~13301 MB free

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	const size = 400 << 20
	plan, err := p.Plan(context.Background(), userID, size)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	if len(plan.Shards) != 2 {
		t.Fatalf("planned %d shards for a 400 MB file, want 2", len(plan.Shards))
	}

	used := map[uuid.UUID]int64{}
	for _, s := range plan.Shards {
		used[s.AccountID] += s.PlainSize
	}
	if len(used) != 2 {
		t.Errorf("a 2-shard file used %d drive(s), want 2; both shards on one drive "+
			"fills that drive unfairly and gives up the point of striping", len(used))
	}

	// Still correct: every byte covered once, in order.
	var offset int64
	for i, s := range plan.Shards {
		if s.PlainOffset != offset {
			t.Fatalf("shard %d starts at %d, want %d", i, s.PlainOffset, offset)
		}
		offset += s.PlainSize
	}
	if offset != size {
		t.Fatalf("the plan covers %d bytes, want %d", offset, size)
	}
}

// THE SPREAD MUST NEVER COST AN UPLOAD. It is a preference: if no unused drive
// can take a full shard, the shard goes on a drive already used rather than the
// plan failing. A prettier layout is not worth a refused upload.
func TestSpreadingNeverFailsAnUploadThatFits(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	// One roomy drive and one nearly full. A 600 MB file needs three shards;
	// only the big drive can take them, so all three must land there.
	big, tiny := uuid.New(), uuid.New()
	store.AddAccount(big, 1, "big@example.com", fifteenG, 0)
	store.AddAccount(tiny, 2, "tiny@example.com", 1<<20, 0) // 1 MB total

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	const size = 600 << 20
	plan, err := p.Plan(context.Background(), userID, size)
	if err != nil {
		t.Fatalf("Plan() = %v; the spread preference must not refuse a file that fits", err)
	}

	var offset int64
	for _, s := range plan.Shards {
		offset += s.PlainSize
	}
	if offset != size {
		t.Errorf("the plan covers %d bytes, want %d", offset, size)
	}
}

// A drive that can only take a sliver must NOT attract one. Letting it would
// fragment every upload into tiny shards, which costs more than the uneven
// fill the spread exists to fix.
//
// What enforces this is that the spread pass asks for the WHOLE shard and
// never shrinks it, so a drive without room loses at the reservation. The
// `c.Free()` pre-check in placeShard is only an optimisation — deleting it
// leaves this test green, which is correct and was verified by mutation.
// Mutating the no-shrink property fails it immediately.
func TestSpreadingDoesNotFragmentOntoANearlyFullDrive(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	big, sliver := uuid.New(), uuid.New()
	store.AddAccount(big, 1, "big@example.com", fifteenG, 0)
	// Room for 4 MB: enough to be a candidate, far short of a 256 MB shard.
	store.AddAccount(sliver, 2, "sliver@example.com", 4<<20, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	const size = 512 << 20
	plan, err := p.Plan(context.Background(), userID, size)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	for i, s := range plan.Shards {
		if s.AccountID == sliver {
			t.Errorf("shard %d (%d bytes) was placed on a drive with only 4 MB free; "+
				"the spread preference must require room for a FULL shard", i, s.PlainSize)
		}
	}
	if len(plan.Shards) != 2 {
		t.Errorf("planned %d shards, want 2; extra shards mean the file was fragmented",
			len(plan.Shards))
	}
}

// PolicyPriority means "fill the first drive until it is full". Spreading
// would contradict the thing the user chose it for, so it is exempt.
func TestPriorityPolicyStillFillsOneDriveFirst(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	first, second := uuid.New(), uuid.New()
	store.AddAccount(first, 1, "first@example.com", fifteenG, 0)
	store.AddAccount(second, 2, "second@example.com", fifteenG, 0)

	p := NewPlanner(r, PolicyPriority, shard256, noOverhead)

	plan, err := p.Plan(context.Background(), userID, 400<<20)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	for i, s := range plan.Shards {
		if s.AccountID != first {
			t.Errorf("shard %d went to the second drive under PolicyPriority, "+
				"which is documented to fill the first drive until it is full", i)
		}
	}
}

// With more shards than drives, the spread must WRAP rather than give up after
// one pass: three shards over two drives is 2/1, not 1/1 plus a free-for-all.
func TestSpreadingWrapsWhenThereAreMoreShardsThanDrives(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	a, b := uuid.New(), uuid.New()
	store.AddAccount(a, 1, "a@example.com", fifteenG, 0)
	store.AddAccount(b, 2, "b@example.com", fifteenG, 0)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	// Three full shards.
	plan, err := p.Plan(context.Background(), userID, 768<<20)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}
	if len(plan.Shards) != 3 {
		t.Fatalf("planned %d shards, want 3", len(plan.Shards))
	}
	counts := map[uuid.UUID]int{}
	for _, s := range plan.Shards {
		counts[s.AccountID]++
	}
	if len(counts) != 2 {
		t.Fatalf("3 shards used %d drive(s), want 2", len(counts))
	}
	for id, n := range counts {
		if n > 2 {
			t.Errorf("drive %s took %d of 3 shards; the spread should be 2/1", id, n)
		}
	}
}

// The reservations must match the plan exactly. A spread pass that reserved on
// one drive and then placed on another would strand capacity until the janitor
// ran, and the leak would be invisible in the plan itself.
func TestSpreadingReservesExactlyWhatItPlaces(t *testing.T) {
	r, store := newReserver(t)
	userID := uuid.New()

	a, b := uuid.New(), uuid.New()
	store.AddAccount(a, 1, "a@example.com", fifteenG, 2205<<20)
	store.AddAccount(b, 2, "b@example.com", fifteenG, 1699<<20)

	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	const size = 400 << 20
	plan, err := p.Plan(context.Background(), userID, size)
	if err != nil {
		t.Fatalf("Plan() = %v", err)
	}

	perDrive := map[uuid.UUID]int64{}
	for _, s := range plan.Shards {
		perDrive[s.AccountID] += s.PlainSize
	}
	for _, id := range []uuid.UUID{a, b} {
		if got, want := store.ReservedOn(id), perDrive[id]; got != want {
			t.Errorf("drive %s holds %d reserved bytes but the plan puts %d there",
				id, got, want)
		}
	}
	var total int64
	for _, id := range []uuid.UUID{a, b} {
		total += store.ReservedOn(id)
	}
	if total != size {
		t.Errorf("reserved %d bytes for a %d byte file", total, size)
	}
}
