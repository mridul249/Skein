package files

import (
	"context"

	"github.com/google/uuid"
)

// SingleShardPlanner puts every upload in one shard on one account.
//
// It is what Phase 3 ships: the streaming path is the thing being proven, and
// proving it does not require more than one destination. Phase 5 replaces this
// with a planner that fills across accounts, and the upload path does not
// change, because the shape of a one-shard plan and a many-shard plan is the
// same.
type SingleShardPlanner struct {
	// pick chooses the destination account. It returns nil when no drive is
	// connected, which the local backend accepts.
	pick func(ctx context.Context, userID uuid.UUID, size int64) (*uuid.UUID, error)
}

// NewSingleShardPlanner builds a planner over an account chooser.
func NewSingleShardPlanner(pick func(ctx context.Context, userID uuid.UUID, size int64) (*uuid.UUID, error)) *SingleShardPlanner {
	return &SingleShardPlanner{pick: pick}
}

// Plan returns a one-shard layout.
func (p *SingleShardPlanner) Plan(ctx context.Context, userID uuid.UUID, size int64) (Plan, error) {
	var account *uuid.UUID
	if p.pick != nil {
		chosen, err := p.pick(ctx, userID, size)
		if err != nil {
			return Plan{}, err
		}
		account = chosen
	}

	return Plan{
		UserID: userID,
		Shards: []PlannedShard{{
			AccountID:   account,
			PlainSize:   size,
			PlainOffset: 0,
		}},
	}, nil
}

// Compile-time check.
var _ Planner = (*SingleShardPlanner)(nil)
