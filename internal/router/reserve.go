// Package router decides where bytes go: which accounts an upload is laid
// out across, and the atomic reservation that stops two uploads claiming the
// same free space.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
)

// reservationTTL bounds how long an upload may hold capacity it has not used.
//
// Long enough for a slow 256 MiB shard on a poor connection, short enough that
// a killed process does not strand capacity for an afternoon. The janitor
// extends nothing: a live upload re-reserves per shard as it goes.
const reservationTTL = 30 * time.Minute

// Candidate is one account the planner may use, with its live free space.
type Candidate struct {
	AccountID uuid.UUID
	Ordinal   int32
	Email     string
	Total     int64
	Used      int64
	Reserved  int64
}

// Free is what an upload may actually claim: reported free space minus what
// concurrent uploads have already committed to.
func (c Candidate) Free() int64 {
	free := c.Total - c.Used - c.Reserved
	if free < 0 {
		return 0
	}
	return free
}

// Reservation is one held claim on an account's capacity.
type Reservation struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Bytes     int64
}

// Store is the persistence the reservation scheme needs.
type Store interface {
	// Candidates returns the user's active accounts, most free space first.
	Candidates(ctx context.Context, userID uuid.UUID) ([]Candidate, error)

	// Reserve is the atomic conditional UPDATE from Architecture.md §5. It
	// returns ErrNoCapacity — not an error — when another upload got there
	// first, because that is an ordinary outcome the caller retries past.
	Reserve(ctx context.Context, accountID uuid.UUID, bytes int64, uploadID uuid.UUID, expiresAt time.Time) (Reservation, error)

	// Release gives bytes back for one upload.
	Release(ctx context.Context, uploadID uuid.UUID) (int64, error)

	// ReclaimExpired releases reservations past their expiry and reports how
	// many were freed.
	ReclaimExpired(ctx context.Context) (released int, bytes int64, err error)
}

// ErrNoCapacity reports that an account could not satisfy a reservation. It is
// a control-flow signal, not a failure: the planner tries the next candidate.
var ErrNoCapacity = errors.New("router: account has no capacity for this reservation")

// Reserver takes and releases capacity claims.
type Reserver struct {
	store Store
	log   *slog.Logger
	now   func() time.Time
}

// NewReserver wires the reservation service.
func NewReserver(store Store, log *slog.Logger) *Reserver {
	return &Reserver{store: store, log: log, now: time.Now}
}

// Reserve claims bytes on one account for an upload.
func (r *Reserver) Reserve(ctx context.Context, accountID uuid.UUID, bytes int64, uploadID uuid.UUID) (Reservation, error) {
	if bytes <= 0 {
		return Reservation{}, fmt.Errorf("router: reservation must be positive, got %d", bytes)
	}
	res, err := r.store.Reserve(ctx, accountID, bytes, uploadID, r.now().Add(reservationTTL))
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			return Reservation{}, ErrNoCapacity
		}
		return Reservation{}, fmt.Errorf("reserve %d bytes on %s: %w", bytes, accountID, err)
	}
	return res, nil
}

// Release gives back everything an upload reserved.
//
// It runs on both the success and the failure path: on success the bytes are
// now used rather than reserved, and on failure they were never used at all.
// Either way holding them is wrong.
func (r *Reserver) Release(ctx context.Context, uploadID uuid.UUID) error {
	n, err := r.store.Release(ctx, uploadID)
	if err != nil {
		return fmt.Errorf("release reservations for upload %s: %w", uploadID, err)
	}
	if n > 0 {
		r.log.DebugContext(ctx, "released reservations",
			slog.String("upload_id", uploadID.String()),
			slog.Int64("count", n))
	}
	return nil
}

// ReclaimExpired is the janitor. It runs on a ticker and releases capacity
// held by uploads that will never finish — a killed process, a container
// restart, a network partition that outlived the request.
//
// Without it a crashed upload strands its reservation permanently, and a
// self-hoster's pool shrinks every time something goes wrong.
func (r *Reserver) ReclaimExpired(ctx context.Context) error {
	count, bytes, err := r.store.ReclaimExpired(ctx)
	if err != nil {
		return fmt.Errorf("reclaim expired reservations: %w", err)
	}
	if count > 0 {
		// Worth noticing: a steady stream of these means uploads are
		// dying somewhere.
		r.log.InfoContext(ctx, "reclaimed stale reservations",
			slog.Int("count", count),
			slog.Int64("bytes", bytes))
	}
	return nil
}

// Candidates returns the user's accounts with live free space.
func (r *Reserver) Candidates(ctx context.Context, userID uuid.UUID) ([]Candidate, error) {
	c, err := r.store.Candidates(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list capacity candidates: %w", err)
	}
	return c, nil
}

// PooledFree sums what is actually claimable across every candidate.
func PooledFree(candidates []Candidate) int64 {
	var total int64
	for _, c := range candidates {
		total += c.Free()
	}
	return total
}

// ErrInsufficientPool reports that even the whole pool cannot hold a file.
func ErrInsufficientPool(size, pooled int64, drives int) error {
	if drives == 0 {
		return skerr.Public(skerr.ErrQuotaExceeded,
			"No drive is connected. Connect one in Settings.")
	}
	return skerr.Public(skerr.ErrQuotaExceeded,
		"Needs %s. %s free across %d %s. Connect another.",
		HumanBytes(size), HumanBytes(pooled), drives, plural(drives, "drive", "drives"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// HumanBytes formats a size for a user-facing message. Design.md §7: errors
// say what happened in numbers a person can act on.
func HumanBytes(n int64) string {
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
