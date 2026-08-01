package accounts

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
)

// MemoryStore is an in-memory Store for tests, matching the pattern used by
// auth.MemoryStore and storage/local: the suite exercises the real service
// with no database and no network.
//
// It reproduces the behaviours the service depends on — provider-id
// uniqueness, ownership scoping on every read, and single-use OAuth state.
type MemoryStore struct {
	mu       sync.Mutex
	accounts map[uuid.UUID]StoredAccount
	capacity map[uuid.UUID]Capacity
	states   map[string]pendingState
	clock    func() time.Time
}

type pendingState struct {
	pending   PendingOAuth
	expiresAt time.Time
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accounts: map[uuid.UUID]StoredAccount{},
		capacity: map[uuid.UUID]Capacity{},
		states:   map[string]pendingState{},
		clock:    time.Now,
	}
}

// CreateAccount links a provider account, rejecting a duplicate provider id.
func (m *MemoryStore) CreateAccount(_ context.Context, n NewAccount) (StoredAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, a := range m.accounts {
		if a.UserID == n.UserID && a.Kind == n.Kind && a.ProviderAccountID == n.ProviderAccountID {
			return StoredAccount{}, skerr.ErrConflict
		}
	}
	acct := StoredAccount{
		Account: Account{
			ID:           n.ID,
			UserID:       n.UserID,
			Kind:         n.Kind,
			Email:        n.Email,
			DisplayName:  n.DisplayName,
			Status:       StatusActive,
			Ordinal:      n.Ordinal,
			CreatedAt:    m.clock(),
			TokenExpires: n.TokenExpiresAt,
		},
		ProviderAccountID: n.ProviderAccountID,
		AccessTokenEnc:    n.AccessTokenEnc,
		RefreshTokenEnc:   n.RefreshTokenEnc,
	}
	m.accounts[n.ID] = acct
	return acct, nil
}

// UpdateAccountTokens refreshes stored credentials, scoped to the owner.
func (m *MemoryStore) UpdateAccountTokens(_ context.Context, u TokenUpdate) (StoredAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	acct, ok := m.accounts[u.ID]
	if !ok || acct.UserID != u.UserID {
		return StoredAccount{}, skerr.ErrNotFound
	}
	acct.AccessTokenEnc = u.AccessTokenEnc
	// COALESCE semantics: a provider that omits the refresh token on a
	// refresh must not wipe the one already stored.
	if len(u.RefreshTokenEnc) > 0 {
		acct.RefreshTokenEnc = u.RefreshTokenEnc
	}
	acct.TokenExpires = u.TokenExpiresAt
	acct.Email = u.Email
	acct.DisplayName = u.DisplayName
	acct.Status = StatusActive
	acct.LastError = ""
	m.accounts[u.ID] = acct
	return acct, nil
}

// GetAccount loads one account the user owns.
func (m *MemoryStore) GetAccount(_ context.Context, userID, id uuid.UUID) (StoredAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[id]
	if !ok || acct.UserID != userID {
		return StoredAccount{}, skerr.ErrNotFound
	}
	return acct, nil
}

// GetAccountByProviderID looks up by the provider's own identifier.
func (m *MemoryStore) GetAccountByProviderID(_ context.Context, userID uuid.UUID, kind storage.Kind, providerID string) (StoredAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.UserID == userID && a.Kind == kind && a.ProviderAccountID == providerID {
			return a, nil
		}
	}
	return StoredAccount{}, skerr.ErrNotFound
}

// ListAccounts returns one user's accounts.
func (m *MemoryStore) ListAccounts(_ context.Context, userID uuid.UUID) ([]StoredAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []StoredAccount{}
	for _, a := range m.accounts {
		if a.UserID == userID {
			out = append(out, a)
		}
	}
	return out, nil
}

// ListAccountsForSync returns every active account.
func (m *MemoryStore) ListAccountsForSync(context.Context) ([]StoredAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []StoredAccount{}
	for _, a := range m.accounts {
		if a.Status == StatusActive {
			out = append(out, a)
		}
	}
	return out, nil
}

// NextOrdinal returns the next colour-ramp position.
func (m *MemoryStore) NextOrdinal(_ context.Context, userID uuid.UUID) (int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var maxOrdinal int32
	for _, a := range m.accounts {
		if a.UserID == userID && a.Ordinal > maxOrdinal {
			maxOrdinal = a.Ordinal
		}
	}
	return maxOrdinal + 1, nil
}

// SetAccountStatus records a status change.
func (m *MemoryStore) SetAccountStatus(_ context.Context, id uuid.UUID, status, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[id]
	if !ok {
		return skerr.ErrNotFound
	}
	acct.Status = status
	acct.LastError = lastErr
	m.accounts[id] = acct
	return nil
}

