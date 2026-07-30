package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mridul60214/skein/internal/db/gen"
	"github.com/mridul60214/skein/internal/skerr"
)

// pgUniqueViolation is the SQLSTATE for a unique constraint failure.
const pgUniqueViolation = "23505"

// PGStore is the Postgres implementation of Store. Its only job is to
// translate between generated types and domain types, and to turn driver
// errors into the sentinels in skerr so nothing above this layer sees SQL.
type PGStore struct{ q *gen.Queries }

// NewPGStore wraps a pgx connection pool.
func NewPGStore(db gen.DBTX) *PGStore { return &PGStore{q: gen.New(db)} }

// CreateUser inserts a user, mapping a duplicate email to ErrConflict.
func (s *PGStore) CreateUser(ctx context.Context, id uuid.UUID, email, passwordHash string) (User, error) {
	row, err := s.q.CreateUser(ctx, gen.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: passwordHash,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return User{}, skerr.ErrConflict
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return toUser(row), nil
}

// GetUserByEmail loads a user by address.
func (s *PGStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	row, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		return User{}, mapNoRows(err, "select user by email")
	}
	return toUser(row), nil
}

// GetUserByID loads a user by id.
func (s *PGStore) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return User{}, mapNoRows(err, "select user by id")
	}
	return toUser(row), nil
}

// UpdateUserPassword replaces a stored password hash.
func (s *PGStore) UpdateUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	if err := s.q.UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{
		ID:           id,
		PasswordHash: passwordHash,
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return nil
}

// CreateSession inserts one issued refresh token.
func (s *PGStore) CreateSession(ctx context.Context, n NewSession) (Session, error) {
	row, err := s.q.CreateSession(ctx, gen.CreateSessionParams{
		ID:          n.ID,
		UserID:      n.UserID,
		FamilyID:    n.FamilyID,
		PrevID:      n.PrevID,
		RefreshHash: n.RefreshHash,
		UserAgent:   truncate(n.UserAgent, 512),
		Ip:          n.IP,
		ExpiresAt:   ts(n.ExpiresAt),
	})
	if err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	return toSession(row), nil
}

// CreateTokenFamily records a new login's family.
func (s *PGStore) CreateTokenFamily(ctx context.Context, familyID, userID uuid.UUID) error {
	if err := s.q.CreateTokenFamily(ctx, gen.CreateTokenFamilyParams{
		ID:     familyID,
		UserID: userID,
	}); err != nil {
		return fmt.Errorf("create token family: %w", err)
	}
	return nil
}

// RevokeTokenFamily marks a family revoked. This is the write that actually
// stops future refreshes; see the comment on Store.RevokeTokenFamily.
func (s *PGStore) RevokeTokenFamily(ctx context.Context, familyID uuid.UUID) (int64, error) {
	n, err := s.q.RevokeTokenFamily(ctx, familyID)
	if err != nil {
		return 0, fmt.Errorf("revoke token family: %w", err)
	}
	return n, nil
}

// GetSessionByRefreshHash looks up a session by token hash, including rows
// that are used, revoked or expired.
func (s *PGStore) GetSessionByRefreshHash(ctx context.Context, hash []byte) (Session, error) {
	row, err := s.q.GetSessionByRefreshHash(ctx, hash)
	if err != nil {
		return Session{}, mapNoRows(err, "select session by hash")
	}
	return toSession(row), nil
}

// ClaimSession marks a session used if and only if it is currently claimable.
// Zero rows means the caller lost the race, which the service reads as reuse.
func (s *PGStore) ClaimSession(ctx context.Context, id uuid.UUID) (Session, error) {
	row, err := s.q.MarkSessionUsed(ctx, id)
	if err != nil {
		return Session{}, mapNoRows(err, "claim session")
	}
	return toSession(row), nil
}

// RevokeSessionFamily revokes every unrevoked session sharing a family id.
func (s *PGStore) RevokeSessionFamily(ctx context.Context, familyID uuid.UUID) (int64, error) {
	n, err := s.q.RevokeSessionFamily(ctx, familyID)
	if err != nil {
		return 0, fmt.Errorf("revoke family: %w", err)
	}
	return n, nil
}

// RevokeSession revokes a single session.
func (s *PGStore) RevokeSession(ctx context.Context, id uuid.UUID) (int64, error) {
	n, err := s.q.RevokeSession(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("revoke session: %w", err)
	}
	return n, nil
}

// RevokeAllUserSessions signs a user out everywhere.
func (s *PGStore) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) (int64, error) {
	n, err := s.q.RevokeAllUserSessions(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	return n, nil
}

// DeleteExpiredSessions removes sessions long past expiry.
func (s *PGStore) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return n, nil
}

// RecordSecurityEvent appends an audit record.
func (s *PGStore) RecordSecurityEvent(ctx context.Context, e SecurityEvent) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		// Never block the audit write on an unserialisable detail map.
		detail = []byte(`{"detail_encode_error":true}`)
	}
	if err := s.q.RecordSecurityEvent(ctx, gen.RecordSecurityEventParams{
		UserID:    e.UserID,
		Kind:      e.Kind,
		Detail:    detail,
		Ip:        e.IP,
		UserAgent: truncate(e.UserAgent, 512),
	}); err != nil {
		return fmt.Errorf("insert security event: %w", err)
	}
	return nil
}

func mapNoRows(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return skerr.ErrNotFound
	}
	return fmt.Errorf("%s: %w", what, err)
}

func toUser(r gen.User) User {
	return User{
		ID:              r.ID,
		Email:           r.Email,
		PasswordHash:    r.PasswordHash,
		EmailVerifiedAt: nullableTime(r.EmailVerifiedAt),
		CreatedAt:       r.CreatedAt.Time,
	}
}

func toSession(r gen.Session) Session {
	return Session{
		ID:        r.ID,
		UserID:    r.UserID,
		FamilyID:  r.FamilyID,
		PrevID:    r.PrevID,
		UserAgent: r.UserAgent,
		IP:        r.Ip,
		CreatedAt: r.CreatedAt.Time,
		ExpiresAt: r.ExpiresAt.Time,
		UsedAt:    nullableTime(r.UsedAt),
		RevokedAt: nullableTime(r.RevokedAt),
	}
}

func nullableTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// truncate bounds a client-supplied string before it reaches the database.
// User-Agent is attacker-controlled and unbounded on the wire.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ParseIP converts a resolved client address to the form the store wants.
// An unparseable address is stored as NULL rather than as a wrong value.
func ParseIP(s string) *netip.Addr {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	return &addr
}
