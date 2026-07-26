package accounts

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
)

// Resolver hands out a storage.Backend for a connected account, and a
// fallback backend when a deployment has none.
//
// It satisfies files.BackendResolver. The interface is declared over in
// internal/files, by the consumer; this is the implementation.
type Resolver struct {
	svc *Service

	// fallback is used when a shard has no account id, which happens on a
	// deployment running against local disk with no drive connected.
	fallback storage.Backend

	// cache keeps one backend per account for the process lifetime. Building
	// one is cheap but it carries the OAuth token source, and sharing that
	// is what makes token refresh happen once rather than per request.
	mu    sync.RWMutex
	cache map[uuid.UUID]storage.Backend
}

// NewResolver builds a backend resolver. fallback may be nil, in which case a
// shard with no account is an error rather than a silent local write.
func NewResolver(svc *Service, fallback storage.Backend) *Resolver {
	return &Resolver{svc: svc, fallback: fallback, cache: map[uuid.UUID]storage.Backend{}}
}

// For returns the backend holding a shard.
func (r *Resolver) For(ctx context.Context, userID uuid.UUID, accountID *uuid.UUID) (storage.Backend, error) {
	if accountID == nil {
		if r.fallback == nil {
			return nil, skerr.Public(skerr.ErrUnavailable,
				"No drive is connected. Connect one in Settings.")
		}
		return r.fallback, nil
	}

	r.mu.RLock()
	cached, ok := r.cache[*accountID]
	r.mu.RUnlock()
	if ok {
		return cached, nil
	}

	// Ownership is enforced inside Backend, which loads the account with a
	// user_id predicate. A caller cannot reach another user's drive by
	// guessing an id.
	backend, err := r.svc.Backend(ctx, userID, *accountID)
	if err != nil {
		return nil, fmt.Errorf("resolve backend for account %s: %w", *accountID, err)
	}

	r.mu.Lock()
	r.cache[*accountID] = backend
	r.mu.Unlock()

	return backend, nil
}

// Forget drops a cached backend, so disconnecting and reconnecting a drive
// does not keep using a token source built from the old grant.
func (r *Resolver) Forget(accountID uuid.UUID) {
	r.mu.Lock()
	delete(r.cache, accountID)
	r.mu.Unlock()
}

// PickAccount chooses where a new upload of the given size should go.
//
// Phase 3 ships the simplest policy that is not wrong: the account with the
// most free space that can hold the whole file. Phase 5 replaces this with the
// routing policies and the striping planner; the signature is what
// files.NewSingleShardPlanner takes, so that swap does not reach the upload
// path.
func (r *Resolver) PickAccount(ctx context.Context, userID uuid.UUID, size int64) (*uuid.UUID, error) {
	pool, err := r.svc.PoolFor(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("read pooled quota: %w", err)
	}

	var best *Capacity
	for i := range pool.Accounts {
		c := &pool.Accounts[i]
		if c.Account.Status != StatusActive {
			continue
		}
		if c.FreeBytes() < size {
			continue
		}
		if best == nil || c.FreeBytes() > best.FreeBytes() {
			best = c
		}
	}

	if best == nil {
		// No single drive can hold it. Striping is what fixes this, and
		// it lands in Phase 5 — until then the error says exactly what is
		// wrong and by how much, per Design.md §7.
		if pool.FreeBytes >= size {
			return nil, skerr.Public(skerr.ErrQuotaExceeded,
				"Needs %s. No single drive has that much free, though %s is free across your drives.",
				humanBytes(size), humanBytes(pool.FreeBytes))
		}
		if len(pool.Accounts) == 0 {
			if r.fallback != nil {
				return nil, nil
			}
			return nil, skerr.Public(skerr.ErrQuotaExceeded,
				"No drive is connected. Connect one in Settings.")
		}
		return nil, skerr.Public(skerr.ErrQuotaExceeded,
			"Needs %s. %s free across %d drives. Connect another.",
			humanBytes(size), humanBytes(pool.FreeBytes), len(pool.Accounts))
	}

	id := best.Account.ID
	return &id, nil
}

// humanBytes formats a size for a user-facing message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
