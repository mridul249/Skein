package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
)

// MemoryStore is an in-memory Store.
//
// It exists for the same reason storage/local does: the test suite needs to
// exercise the real service with no network and no database, and a double that
// ships with the package is one that stays in step with the interface.
//
// It reproduces the two behaviours the service actually depends on — unique
// emails, and a ClaimSession that can succeed at most once per session. It is
// not a persistence layer and must never be wired into main.
type MemoryStore struct {
	mu       sync.Mutex
	users    map[uuid.UUID]User
	byEmail  map[string]uuid.UUID
	sessions map[uuid.UUID]Session
	byHash   map[string]uuid.UUID
	// families mirrors token_families. The value is the revocation time, nil
	// while the family is live.
	families map[uuid.UUID]*time.Time
	events   []SecurityEvent
	clock    func() time.Time
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:    map[uuid.UUID]User{},
		byEmail:  map[string]uuid.UUID{},
		sessions: map[uuid.UUID]Session{},
		byHash:   map[string]uuid.UUID{},
		families: map[uuid.UUID]*time.Time{},
		clock:    time.Now,
	}
}

// CreateUser inserts a user, rejecting a duplicate address.
func (m *MemoryStore) CreateUser(_ context.Context, id uuid.UUID, email, hash string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.ToLower(email)
	if _, exists := m.byEmail[key]; exists {
		return User{}, skerr.ErrConflict
	}
	// session_epoch DEFAULT 1 in both dialects. Starting at 0 here would make
	// the double disagree with the schema about what a fresh user looks like.
	u := User{ID: id, Email: key, PasswordHash: hash, CreatedAt: m.clock(), SessionEpoch: 1}
	m.users[id] = u
	m.byEmail[key] = id
	return u, nil
}

// GetUserByEmail loads a user by address.
func (m *MemoryStore) GetUserByEmail(_ context.Context, email string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[strings.ToLower(email)]
	if !ok {
		return User{}, skerr.ErrNotFound
	}
	return m.users[id], nil
}

// GetUserByID loads a user by id.
func (m *MemoryStore) GetUserByID(_ context.Context, id uuid.UUID) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, skerr.ErrNotFound
	}
	return u, nil
}

// UpdateUserPassword replaces a stored password hash.
func (m *MemoryStore) UpdateUserPassword(_ context.Context, id uuid.UUID, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return skerr.ErrNotFound
	}
	u.PasswordHash = hash
	m.users[id] = u
	return nil
}

// CreateSession records one issued refresh token.
// BumpUserSessionEpoch invalidates every session the user has by superseding
// the epoch they were born under. It enumerates nothing, which is the whole
// point -- see the interface doc and known issue #18.
func (m *MemoryStore) BumpUserSessionEpoch(_ context.Context, userID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return 0, skerr.ErrNotFound
	}
	u.SessionEpoch++
	m.users[userID] = u
	return u.SessionEpoch, nil
}

// CreateTokenFamily records a family. Mirrors the FK ordering the real schema
// enforces: a session cannot be inserted before its family exists.
func (m *MemoryStore) CreateTokenFamily(_ context.Context, familyID, _ uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.families[familyID]; !exists {
		m.families[familyID] = nil
	}
	return nil
}

// RevokeTokenFamily marks a family revoked, preserving the first revocation.
func (m *MemoryStore) RevokeTokenFamily(_ context.Context, familyID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at, ok := m.families[familyID]; ok && at != nil {
		return 0, nil // already revoked; do not move the timestamp
	}
	t := m.clock()
	m.families[familyID] = &t
	return 1, nil
}

// FamilyRevokedAt reports a family's revocation time. Test support.
func (m *MemoryStore) FamilyRevokedAt(familyID uuid.UUID) (*time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.families[familyID]
	return at, ok
}

func (m *MemoryStore) CreateSession(_ context.Context, n NewSession) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, clash := m.byHash[string(n.RefreshHash)]; clash {
		return Session{}, skerr.ErrConflict
	}
	s := Session{
		ID:        n.ID,
		UserID:    n.UserID,
		FamilyID:  n.FamilyID,
		PrevID:    n.PrevID,
		UserAgent: n.UserAgent,
		IP:        n.IP,
		CreatedAt: m.clock(),
		ExpiresAt: n.ExpiresAt,
		// Stored as supplied. This store must not read the user's current
		// epoch here for the same reason the SQL does not: the value is
		// INHERITED from the claimed parent, and re-reading it reproduces
		// issue #18.
		Epoch: n.Epoch,
	}
	m.sessions[s.ID] = s
	m.byHash[string(n.RefreshHash)] = s.ID
	return s, nil
}

