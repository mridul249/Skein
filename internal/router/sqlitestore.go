package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteStore is the SQLite implementation of Store for skein-desktop.
type SQLiteStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLiteStore wraps an open migrated SQLite database.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db, now: time.Now}
}

// Candidates returns the user's active accounts, most free space first.
func (s *SQLiteStore) Candidates(ctx context.Context, userID uuid.UUID) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ca.id, ca.ordinal, ca.email,
       COALESCE(sa.total_bytes, 0),
       COALESCE(sa.used_bytes, 0),
       COALESCE(sa.reserved_bytes, 0)
  FROM connected_accounts ca
  JOIN storage_accounts sa ON sa.connected_account_id = ca.id
 WHERE ca.user_id = ?
   AND ca.status = 'active'
 ORDER BY (sa.total_bytes - sa.used_bytes - sa.reserved_bytes) DESC, ca.ordinal`, userID.String())
	if err != nil {
		return nil, fmt.Errorf("list planning candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var id string
		var c Candidate
		if err := rows.Scan(&id, &c.Ordinal, &c.Email, &c.Total, &c.Used, &c.Reserved); err != nil {
			return nil, fmt.Errorf("scan planning candidate: %w", err)
		}
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("parse planning account id %q: %w", id, err)
		}
		c.AccountID = parsed
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan planning candidates: %w", err)
	}
	if out == nil {
		out = []Candidate{}
	}
	return out, nil
}

// Reserve claims capacity with SQLite's atomic conditional UPDATE.
func (s *SQLiteStore) Reserve(ctx context.Context, accountID uuid.UUID, bytes int64, uploadID uuid.UUID, expiresAt time.Time) (Reservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var got string
	err = tx.QueryRowContext(ctx, `
UPDATE storage_accounts
   SET reserved_bytes = reserved_bytes + ?1
 WHERE connected_account_id = ?2
   AND (total_bytes - used_bytes - reserved_bytes) >= ?1
RETURNING connected_account_id`, bytes, accountID.String()).Scan(&got)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Reservation{}, ErrNoCapacity
		}
		return Reservation{}, fmt.Errorf("reserve bytes: %w", err)
	}

	id := uuid.New()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO quota_reservations (id, storage_account_id, bytes, upload_id, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		id.String(), got, bytes, uploadID.String(), s.fmt(s.now()), s.fmt(expiresAt)); err != nil {
		return Reservation{}, fmt.Errorf("record reservation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Reservation{}, fmt.Errorf("commit reservation: %w", err)
	}
	return Reservation{ID: id, AccountID: accountID, Bytes: bytes}, nil
}

// Release gives back everything one upload reserved.
func (s *SQLiteStore) Release(ctx context.Context, uploadID uuid.UUID) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
DELETE FROM quota_reservations
 WHERE upload_id = ?
RETURNING storage_account_id, bytes`, uploadID.String())
	if err != nil {
		return 0, fmt.Errorf("delete reservations: %w", err)
	}
	defer rows.Close()

	var n int64
	for rows.Next() {
		var accountID string
		var bytes int64
		if err := rows.Scan(&accountID, &bytes); err != nil {
			return 0, fmt.Errorf("scan reservation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE storage_accounts
   SET reserved_bytes = max(0, reserved_bytes - ?)
 WHERE connected_account_id = ?`, bytes, accountID); err != nil {
			return 0, fmt.Errorf("release reservation: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan reservations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit release: %w", err)
	}
	return n, nil
}

// ReclaimExpired releases stale reservations.
func (s *SQLiteStore) ReclaimExpired(ctx context.Context) (int, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
DELETE FROM quota_reservations
 WHERE expires_at < ?
RETURNING storage_account_id, bytes`, s.fmt(s.now()))
	if err != nil {
		return 0, 0, fmt.Errorf("select expired reservations: %w", err)
	}
	defer rows.Close()

	var count int
	var total int64
	for rows.Next() {
		var accountID string
		var bytes int64
		if err := rows.Scan(&accountID, &bytes); err != nil {
			return 0, 0, fmt.Errorf("scan expired reservation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE storage_accounts
   SET reserved_bytes = max(0, reserved_bytes - ?)
 WHERE connected_account_id = ?`, bytes, accountID); err != nil {
			return 0, 0, fmt.Errorf("release expired reservation: %w", err)
		}
		count++
		total += bytes
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan expired reservations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit reclaim: %w", err)
	}
	return count, total, nil
}

func (s *SQLiteStore) fmt(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

var _ Store = (*SQLiteStore)(nil)
