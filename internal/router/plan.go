package router

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
)

// Policy decides the order candidates are filled in.
type Policy string

// The routing policies.
const (
	// PolicyMostAvailable fills the emptiest drive first. It keeps every
	// drive roughly level, which minimises the number of files that have to
	// be striped at all.
	PolicyMostAvailable Policy = "most-available"

	// PolicyPriority fills in connection order, so the first drive is used
	// until it is full. Useful when one drive is faster or cheaper.
	PolicyPriority Policy = "priority"

	// PolicyRoundRobin spreads consecutive uploads across drives. It exists
	// for the case where per-drive request quotas, not space, are the
	// constraint.
	PolicyRoundRobin Policy = "round-robin"
)

// Shard is one planned piece of an upload.
type Shard struct {
	AccountID uuid.UUID
	// PlainSize is the plaintext byte count. Encryption expands this; the
	// upload path computes the stored size from it.
	PlainSize int64
	// PlainOffset is where this shard's first byte sits in the whole file.
	PlainOffset int64
}

// Plan is an ordered shard layout.
type Plan struct {
	UploadID uuid.UUID
	Shards   []Shard
	// Reservations must be released once the upload finishes or fails.
	Reservations []Reservation
}

// Planner lays an upload out across accounts and reserves the capacity for it.
type Planner struct {
	reserver *Reserver
	policy   Policy
	// shardSize is the target plaintext bytes per shard. The tail is short.
	shardSize int64
	// roundRobinCursor advances once per placement under PolicyRoundRobin,
	// so consecutive shards of one upload land on different drives.
	//
	// Atomic because a Planner is shared by every concurrent upload, and a
	// plain int here is a data race that -race catches immediately. The
	// value is a spread heuristic rather than a correctness invariant, so
	// interleaved increments between concurrent plans are fine; what is not
	// fine is the unsynchronised read/write itself.
	roundRobinCursor atomic.Uint64
	// overhead maps a plaintext shard size to its stored size, so the
	// reservation covers what is actually written rather than what the user
	// supplied. Encryption expands; a reservation that ignores that runs a
	// drive out of space at the last frame.
	overhead func(plainSize int64) int64
}

// NewPlanner builds a planner.
func NewPlanner(reserver *Reserver, policy Policy, shardSize int64, overhead func(int64) int64) *Planner {
	if shardSize <= 0 {
		shardSize = 256 << 20
	}
	if overhead == nil {
		overhead = func(n int64) int64 { return n }
	}
	return &Planner{reserver: reserver, policy: policy, shardSize: shardSize, overhead: overhead}
}

// Plan lays out an upload of the given plaintext size and reserves capacity.
//
// Architecture.md §6. Greedy fill in policy order, 256 MiB shards by default,
// short tail. Two properties matter more than the packing quality:
//
//   - It fails early. If the pooled free space cannot hold the file, nothing
//     is reserved and nothing is written. Starting an upload that cannot
//     finish wastes the user's bandwidth and leaves orphans to clean up.
//   - Every shard's capacity is reserved before any byte is written, through
//     the atomic UPDATE. Two concurrent 20 GB uploads against 30 GB of pooled
//     space produce one success and one clean refusal, not two half-written
//     files.
func (p *Planner) Plan(ctx context.Context, userID uuid.UUID, plainSize int64) (Plan, error) {
	if plainSize < 0 {
		return Plan{}, fmt.Errorf("router: negative size %d", plainSize)
	}

	candidates, err := p.reserver.Candidates(ctx, userID)
	if err != nil {
		return Plan{}, err
	}
	ordered := p.order(candidates)

	uploadID := uuid.New()
	plan := Plan{UploadID: uploadID}

	// A zero-byte file still needs a home: it gets one empty shard, so the
	// manifest is never empty and the read path has nothing special to do.
	if plainSize == 0 {
		if len(ordered) == 0 {
			return Plan{}, ErrInsufficientPool(0, 0, 0)
		}
		plan.Shards = []Shard{{AccountID: ordered[0].AccountID}}
		return plan, nil
	}

	// Fail early, before reserving anything. This check is advisory — the
	// reservations below are what actually enforce it — but it turns the
	// common case into one clear message rather than a partial plan that
	// unwinds.
	if pooled := PooledFree(ordered); pooled < p.overhead(plainSize) {
		return Plan{}, ErrInsufficientPool(plainSize, pooled, len(ordered))
	}

	var offset int64
	for offset < plainSize {
		remaining := plainSize - offset
		want := min(remaining, p.shardSize)

		shard, res, perr := p.placeShard(ctx, ordered, want, uploadID)
		if perr != nil {
			// Unwind: release everything reserved so far so a failed
			// plan does not strand capacity until the janitor runs.
			p.rollback(ctx, uploadID)
			return Plan{}, perr
		}

		shard.PlainOffset = offset
		plan.Shards = append(plan.Shards, shard)
		plan.Reservations = append(plan.Reservations, res)
		offset += shard.PlainSize

		// Reserving reduces the free space this loop sees next time
		// round, so the local view stays in step with the database.
		p.debit(ordered, shard.AccountID, p.overhead(shard.PlainSize))
		ordered = p.order(ordered)
	}

	return plan, nil
}

