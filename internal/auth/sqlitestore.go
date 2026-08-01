package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/db/gensqlite"
	"github.com/mridul60214/skein/internal/skerr"
)

// timeLayout is how every timestamp is stored. RFC 3339 with nanoseconds, in
// UTC, fixed width: SQLite has no date type, and text comparison is what
// `expires_at > ?` relies on, so the format has to sort lexicographically in
// the same order it sorts chronologically. Fixed-width UTC does; anything
// carrying an offset does not ("2026-01-01T00:00:00+05:30" sorts before
// "2026-01-01T00:00:00Z" but is later in time).
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteStore is the SQLite implementation of Store, for the desktop build
// (Phase 7 Task 3). It satisfies the same interface as PGStore and is verified
// against the same test suite -- see storeconformance_test.go.
//
// It carries more conversion code than PGStore because SQLite has no UUID,
// timestamp, inet or JSON types: every one of those is TEXT on disk, so the
// parsing lives here rather than in sqlc's type overrides. That is deliberate.
// A malformed timestamp read back from a file the user can open and edit
// themselves is a plausible failure, and it should surface as a handled error
// from this layer rather than a panic from generated code.
type SQLiteStore struct {
	q *gensqlite.Queries

	// now is the clock. Postgres queries call now() server-side; the SQLite
	// dialect passes timestamps in as parameters, which means this store
	// decides what "now" is and a test can replace it.
	now func() time.Time
}

// NewSQLiteStore wraps an open SQLite database.
//
// The caller owns the *sql.DB and its pragmas. Callers that write concurrently
// want `_pragma=busy_timeout(5000)` and `_pragma=journal_mode(WAL)` in the DSN;
// see OpenSQLite, which applies both.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{q: gensqlite.New(db), now: time.Now}
}

// OpenSQLite opens a SQLite database with the pragmas this store expects.
//
// busy_timeout is not optional. SQLite permits one writer at a time and
// returns SQLITE_BUSY immediately to the loser unless a timeout is set, which
// would turn ordinary write contention into spurious errors. Five seconds is
// the value Phase7 Task 3 specifies.
//
// foreign_keys is off by default in SQLite -- unlike Postgres, where it is not
// even a question. sessions.family_id references token_families and
// sessions.user_id references users; without this the schema's referential
// integrity is decoration.
func OpenSQLite(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, nil
}

// CreateUser inserts a user, mapping a duplicate email to ErrConflict.
func (s *SQLiteStore) CreateUser(ctx context.Context, id uuid.UUID, email, passwordHash string) (User, error) {
	now := s.fmtTime(s.now())
	row, err := s.q.CreateUser(ctx, gensqlite.CreateUserParams{
		ID:           id.String(),
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, skerr.ErrConflict
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return s.toUser(row)
}

// GetUserByEmail loads a user by address. Case-insensitivity comes from the
// column's NOCASE collation, matching Postgres citext.
func (s *SQLiteStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	row, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		return User{}, mapSQLiteNoRows(err, "select user by email")
	}
	return s.toUser(row)
}

// GetUserByID loads a user by id.
func (s *SQLiteStore) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.GetUserByID(ctx, id.String())
	if err != nil {
		return User{}, mapSQLiteNoRows(err, "select user by id")
	}
	return s.toUser(row)
}

