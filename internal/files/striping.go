package files

import (
	"context"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/router"
)

// StripingPlanner adapts router.Planner to the Planner interface the upload
// path consumes.
//
// It replaces SingleShardPlanner wholesale. Nothing in the upload path
// changes, because a one-shard plan and a many-shard plan are the same shape —
// which is why file_shards existed from Phase 3 rather than arriving here.
type StripingPlanner struct {
	inner *router.Planner
	// release gives back the capacity a plan reserved. The upload path calls
	// it on both the success and the failure path: on success the bytes are
	// used rather than reserved, on failure they were never used at all.
	release func(ctx context.Context, uploadID uuid.UUID) error
}

// NewStripingPlanner wraps a router planner.
func NewStripingPlanner(inner *router.Planner, reserver *router.Reserver) *StripingPlanner {
	return &StripingPlanner{inner: inner, release: reserver.Release}
}

// Plan lays an upload out across accounts, reserving capacity for each shard
// before any byte is written.
func (p *StripingPlanner) Plan(ctx context.Context, userID uuid.UUID, size int64) (Plan, error) {
	inner, err := p.inner.Plan(ctx, userID, size)
	if err != nil {
		return Plan{}, err
	}

	out := Plan{
		UserID:   userID,
		UploadID: inner.UploadID,
		Shards:   make([]PlannedShard, 0, len(inner.Shards)),
	}
	for _, s := range inner.Shards {
		id := s.AccountID
		out.Shards = append(out.Shards, PlannedShard{
			AccountID:   &id,
			PlainSize:   s.PlainSize,
			PlainOffset: s.PlainOffset,
		})
	}
	return out, nil
}

// Release gives back a plan's reservations.
func (p *StripingPlanner) Release(ctx context.Context, uploadID uuid.UUID) error {
	if p.release == nil || uploadID == uuid.Nil {
		return nil
	}
	return p.release(ctx, uploadID)
}

// Compile-time check.
var _ Planner = (*StripingPlanner)(nil)
