package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mridul249/Skein/internal/db/gen"
)

// PGStore is the Postgres implementation of Store.
type PGStore struct {
	q  *gen.Queries
	db gen.DBTX
}

// NewPGStore wraps a pgx connection pool.
func NewPGStore(db gen.DBTX) *PGStore { return &PGStore{q: gen.New(db), db: db} }

// Candidates returns the user's active accounts, most free space first.
func (s *PGStore) Candidates(ctx context.Context, userID uuid.UUID) ([]Candidate, error) {
	rows, err := s.q.ListAccountCapacityForPlanning(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list planning candidates: %w", err)
	}
	out := make([]Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, Candidate{
			AccountID: r.ID,
			Ordinal:   r.Ordinal,
			Email:     r.Email,
			Total:     r.TotalBytes,
			Used:      r.UsedBytes,
			Reserved:  r.ReservedBytes,
		})
	}
	return out, nil
}

// Reserve claims capacity with the atomic conditional UPDATE.
//
// The UPDATE and the reservation row go in one transaction: the counter on
// storage_accounts is what enforces the limit, and the row is what lets the
// janitor find it again. A crash between the two would leave capacity held
// with nothing pointing at it — unreclaimable until someone noticed by hand.
func (s *PGStore) Reserve(ctx context.Context, accountID uuid.UUID, bytes int64, uploadID uuid.UUID, expiresAt time.Time) (Reservation, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	if _, err := q.ReserveBytes(ctx, gen.ReserveBytesParams{
		Bytes:              bytes,
		ConnectedAccountID: accountID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The predicate did not hold: another upload took the space
			// first. An ordinary outcome, not a failure.
			return Reservation{}, ErrNoCapacity
		}
		return Reservation{}, fmt.Errorf("reserve bytes: %w", err)
	}

	id := uuid.New()
	if err := q.RecordReservation(ctx, gen.RecordReservationParams{
		ID:               id,
		StorageAccountID: accountID,
		Bytes:            bytes,
		UploadID:         uploadID,
		ExpiresAt:        pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return Reservation{}, fmt.Errorf("record reservation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, fmt.Errorf("commit reservation: %w", err)
	}

	return Reservation{ID: id, AccountID: accountID, Bytes: bytes}, nil
}

// Release gives back everything one upload reserved.
func (s *PGStore) Release(ctx context.Context, uploadID uuid.UUID) (int64, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	// Deleting and returning in one statement is what makes this safe to
	// run twice: the second call finds nothing and releases nothing.
	rows, err := q.DeleteReservationsForUpload(ctx, uploadID)
	if err != nil {
		return 0, fmt.Errorf("delete reservations: %w", err)
	}

	for _, r := range rows {
		if err := q.ReleaseReservation(ctx, gen.ReleaseReservationParams{
			Bytes:              r.Bytes,
			ConnectedAccountID: r.StorageAccountID,
		}); err != nil {
			return 0, fmt.Errorf("release reservation: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit release: %w", err)
	}
	return int64(len(rows)), nil
}

// ReclaimExpired is the janitor's database half.
func (s *PGStore) ReclaimExpired(ctx context.Context) (int, int64, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	rows, err := q.ExpiredReservations(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("select expired reservations: %w", err)
	}

	var total int64
	for _, r := range rows {
		if err := q.ReleaseReservation(ctx, gen.ReleaseReservationParams{
			Bytes:              r.Bytes,
			ConnectedAccountID: r.StorageAccountID,
		}); err != nil {
			return 0, 0, fmt.Errorf("release expired reservation: %w", err)
		}
		total += r.Bytes
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit reclaim: %w", err)
	}
	return len(rows), total, nil
}

func (s *PGStore) begin(ctx context.Context) (pgx.Tx, error) {
	beginner, ok := s.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, errors.New("router: database handle does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	return tx, nil
}

// Compile-time check.
var _ Store = (*PGStore)(nil)
