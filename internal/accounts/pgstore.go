package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mridul249/Skein/internal/db/gen"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

const pgUniqueViolation = "23505"

// PGStore is the Postgres implementation of Store.
type PGStore struct{ q *gen.Queries }

// NewPGStore wraps a pgx connection pool.
func NewPGStore(db gen.DBTX) *PGStore { return &PGStore{q: gen.New(db)} }

// CreateAccount links a new provider account.
func (s *PGStore) CreateAccount(ctx context.Context, n NewAccount) (StoredAccount, error) {
	row, err := s.q.CreateConnectedAccount(ctx, gen.CreateConnectedAccountParams{
		ID:                n.ID,
		UserID:            n.UserID,
		Kind:              string(n.Kind),
		ProviderAccountID: n.ProviderAccountID,
		Email:             n.Email,
		DisplayName:       n.DisplayName,
		AccessTokenEnc:    n.AccessTokenEnc,
		RefreshTokenEnc:   n.RefreshTokenEnc,
		TokenExpiresAt:    nullTS(n.TokenExpiresAt),
		Ordinal:           n.Ordinal,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return StoredAccount{}, skerr.ErrConflict
		}
		return StoredAccount{}, fmt.Errorf("insert connected account: %w", err)
	}
	return toStoredAccount(row), nil
}

// UpdateAccountTokens refreshes stored credentials, scoped to the owner.
func (s *PGStore) UpdateAccountTokens(ctx context.Context, u TokenUpdate) (StoredAccount, error) {
	row, err := s.q.UpdateAccountTokens(ctx, gen.UpdateAccountTokensParams{
		ID:              u.ID,
		UserID:          u.UserID,
		AccessTokenEnc:  u.AccessTokenEnc,
		RefreshTokenEnc: u.RefreshTokenEnc,
		TokenExpiresAt:  nullTS(u.TokenExpiresAt),
		Email:           u.Email,
		DisplayName:     u.DisplayName,
	})
	if err != nil {
		return StoredAccount{}, mapNoRows(err, "update account tokens")
	}
	return toStoredAccount(row), nil
}

// GetAccount loads one account the user owns. Rules.md §2.7: ownership is a
// predicate in the query, never a comparison after the fact.
func (s *PGStore) GetAccount(ctx context.Context, userID, id uuid.UUID) (StoredAccount, error) {
	row, err := s.q.GetConnectedAccount(ctx, gen.GetConnectedAccountParams{ID: id, UserID: userID})
	if err != nil {
		return StoredAccount{}, mapNoRows(err, "select connected account")
	}
	return toStoredAccount(row), nil
}

// GetAccountByProviderID looks an account up by the provider's own identifier.
func (s *PGStore) GetAccountByProviderID(ctx context.Context, userID uuid.UUID, kind storage.Kind, providerID string) (StoredAccount, error) {
	row, err := s.q.GetConnectedAccountByProviderID(ctx, gen.GetConnectedAccountByProviderIDParams{
		UserID:            userID,
		Kind:              string(kind),
		ProviderAccountID: providerID,
	})
	if err != nil {
		return StoredAccount{}, mapNoRows(err, "select account by provider id")
	}
	return toStoredAccount(row), nil
}

// ListAccounts returns the user's accounts in ramp order.
func (s *PGStore) ListAccounts(ctx context.Context, userID uuid.UUID) ([]StoredAccount, error) {
	rows, err := s.q.ListConnectedAccounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list connected accounts: %w", err)
	}
	return mapStoredAccounts(rows), nil
}

// ListAccountsForSync returns every active account across all users. It backs
// the background ticker, which acts for no particular user.
func (s *PGStore) ListAccountsForSync(ctx context.Context) ([]StoredAccount, error) {
	rows, err := s.q.ListActiveAccountsForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts for sync: %w", err)
	}
	return mapStoredAccounts(rows), nil
}

// NextOrdinal returns the next position in the account colour ramp.
func (s *PGStore) NextOrdinal(ctx context.Context, userID uuid.UUID) (int32, error) {
	n, err := s.q.NextAccountOrdinal(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("next ordinal: %w", err)
	}
	return int32(n), nil
}

// SetAccountStatus records a status change and its reason.
func (s *PGStore) SetAccountStatus(ctx context.Context, id uuid.UUID, status, lastErr string) error {
	if err := s.q.SetAccountStatus(ctx, gen.SetAccountStatusParams{
		ID: id, Status: status, LastError: lastErr,
	}); err != nil {
		return fmt.Errorf("set account status: %w", err)
	}
	return nil
}

// GetAppFolderID returns the stored app folder id, or "" if none.
func (s *PGStore) GetAppFolderID(ctx context.Context, id uuid.UUID) (string, error) {
	got, err := s.q.GetAppFolderID(ctx, id)
	if err != nil {
		return "", mapNoRows(err, "select app folder id")
	}
	if got == nil {
		return "", nil
	}
	return *got, nil
}

// SetAppFolderID writes the folder id only when the column is still NULL.
// Zero rows means another writer won, which the caller resolves by re-reading.
func (s *PGStore) SetAppFolderID(ctx context.Context, id uuid.UUID, folderID string) (string, error) {
	got, err := s.q.SetAppFolderID(ctx, gen.SetAppFolderIDParams{
		ID:          id,
		AppFolderID: &folderID,
	})
	if err != nil {
		return "", mapNoRows(err, "set app folder id")
	}
	if got == nil {
		return "", nil
	}
	return *got, nil
}

// RebindAppFolderID overwrites the folder id. See the query comment for why
// recovery needs a write SetAppFolderID above deliberately refuses.
func (s *PGStore) RebindAppFolderID(ctx context.Context, id uuid.UUID, folderID string) error {
	if err := s.q.RebindAppFolderID(ctx, gen.RebindAppFolderIDParams{
		ID:          id,
		AppFolderID: &folderID,
	}); err != nil {
		return fmt.Errorf("rebind app folder id: %w", err)
	}
	return nil
}

