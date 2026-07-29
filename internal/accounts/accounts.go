// Package accounts owns connected provider accounts: linking them over OAuth,
// storing their credentials as ciphertext, and keeping their reported capacity
// fresh.
package accounts

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/storage"
)

// Status values for a connected account.
const (
	StatusActive      = "active"
	StatusNeedsReauth = "needs_reauth"
	StatusDisabled    = "disabled"
)

// Account is one connected provider account, with credentials omitted. This is
// the type that reaches handlers, so there is no path by which a token becomes
// part of a response.
type Account struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	Kind         storage.Kind
	Email        string
	DisplayName  string
	Status       string
	LastError    string
	Ordinal      int32
	CreatedAt    time.Time
	TokenExpires *time.Time
}

// Credentials is the decrypted token set for one account. It is produced only
// inside this package and never crosses into the HTTP layer.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
}

// Capacity is an account's reported storage, as shown in the quota rail.
type Capacity struct {
	Account       Account
	TotalBytes    int64
	UsedBytes     int64
	ReservedBytes int64
	LastSyncedAt  *time.Time
}

// FreeBytes is what an upload may actually claim: reported free space minus
// what concurrent uploads have already reserved.
func (c Capacity) FreeBytes() int64 {
	free := c.TotalBytes - c.UsedBytes - c.ReservedBytes
	if free < 0 {
		return 0
	}
	return free
}

// Pool is the aggregate view across every account, which is the number the
// product exists to make meaningful.
type Pool struct {
	TotalBytes int64
	UsedBytes  int64
	FreeBytes  int64
	Accounts   []Capacity
}

// NewAccount is the input to Store.CreateAccount. Token fields are already
// ciphertext; this package encrypts before it reaches the store, so the store
// never handles a plaintext credential.
type NewAccount struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Kind              storage.Kind
	ProviderAccountID string
	Email             string
	DisplayName       string
	AccessTokenEnc    []byte
	RefreshTokenEnc   []byte
	TokenExpiresAt    *time.Time
	Ordinal           int32
}

// TokenUpdate refreshes the stored credentials of a linked account.
type TokenUpdate struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	AccessTokenEnc  []byte
	RefreshTokenEnc []byte
	TokenExpiresAt  *time.Time
	Email           string
	DisplayName     string
}

// StoredAccount is an account as persisted, including its ciphertext.
type StoredAccount struct {
	Account
	ProviderAccountID string
	AccessTokenEnc    []byte
	RefreshTokenEnc   []byte
	// AppFolderID is the provider folder shards are written into. Empty
	// means one has not been established yet; it is never "root".
	AppFolderID string
}

// PendingOAuth is a state row awaiting its callback.
type PendingOAuth struct {
	UserID     uuid.UUID
	Kind       storage.Kind
	RedirectTo string
}

// Store is the persistence this package needs.
type Store interface {
	CreateAccount(ctx context.Context, n NewAccount) (StoredAccount, error)
	UpdateAccountTokens(ctx context.Context, u TokenUpdate) (StoredAccount, error)
	GetAccount(ctx context.Context, userID, id uuid.UUID) (StoredAccount, error)
	GetAccountByProviderID(ctx context.Context, userID uuid.UUID, kind storage.Kind, providerID string) (StoredAccount, error)
	ListAccounts(ctx context.Context, userID uuid.UUID) ([]StoredAccount, error)
	ListAccountsForSync(ctx context.Context) ([]StoredAccount, error)
	NextOrdinal(ctx context.Context, userID uuid.UUID) (int32, error)
	SetAccountStatus(ctx context.Context, id uuid.UUID, status, lastErr string) error

	// GetAppFolderID returns "" when no folder has been established.
	GetAppFolderID(ctx context.Context, id uuid.UUID) (string, error)
	// SetAppFolderID writes the folder id only if none is set yet, and
	// returns ErrNotFound when another writer got there first.
	SetAppFolderID(ctx context.Context, id uuid.UUID, folderID string) (string, error)
	DeleteAccount(ctx context.Context, userID, id uuid.UUID) (int64, error)

	UpsertCapacity(ctx context.Context, accountID uuid.UUID, total, used int64) error
	SetCapacityError(ctx context.Context, accountID uuid.UUID, msg string) error
	ListCapacity(ctx context.Context, userID uuid.UUID) ([]Capacity, error)

	CreateOAuthState(ctx context.Context, stateHash []byte, p PendingOAuth, expiresAt time.Time) error
	ConsumeOAuthState(ctx context.Context, stateHash []byte) (PendingOAuth, error)
	DeleteExpiredOAuthStates(ctx context.Context) (int64, error)
}