// UpdateUserPassword replaces a stored password hash.
func (s *SQLiteStore) UpdateUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	if err := s.q.UpdateUserPassword(ctx, gensqlite.UpdateUserPasswordParams{
		ID:           id.String(),
		PasswordHash: passwordHash,
		UpdatedAt:    s.fmtTime(s.now()),
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return nil
}

// CreateTokenFamily records a new login's family.
func (s *SQLiteStore) CreateTokenFamily(ctx context.Context, familyID, userID uuid.UUID) error {
	if err := s.q.CreateTokenFamily(ctx, gensqlite.CreateTokenFamilyParams{
		ID:        familyID.String(),
		UserID:    userID.String(),
		CreatedAt: s.fmtTime(s.now()),
	}); err != nil {
		return fmt.Errorf("create token family: %w", err)
	}
	return nil
}

// RevokeTokenFamily marks a family revoked. This is the write that actually
// stops future refreshes; see the comment on Store.RevokeTokenFamily.
func (s *SQLiteStore) RevokeTokenFamily(ctx context.Context, familyID uuid.UUID) (int64, error) {
	n, err := s.q.RevokeTokenFamily(ctx, gensqlite.RevokeTokenFamilyParams{
		RevokedAt: s.fmtTimePtr(s.now()),
		ID:        familyID.String(),
	})
	if err != nil {
		return 0, fmt.Errorf("revoke token family: %w", err)
	}
	return n, nil
}

// CreateSession inserts one issued refresh token.
func (s *SQLiteStore) CreateSession(ctx context.Context, n NewSession) (Session, error) {
	row, err := s.q.CreateSession(ctx, gensqlite.CreateSessionParams{
		ID:          n.ID.String(),
		UserID:      n.UserID.String(),
		FamilyID:    n.FamilyID.String(),
		PrevID:      uuidPtrToString(n.PrevID),
		RefreshHash: n.RefreshHash,
		UserAgent:   truncate(n.UserAgent, 512),
		Ip:          addrPtrToString(n.IP),
		CreatedAt:   s.fmtTime(s.now()),
		ExpiresAt:   s.fmtTime(n.ExpiresAt),
	})
	if err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	return s.toSession(row)
}

// GetSessionByRefreshHash looks up a session by token hash, including rows
// that are used, revoked or expired.
func (s *SQLiteStore) GetSessionByRefreshHash(ctx context.Context, hash []byte) (Session, error) {
	row, err := s.q.GetSessionByRefreshHash(ctx, hash)
	if err != nil {
		return Session{}, mapSQLiteNoRows(err, "select session by hash")
	}
	return s.toSession(row)
}

// ClaimSession marks a session used if and only if it is currently claimable.
// Zero rows means the caller lost the race, which the service reads as reuse.
func (s *SQLiteStore) ClaimSession(ctx context.Context, id uuid.UUID) (Session, error) {
	now := s.now()
	row, err := s.q.MarkSessionUsed(ctx, gensqlite.MarkSessionUsedParams{
		UsedAt:    s.fmtTimePtr(now),
		ID:        id.String(),
		ExpiresAt: s.fmtTime(now),
	})
	if err != nil {
		return Session{}, mapSQLiteNoRows(err, "claim session")
	}
	return s.toSession(row)
}

// RevokeSessionFamily revokes every unrevoked session sharing a family id.
func (s *SQLiteStore) RevokeSessionFamily(ctx context.Context, familyID uuid.UUID) (int64, error) {
	n, err := s.q.RevokeSessionFamily(ctx, gensqlite.RevokeSessionFamilyParams{
		RevokedAt: s.fmtTimePtr(s.now()),
		FamilyID:  familyID.String(),
	})
	if err != nil {
		return 0, fmt.Errorf("revoke family: %w", err)
	}
	return n, nil
}

// RevokeSession revokes a single session.
func (s *SQLiteStore) RevokeSession(ctx context.Context, id uuid.UUID) (int64, error) {
	n, err := s.q.RevokeSession(ctx, gensqlite.RevokeSessionParams{
		RevokedAt: s.fmtTimePtr(s.now()),
		ID:        id.String(),
	})
	if err != nil {
		return 0, fmt.Errorf("revoke session: %w", err)
	}
	return n, nil
}

// RevokeAllUserSessions signs a user out everywhere.
func (s *SQLiteStore) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) (int64, error) {
	n, err := s.q.RevokeAllUserSessions(ctx, gensqlite.RevokeAllUserSessionsParams{
		RevokedAt: s.fmtTimePtr(s.now()),
		UserID:    userID.String(),
	})
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	return n, nil
}