// placeShard finds an account that can take one shard, shrinking it if no
// single account can hold a full one.
func (p *Planner) placeShard(ctx context.Context, candidates []Candidate, want int64, uploadID uuid.UUID) (Shard, Reservation, error) {
	for _, c := range candidates {
		free := c.Free()
		if free <= 0 {
			continue
		}

		// How much plaintext fits, once expansion is accounted for. A
		// binary search would be exact; halving is enough because the
		// overhead is a small fraction and the reservation below is the
		// real check anyway.
		size := want
		for size > 0 && p.overhead(size) > free {
			size /= 2
		}
		if size <= 0 {
			continue
		}

		res, err := p.reserver.Reserve(ctx, c.AccountID, p.overhead(size), uploadID)
		if err != nil {
			if err == ErrNoCapacity {
				// Someone else claimed the space between the read and
				// here. That is exactly the race the conditional
				// UPDATE exists to make safe: try the next account.
				continue
			}
			return Shard{}, Reservation{}, err
		}
		return Shard{AccountID: c.AccountID, PlainSize: size}, res, nil
	}

	return Shard{}, Reservation{}, skerr.Public(skerr.ErrQuotaExceeded,
		"Ran out of space partway through. Nothing was saved.")
}

// rollback releases every reservation an abandoned plan took.
func (p *Planner) rollback(ctx context.Context, uploadID uuid.UUID) {
	if err := p.reserver.Release(ctx, uploadID); err != nil {
		// Not fatal: the janitor will get it at expiry. Still worth
		// knowing about, because it means capacity is briefly wrong.
		p.reserver.log.WarnContext(ctx, "could not roll back a failed plan",
			"upload_id", uploadID.String(), "error", err.Error())
	}
}

// order sorts candidates by the active policy.
func (p *Planner) order(in []Candidate) []Candidate {
	out := make([]Candidate, len(in))
	copy(out, in)

	switch p.policy {
	case PolicyPriority:
		sort.SliceStable(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })

	case PolicyRoundRobin:
		sort.SliceStable(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
		// Add(1)-1 takes this placement's slot and reserves the next one
		// in a single operation, so two concurrent plans cannot both read
		// the same cursor value.
		n := p.roundRobinCursor.Add(1) - 1
		if len(out) > 1 {
			shift := int(n % uint64(len(out)))
			out = append(out[shift:], out[:shift]...)
		}

	default: // PolicyMostAvailable
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Free() != out[j].Free() {
				return out[i].Free() > out[j].Free()
			}
			return out[i].Ordinal < out[j].Ordinal
		})
	}
	return out
}

// debit reduces the local view of an account's free space after a reservation.
func (p *Planner) debit(candidates []Candidate, accountID uuid.UUID, bytes int64) {
	for i := range candidates {
		if candidates[i].AccountID == accountID {
			candidates[i].Reserved += bytes
			return
		}
	}
}
