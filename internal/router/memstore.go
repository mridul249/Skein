package router

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is an in-memory Store for tests.
//
// The one behaviour it has to reproduce faithfully is the atomicity of
// Reserve: the check and the increment happen under one lock, exactly as the
// conditional UPDATE does under one statement. A double that checks and then
// increments separately would let the concurrency tests pass against a
// scheme that is actually racy, which would make them worse than useless.
type MemoryStore struct {
	mu       sync.Mutex
	accounts map[uuid.UUID]*Candidate
	held     map[uuid.UUID][]heldReservation
	clock    func() time.Time
}

type heldReservation struct {
	id        uuid.UUID
	accountID uuid.UUID
	bytes     int64
	expiresAt time.Time
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accounts: map[uuid.UUID]*Candidate{},
		held:     map[uuid.UUID][]heldReservation{},
		clock:    time.Now,
	}
}

// AddAccount registers a candidate with the given capacity. Test support.
func (m *MemoryStore) AddAccount(id uuid.UUID, ordinal int32, email string, total, used int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[id] = &Candidate{
		AccountID: id, Ordinal: ordinal, Email: email, Total: total, Used: used,
	}
}

// Candidates returns the registered accounts, most free space first.
func (m *MemoryStore) Candidates(context.Context, uuid.UUID) ([]Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Candidate, 0, len(m.accounts))
	for _, c := range m.accounts {
		out = append(out, *c)
	}
	return out, nil
}

// Reserve claims capacity. The check and the increment are one critical
// section, mirroring the single-statement UPDATE in Postgres.
func (m *MemoryStore) Reserve(_ context.Context, accountID uuid.UUID, bytes int64, uploadID uuid.UUID, expiresAt time.Time) (Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	acct, ok := m.accounts[accountID]
	if !ok {
		return Reservation{}, ErrNoCapacity
	}
	if acct.Total-acct.Used-acct.Reserved < bytes {
		return Reservation{}, ErrNoCapacity
	}

	acct.Reserved += bytes
	id := uuid.New()
	m.held[uploadID] = append(m.held[uploadID], heldReservation{
		id: id, accountID: accountID, bytes: bytes, expiresAt: expiresAt,
	})
	return Reservation{ID: id, AccountID: accountID, Bytes: bytes}, nil
}

// Release gives back an upload's reservations. Calling it twice is a no-op.
func (m *MemoryStore) Release(_ context.Context, uploadID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := m.held[uploadID]
	delete(m.held, uploadID)

	for _, r := range list {
		m.creditLocked(r.accountID, r.bytes)
	}
	return int64(len(list)), nil
}

// ReclaimExpired releases everything past its expiry.
func (m *MemoryStore) ReclaimExpired(context.Context) (int, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	var count int
	var bytes int64

	for uploadID, list := range m.held {
		var keep []heldReservation
		for _, r := range list {
			if r.expiresAt.After(now) {
				keep = append(keep, r)
				continue
			}
			m.creditLocked(r.accountID, r.bytes)
			count++
			bytes += r.bytes
		}
		if len(keep) == 0 {
			delete(m.held, uploadID)
		} else {
			m.held[uploadID] = keep
		}
	}
	return count, bytes, nil
}

// creditLocked returns bytes to an account, never below zero. The floor
// matters: without it a double release inflates free space for everyone.
func (m *MemoryStore) creditLocked(accountID uuid.UUID, bytes int64) {
	acct, ok := m.accounts[accountID]
	if !ok {
		return
	}
	acct.Reserved -= bytes
	if acct.Reserved < 0 {
		acct.Reserved = 0
	}
}

// ReservedOn reports an account's currently held bytes. Test support.
func (m *MemoryStore) ReservedOn(accountID uuid.UUID) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if acct, ok := m.accounts[accountID]; ok {
		return acct.Reserved
	}
	return 0
}

// Expire backdates every reservation an upload holds. Test support.
func (m *MemoryStore) Expire(uploadID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.held[uploadID] {
		m.held[uploadID][i].expiresAt = m.clock().Add(-time.Hour)
	}
}

// Compile-time check.
var _ Store = (*MemoryStore)(nil)