// DeleteExpiredSessions removes sessions long past expiry.
//
// The 30-day retention window is computed here rather than in SQL. The
// Postgres query says `now() - INTERVAL '30 days'`; keeping the number in Go
// means the two dialects cannot drift apart on it.
func (s *SQLiteStore) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-30 * 24 * time.Hour)
	n, err := s.q.DeleteExpiredSessions(ctx, s.fmtTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return n, nil
}

// RecordSecurityEvent appends an audit record.
func (s *SQLiteStore) RecordSecurityEvent(ctx context.Context, e SecurityEvent) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		// Never block the audit write on an unserialisable detail map.
		detail = []byte(`{"detail_encode_error":true}`)
	}
	if err := s.q.RecordSecurityEvent(ctx, gensqlite.RecordSecurityEventParams{
		UserID:    uuidPtrToString(e.UserID),
		Kind:      e.Kind,
		Detail:    string(detail),
		Ip:        addrPtrToString(e.IP),
		UserAgent: truncate(e.UserAgent, 512),
		CreatedAt: s.fmtTime(s.now()),
	}); err != nil {
		return fmt.Errorf("insert security event: %w", err)
	}
	return nil
}

// --- conversions -----------------------------------------------------------

func (s *SQLiteStore) fmtTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func (s *SQLiteStore) fmtTimePtr(t time.Time) *string {
	v := s.fmtTime(t)
	return &v
}

func (s *SQLiteStore) toUser(r gensqlite.User) (User, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return User{}, fmt.Errorf("parse user id %q: %w", r.ID, err)
	}
	created, err := parseTime(r.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("parse user created_at: %w", err)
	}
	verified, err := parseTimePtr(r.EmailVerifiedAt)
	if err != nil {
		return User{}, fmt.Errorf("parse user email_verified_at: %w", err)
	}
	return User{
		ID:              id,
		Email:           r.Email,
		PasswordHash:    r.PasswordHash,
		EmailVerifiedAt: verified,
		CreatedAt:       created,
	}, nil
}

func (s *SQLiteStore) toSession(r gensqlite.Session) (Session, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return Session{}, fmt.Errorf("parse session id %q: %w", r.ID, err)
	}
	userID, err := uuid.Parse(r.UserID)
	if err != nil {
		return Session{}, fmt.Errorf("parse session user_id: %w", err)
	}
	familyID, err := uuid.Parse(r.FamilyID)
	if err != nil {
		return Session{}, fmt.Errorf("parse session family_id: %w", err)
	}
	prevID, err := parseUUIDPtr(r.PrevID)
	if err != nil {
		return Session{}, fmt.Errorf("parse session prev_id: %w", err)
	}
	created, err := parseTime(r.CreatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse session created_at: %w", err)
	}
	expires, err := parseTime(r.ExpiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse session expires_at: %w", err)
	}
	usedAt, err := parseTimePtr(r.UsedAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse session used_at: %w", err)
	}
	revokedAt, err := parseTimePtr(r.RevokedAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse session revoked_at: %w", err)
	}
	return Session{
		ID:        id,
		UserID:    userID,
		FamilyID:  familyID,
		PrevID:    prevID,
		UserAgent: r.UserAgent,
		IP:        parseAddrPtr(r.Ip),
		CreatedAt: created,
		ExpiresAt: expires,
		UsedAt:    usedAt,
		RevokedAt: revokedAt,
	}, nil
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

func parseTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := parseTime(*s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func parseUUIDPtr(s *string) (*uuid.UUID, error) {
	if s == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// parseAddrPtr mirrors ParseIP: an unreadable address becomes NULL rather than
// a wrong value, and never an error -- an audit row with no IP is better than
// a failed read.
func parseAddrPtr(s *string) *netip.Addr {
	if s == nil {
		return nil
	}
	addr, err := netip.ParseAddr(*s)
	if err != nil {
		return nil
	}
	return &addr
}

func uuidPtrToString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	v := id.String()
	return &v
}

func addrPtrToString(a *netip.Addr) *string {
	if a == nil {
		return nil
	}
	v := a.String()
	return &v
}

func mapSQLiteNoRows(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return skerr.ErrNotFound
	}
	return fmt.Errorf("%s: %w", what, err)
}