// ClearAccountTokens wipes stored credentials, leaving the row and its id in
// place. See the Store interface, and Service.Disconnect for why the row has
// to survive.
func (s *PGStore) ClearAccountTokens(ctx context.Context, id uuid.UUID) error {
	if err := s.q.ClearAccountTokens(ctx, id); err != nil {
		return fmt.Errorf("clear account tokens: %w", err)
	}
	return nil
}

// DeleteAccount unlinks an account the user owns. Not the disconnect path —
// see the note on DeleteConnectedAccount in queries/accounts.sql.
func (s *PGStore) DeleteAccount(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	n, err := s.q.DeleteConnectedAccount(ctx, gen.DeleteConnectedAccountParams{ID: id, UserID: userID})
	if err != nil {
		return 0, fmt.Errorf("delete connected account: %w", err)
	}
	return n, nil
}

// UpsertCapacity records a fresh quota reading.
func (s *PGStore) UpsertCapacity(ctx context.Context, accountID uuid.UUID, total, used int64) error {
	if err := s.q.UpsertStorageAccount(ctx, gen.UpsertStorageAccountParams{
		ConnectedAccountID: accountID,
		TotalBytes:         total,
		UsedBytes:          used,
	}); err != nil {
		return fmt.Errorf("upsert storage account: %w", err)
	}
	return nil
}

// SetCapacityError records why a quota read failed.
func (s *PGStore) SetCapacityError(ctx context.Context, accountID uuid.UUID, msg string) error {
	if err := s.q.SetStorageAccountError(ctx, gen.SetStorageAccountErrorParams{
		ConnectedAccountID: accountID, LastError: msg,
	}); err != nil {
		return fmt.Errorf("set storage account error: %w", err)
	}
	return nil
}

// ListCapacity returns per-account capacity joined to the account, in one
// query rather than a lookup per row.
func (s *PGStore) ListCapacity(ctx context.Context, userID uuid.UUID) ([]Capacity, error) {
	rows, err := s.q.ListStorageAccounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list storage accounts: %w", err)
	}
	out := make([]Capacity, 0, len(rows))
	for _, r := range rows {
		out = append(out, Capacity{
			Account: Account{
				ID:          r.ID,
				UserID:      userID,
				Kind:        storage.Kind(r.Kind),
				Email:       r.Email,
				DisplayName: r.DisplayName,
				Status:      r.Status,
				LastError:   r.LastError,
				Ordinal:     r.Ordinal,
			},
			TotalBytes:    r.TotalBytes,
			UsedBytes:     r.UsedBytes,
			ReservedBytes: r.ReservedBytes,
			LastSyncedAt:  nullableTime(r.LastSyncedAt),
		})
	}
	return out, nil
}

// CreateOAuthState stores the hash of a pending authorisation.
func (s *PGStore) CreateOAuthState(ctx context.Context, stateHash []byte, p PendingOAuth, expiresAt time.Time) error {
	var verifier *string
	if p.PKCEVerifier != "" {
		verifier = &p.PKCEVerifier
	}
	if err := s.q.CreateOAuthState(ctx, gen.CreateOAuthStateParams{
		StateHash:    stateHash,
		UserID:       p.UserID,
		Kind:         string(p.Kind),
		RedirectTo:   p.RedirectTo,
		PkceVerifier: verifier,
		ExpiresAt:    pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return fmt.Errorf("insert oauth state: %w", err)
	}
	return nil
}

// ConsumeOAuthState reads and deletes a pending authorisation in one
// statement, which is what makes it single use.
func (s *PGStore) ConsumeOAuthState(ctx context.Context, stateHash []byte) (PendingOAuth, error) {
	row, err := s.q.ConsumeOAuthState(ctx, stateHash)
	if err != nil {
		return PendingOAuth{}, mapNoRows(err, "consume oauth state")
	}
	var verifier string
	if row.PkceVerifier != nil {
		verifier = *row.PkceVerifier
	}
	return PendingOAuth{
		UserID:       row.UserID,
		Kind:         storage.Kind(row.Kind),
		RedirectTo:   row.RedirectTo,
		PKCEVerifier: verifier,
	}, nil
}

// DeleteExpiredOAuthStates clears abandoned authorisations.
func (s *PGStore) DeleteExpiredOAuthStates(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredOAuthStates(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete expired oauth states: %w", err)
	}
	return n, nil
}

func mapStoredAccounts(rows []gen.ConnectedAccount) []StoredAccount {
	out := make([]StoredAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStoredAccount(r))
	}
	return out
}

func toStoredAccount(r gen.ConnectedAccount) StoredAccount {
	return StoredAccount{
		Account: Account{
			ID:           r.ID,
			UserID:       r.UserID,
			Kind:         storage.Kind(r.Kind),
			Email:        r.Email,
			DisplayName:  r.DisplayName,
			Status:       r.Status,
			LastError:    r.LastError,
			Ordinal:      r.Ordinal,
			CreatedAt:    r.CreatedAt.Time,
			TokenExpires: nullableTime(r.TokenExpiresAt),
		},
		ProviderAccountID: r.ProviderAccountID,
		AccessTokenEnc:    r.AccessTokenEnc,
		RefreshTokenEnc:   r.RefreshTokenEnc,
		AppFolderID:       deref(r.AppFolderID),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func mapNoRows(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return skerr.ErrNotFound
	}
	return fmt.Errorf("%s: %w", what, err)
}

func nullableTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func nullTS(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// Compile-time check.
var _ Store = (*PGStore)(nil)