// GetAppFolderID returns the stored app folder id, or "" if none.
func (m *MemoryStore) GetAppFolderID(_ context.Context, id uuid.UUID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[id]
	if !ok {
		return "", skerr.ErrNotFound
	}
	return acct.AppFolderID, nil
}

// SetAppFolderID mirrors the conditional UPDATE: it writes only when no folder
// is recorded yet, and reports ErrNotFound when another writer won. Holding the
// lock across the check and the write is what makes it atomic here, exactly as
// the WHERE clause does in Postgres.
func (m *MemoryStore) SetAppFolderID(_ context.Context, id uuid.UUID, folderID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[id]
	if !ok {
		return "", skerr.ErrNotFound
	}
	if acct.AppFolderID != "" {
		return "", skerr.ErrNotFound
	}
	acct.AppFolderID = folderID
	m.accounts[id] = acct
	return folderID, nil
}

// ClearAccountTokens wipes stored credentials, leaving the row and its id in
// place. See the Store interface, and Service.Disconnect for why the row has
// to survive.
func (m *MemoryStore) ClearAccountTokens(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[id]
	if !ok {
		return skerr.ErrNotFound
	}
	acct.AccessTokenEnc = nil
	acct.RefreshTokenEnc = nil
	m.accounts[id] = acct
	return nil
}

// DeleteAccount unlinks an account the user owns.
func (m *MemoryStore) DeleteAccount(_ context.Context, userID, id uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[id]
	if !ok || acct.UserID != userID {
		return 0, nil
	}
	delete(m.accounts, id)
	delete(m.capacity, id)
	return 1, nil
}

// UpsertCapacity records a quota reading.
func (m *MemoryStore) UpsertCapacity(_ context.Context, accountID uuid.UUID, total, used int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.capacity[accountID]
	c.TotalBytes = total
	c.UsedBytes = used
	now := m.clock()
	c.LastSyncedAt = &now
	c.Account.LastError = ""
	m.capacity[accountID] = c
	return nil
}

// SetCapacityError records why a quota read failed.
func (m *MemoryStore) SetCapacityError(_ context.Context, accountID uuid.UUID, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.capacity[accountID]
	c.Account.LastError = msg
	m.capacity[accountID] = c
	return nil
}

// ListCapacity returns per-account capacity for one user.
func (m *MemoryStore) ListCapacity(_ context.Context, userID uuid.UUID) ([]Capacity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Capacity{}
	for id, a := range m.accounts {
		if a.UserID != userID {
			continue
		}
		c := m.capacity[id]
		c.Account = a.Account
		out = append(out, c)
	}
	return out, nil
}

// SetReserved sets an account's reserved bytes. Test support for the
// reservation paths in Phase 5.
func (m *MemoryStore) SetReserved(accountID uuid.UUID, reserved int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.capacity[accountID]
	c.ReservedBytes = reserved
	m.capacity[accountID] = c
}

// CreateOAuthState stores a pending authorisation by state hash.
func (m *MemoryStore) CreateOAuthState(_ context.Context, stateHash []byte, p PendingOAuth, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[string(stateHash)] = pendingState{pending: p, expiresAt: expiresAt}
	return nil
}

// ConsumeOAuthState reads and deletes in one step, so a state is single use.
func (m *MemoryStore) ConsumeOAuthState(_ context.Context, stateHash []byte) (PendingOAuth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[string(stateHash)]
	if !ok {
		return PendingOAuth{}, skerr.ErrNotFound
	}
	delete(m.states, string(stateHash))
	if !st.expiresAt.After(m.clock()) {
		return PendingOAuth{}, skerr.ErrNotFound
	}
	return st.pending, nil
}

// DeleteExpiredOAuthStates clears abandoned authorisations.
func (m *MemoryStore) DeleteExpiredOAuthStates(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, st := range m.states {
		if !st.expiresAt.After(m.clock()) {
			delete(m.states, k)
			n++
		}
	}
	return n, nil
}

// PendingStateCount reports how many authorisations are outstanding. Test
// support.
func (m *MemoryStore) PendingStateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.states)
}

// Compile-time check that the double still satisfies the interface.
var _ Store = (*MemoryStore)(nil)

// HasStateHash reports whether a state is stored under exactly these bytes.
// Test support; mirrors SQLiteStore.HasStateHash so both stores can be driven
// by the same assertions.
func (m *MemoryStore) HasStateHash(hash []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.states[string(hash)]
	return ok
}

// PendingVerifiers returns the PKCE verifier of every outstanding state. Test
// support.
func (m *MemoryStore) PendingVerifiers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.states))
	for _, st := range m.states {
		out = append(out, st.pending.PKCEVerifier)
	}
	return out
}
