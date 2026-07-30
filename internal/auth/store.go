package auth

import (
	"context"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// User is the domain view of a user row.
type User struct {
	ID              uuid.UUID
	Email           string
	PasswordHash    string
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
}

// Session is the domain view of one issued refresh token. The token itself is
// never part of this struct; only its hash is stored, and even that leaves the
// store only so the service can compare families.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	FamilyID  uuid.UUID
	PrevID    *uuid.UUID
	UserAgent string
	IP        *netip.Addr
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}

// NewSession is the input to Store.CreateSession.
type NewSession struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	FamilyID    uuid.UUID
	PrevID      *uuid.UUID
	RefreshHash []byte
	UserAgent   string
	IP          *netip.Addr
	ExpiresAt   time.Time
}

// SecurityEvent is an append-only audit record.
type SecurityEvent struct {
	UserID    *uuid.UUID
	Kind      string
	Detail    map[string]any
	IP        *netip.Addr
	UserAgent string
}

// Event kinds. These strings end up in the audit table and in alerting, so
// they are constants rather than literals scattered through the service.
const (
	EventLoginSucceeded    = "login.succeeded"
	EventLoginFailed       = "login.failed"
	EventRegistered        = "user.registered"
	EventRefreshRotated    = "refresh.rotated"
	EventRefreshReuse      = "refresh.reuse_detected"
	EventRefreshRejected   = "refresh.rejected"
	EventLogout            = "session.logout"
	EventPasswordChanged   = "password.changed"
	EventAccountDisconnect = "account.disconnected"
)

// Store is the persistence the auth service needs. It is declared here, in the
// consumer, so the service can be tested against an in-memory implementation
// with no database.
type Store interface {
	CreateUser(ctx context.Context, id uuid.UUID, email, passwordHash string) (User, error)
	GetUserByEmail(ctx context.Context, email string) (User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (User, error)
	UpdateUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) error

	// CreateTokenFamily records a new login's family. It must be called
	// before the first session of that family, because sessions.family_id
	// references it.
	CreateTokenFamily(ctx context.Context, familyID, userID uuid.UUID) error

	// RevokeTokenFamily kills every token descended from one login,
	// including successors not yet inserted. Known issue #11: this is the
	// enforcing write, and RevokeSessionFamily is only the audit record.
	// Idempotent, and it preserves the first revocation's timestamp.
	RevokeTokenFamily(ctx context.Context, familyID uuid.UUID) (int64, error)

	CreateSession(ctx context.Context, s NewSession) (Session, error)
	GetSessionByRefreshHash(ctx context.Context, hash []byte) (Session, error)

	// ClaimSession marks a session used, but only if it is currently
	// unused, unrevoked, unexpired and its family is not revoked. It
	// returns ErrNotFound when the claim loses that race — which the caller
	// must treat as reuse.
	//
	// The family half of that condition is what closes known issue #11. A
	// successor inserted after its family was revoked carries revoked_at
	// NULL, so its own row says nothing; the family is read fresh here.
	ClaimSession(ctx context.Context, id uuid.UUID) (Session, error)

	RevokeSessionFamily(ctx context.Context, familyID uuid.UUID) (int64, error)
	RevokeSession(ctx context.Context, id uuid.UUID) (int64, error)
	RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) (int64, error)
	DeleteExpiredSessions(ctx context.Context) (int64, error)

	RecordSecurityEvent(ctx context.Context, e SecurityEvent) error
}