// GetSessionByRefreshHash returns a session including used, revoked and
// expired rows, so the caller can distinguish reuse from an unknown token.
func (m *MemoryStore) GetSessionByRefreshHash(_ context.Context, hash []byte) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byHash[string(hash)]
	if !ok {
		return Session{}, skerr.ErrNotFound
	}
	return m.sessions[id], nil
}

// ClaimSession mirrors the SQL predicate used_at IS NULL AND revoked_at IS
// NULL AND expires_at > now(). Holding the lock across the read and the write
// is what makes it atomic here, exactly as the conditional UPDATE is in
// Postgres.
func (m *MemoryStore) ClaimSession(_ context.Context, id uuid.UUID) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.UsedAt != nil || s.RevokedAt != nil || !s.ExpiresAt.After(m.clock()) {
		return Session{}, skerr.ErrNotFound
	}
	// The family half of the two-part condition, mirroring the NOT EXISTS in
	// MarkSessionUsed. Known issue #11: a successor inserted after its
	// family was revoked has RevokedAt nil, so the check above passes and
	// only this one stops it. If these two predicates ever diverge, the
	// doubles validate against semantics the database no longer has.
	if at, exists := m.families[s.FamilyID]; exists && at != nil {
		return Session{}, skerr.ErrNotFound
	}
	// The epoch half, mirroring the EXISTS (... u.session_epoch =
	// sessions.epoch) in MarkSessionUsed. A session created before a
	// user-level revocation carries a stale epoch, and this is the only place
	// that is enforced -- its own row says nothing.
	if u, exists := m.users[s.UserID]; !exists || u.SessionEpoch != s.Epoch {
		return Session{}, skerr.ErrNotFound
	}
	t := m.clock()
	s.UsedAt = &t
	m.sessions[id] = s
	return s, nil
}

// RevokeSessionFamily revokes every unrevoked session sharing a family id.
func (m *MemoryStore) RevokeSessionFamily(_ context.Context, familyID uuid.UUID) (int64, error) {
	return m.revokeWhere(func(s Session) bool { return s.FamilyID == familyID }), nil
}

// RevokeSession revokes a single session.
func (m *MemoryStore) RevokeSession(_ context.Context, id uuid.UUID) (int64, error) {
	return m.revokeWhere(func(s Session) bool { return s.ID == id }), nil
}

// RevokeAllUserSessions signs a user out everywhere.
func (m *MemoryStore) RevokeAllUserSessions(_ context.Context, userID uuid.UUID) (int64, error) {
	return m.revokeWhere(func(s Session) bool { return s.UserID == userID }), nil
}

func (m *MemoryStore) revokeWhere(match func(Session) bool) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	var n int64
	for id, s := range m.sessions {
		if s.RevokedAt == nil && match(s) {
			t := now
			s.RevokedAt = &t
			m.sessions[id] = s
			n++
		}
	}
	return n
}

// DeleteExpiredSessions removes sessions long past expiry.
func (m *MemoryStore) DeleteExpiredSessions(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.clock().Add(-30 * 24 * time.Hour)
	var n int64
	for id, s := range m.sessions {
		if s.ExpiresAt.Before(cutoff) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

// RecordSecurityEvent appends an audit record.
func (m *MemoryStore) RecordSecurityEvent(_ context.Context, e SecurityEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

// EventsOfKind returns the recorded events with the given kind. Test support.
func (m *MemoryStore) EventsOfKind(kind string) []SecurityEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SecurityEvent
	for _, e := range m.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// SessionsInFamily returns every session belonging to a family. Test support.
func (m *MemoryStore) SessionsInFamily(familyID uuid.UUID) []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.FamilyID == familyID {
			out = append(out, s)
		}
	}
	return out
}

// SessionByID returns one session. Test support.
func (m *MemoryStore) SessionByID(id uuid.UUID) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// ExpireSession backdates a session so expiry paths can be exercised without
// sleeping. Test support.
func (m *MemoryStore) ExpireSession(id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return skerr.ErrNotFound
	}
	s.ExpiresAt = m.clock().Add(-time.Minute)
	m.sessions[id] = s
	return nil
}

// Compile-time proof that the double still satisfies the interface it doubles.
var _ Store = (*MemoryStore)(nil)