// isUniqueViolation reports a UNIQUE constraint failure.
//
// modernc.org/sqlite returns a driver-specific error type whose numeric code
// is not exported in a form worth depending on, so this matches the message.
// SQLite's wording for this has been stable for many years and the string is
// not localised.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// --- test support ----------------------------------------------------------
//
// Mirrors of the inspection helpers on MemoryStore, so the same test suite can
// drive either store (see storeconformance_test.go). Read-only apart from
// ExpireSession, and no production code calls them.
//
// They panic on a malformed row rather than returning an error, matching the
// MemoryStore signatures they stand in for. That is safe here because the only
// writer of these rows is this same store: a parse failure would mean the
// store's own round trip is broken, which is a test bug worth crashing on.

// SessionByID returns one session. Test support.
func (s *SQLiteStore) SessionByID(id uuid.UUID) (Session, bool) {
	row, err := s.q.GetSessionByID(context.Background(), id.String())
	if err != nil {
		return Session{}, false
	}
	sess, err := s.toSession(row)
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: unreadable session row %s: %v", id, err))
	}
	return sess, true
}

// SessionsInFamily returns every session belonging to a family. Test support.
func (s *SQLiteStore) SessionsInFamily(familyID uuid.UUID) []Session {
	rows, err := s.q.SessionsInFamily(context.Background(), familyID.String())
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: list family %s: %v", familyID, err))
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		sess, cerr := s.toSession(r)
		if cerr != nil {
			panic(fmt.Sprintf("sqlitestore: unreadable session row: %v", cerr))
		}
		out = append(out, sess)
	}
	return out
}

// FamilyRevokedAt reports when a family was revoked, and whether it exists.
// Test support.
func (s *SQLiteStore) FamilyRevokedAt(familyID uuid.UUID) (*time.Time, bool) {
	raw, err := s.q.FamilyRevokedAt(context.Background(), familyID.String())
	if err != nil {
		return nil, false
	}
	at, err := parseTimePtr(raw)
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: unreadable family revoked_at: %v", err))
	}
	return at, true
}

// EventsOfKind returns audit rows of one kind, oldest first. Test support.
func (s *SQLiteStore) EventsOfKind(kind string) []SecurityEvent {
	rows, err := s.q.EventsOfKind(context.Background(), kind)
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: list events %q: %v", kind, err))
	}
	out := make([]SecurityEvent, 0, len(rows))
	for _, r := range rows {
		userID, perr := parseUUIDPtr(r.UserID)
		if perr != nil {
			panic(fmt.Sprintf("sqlitestore: unreadable event user_id: %v", perr))
		}
		var detail map[string]any
		if r.Detail != "" {
			_ = json.Unmarshal([]byte(r.Detail), &detail)
		}
		out = append(out, SecurityEvent{
			UserID:    userID,
			Kind:      r.Kind,
			Detail:    detail,
			IP:        parseAddrPtr(r.Ip),
			UserAgent: r.UserAgent,
		})
	}
	return out
}

// ExpireSession backdates a session so expiry paths can be exercised without
// sleeping. Test support.
func (s *SQLiteStore) ExpireSession(id uuid.UUID) error {
	n, err := s.q.ExpireSession(context.Background(), gensqlite.ExpireSessionParams{
		ExpiresAt: s.fmtTime(s.now().Add(-time.Minute)),
		ID:        id.String(),
	})
	if err != nil {
		return fmt.Errorf("expire session: %w", err)
	}
	if n == 0 {
		return skerr.ErrNotFound
	}
	return nil
}

// Compile-time proof that this store satisfies the interface it implements.
var _ Store = (*SQLiteStore)(nil)
