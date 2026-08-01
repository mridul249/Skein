package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/db/gensqlite"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// sqliteTimeLayout matches internal/auth's: fixed-width UTC RFC 3339, so text
// comparison sorts in chronological order. oauth_states.expires_at is compared
// in SQL (ConsumeOAuthState), which is what makes that a correctness
// requirement rather than a formatting preference.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteStore is the SQLite implementation of Store, for the desktop build
// (Phase 7 Task 3). It satisfies the same interface as PGStore and is verified
// against the same test suite -- see storeconformance_test.go.
type SQLiteStore struct {
	q *gensqlite.Queries

	// now is the clock. The Postgres queries call now() server-side; the
	// SQLite dialect binds timestamps as parameters, so this store decides
	// what "now" is and a test can replace it.
	now func() time.Time
}

// NewSQLiteStore wraps an open SQLite database. The caller owns the *sql.DB
// and its pragmas; see auth.OpenSQLite, which sets the ones this expects.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{q: gensqlite.New(db), now: time.Now}
}

// CreateAccount links a provider account.
func (s *SQLiteStore) CreateAccount(ctx context.Context, n NewAccount) (StoredAccount, error) {
	now := s.fmt(s.now())
	row, err := s.q.CreateConnectedAccount(ctx, gensqlite.CreateConnectedAccountParams{
		ID:                n.ID.String(),
		UserID:            n.UserID.String(),
		Kind:              string(n.Kind),
		ProviderAccountID: n.ProviderAccountID,
		Email:             n.Email,
		DisplayName:       n.DisplayName,
		AccessTokenEnc:    n.AccessTokenEnc,
		RefreshTokenEnc:   n.RefreshTokenEnc,
		TokenExpiresAt:    s.fmtPtr(n.TokenExpiresAt),
		Ordinal:           int64(n.Ordinal),
		CreatedAt:         now,
		UpdatedAt:         now,
	})
	if err != nil {
		if isSQLiteUniqueViolation(err) {
			return StoredAccount{}, skerr.ErrConflict
		}
		return StoredAccount{}, fmt.Errorf("insert connected account: %w", err)
	}
	return toSQLiteStoredAccount(row)
}

// UpdateAccountTokens refreshes credentials on an already-linked account, and
// returns it to 'active' -- which is what makes reconnecting a disconnected
// drive restore access (issue #19).
func (s *SQLiteStore) UpdateAccountTokens(ctx context.Context, u TokenUpdate) (StoredAccount, error) {
	row, err := s.q.UpdateAccountTokens(ctx, gensqlite.UpdateAccountTokensParams{
		ID:              u.ID.String(),
		UserID:          u.UserID.String(),
		AccessTokenEnc:  u.AccessTokenEnc,
		RefreshTokenEnc: u.RefreshTokenEnc,
		TokenExpiresAt:  s.fmtPtr(u.TokenExpiresAt),
		Email:           u.Email,
		DisplayName:     u.DisplayName,
		UpdatedAt:       s.fmt(s.now()),
	})
	if err != nil {
		return StoredAccount{}, mapSQLiteNoRows(err, "update account tokens")
	}
	return toSQLiteStoredAccount(row)
}

// GetAccount loads one account the user owns.
func (s *SQLiteStore) GetAccount(ctx context.Context, userID, id uuid.UUID) (StoredAccount, error) {
	row, err := s.q.GetConnectedAccount(ctx, gensqlite.GetConnectedAccountParams{
		ID:     id.String(),
		UserID: userID.String(),
	})
	if err != nil {
		return StoredAccount{}, mapSQLiteNoRows(err, "select connected account")
	}
	return toSQLiteStoredAccount(row)
}

// GetAccountByProviderID looks up by the provider's own identifier.
func (s *SQLiteStore) GetAccountByProviderID(ctx context.Context, userID uuid.UUID, kind storage.Kind, providerID string) (StoredAccount, error) {
	row, err := s.q.GetConnectedAccountByProviderID(ctx, gensqlite.GetConnectedAccountByProviderIDParams{
		UserID:            userID.String(),
		Kind:              string(kind),
		ProviderAccountID: providerID,
	})
	if err != nil {
		return StoredAccount{}, mapSQLiteNoRows(err, "select account by provider id")
	}
	return toSQLiteStoredAccount(row)
}

// ListAccounts returns one user's accounts.
func (s *SQLiteStore) ListAccounts(ctx context.Context, userID uuid.UUID) ([]StoredAccount, error) {
	rows, err := s.q.ListConnectedAccounts(ctx, userID.String())
	if err != nil {
		return nil, fmt.Errorf("list connected accounts: %w", err)
	}
	return toSQLiteStoredAccounts(rows)
}

// ListAccountsForSync returns every active account.
func (s *SQLiteStore) ListAccountsForSync(ctx context.Context) ([]StoredAccount, error) {
	rows, err := s.q.ListActiveAccountsForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts for sync: %w", err)
	}
	return toSQLiteStoredAccounts(rows)
}

// NextOrdinal returns the next colour-ramp position.
func (s *SQLiteStore) NextOrdinal(ctx context.Context, userID uuid.UUID) (int32, error) {
	n, err := s.q.NextAccountOrdinal(ctx, userID.String())
	if err != nil {
		return 0, fmt.Errorf("next ordinal: %w", err)
	}
	return int32(n), nil
}

// SetAccountStatus records a status change.
//
// The status column has a CHECK constraint listing the allowed values, and it
// is enforced under SQLite exactly as under Postgres -- verified 2026-08-01,
// on UPDATE as well as INSERT. Issue #19's fix depends on that: Disconnect
// soft deletes by writing 'disabled' here, so a silently-accepted bad value
// would mean a drive that reports disconnected and keeps serving.
func (s *SQLiteStore) SetAccountStatus(ctx context.Context, id uuid.UUID, status, lastErr string) error {
	if err := s.q.SetAccountStatus(ctx, gensqlite.SetAccountStatusParams{
		ID:        id.String(),
		Status:    status,
		LastError: lastErr,
		UpdatedAt: s.fmt(s.now()),
	}); err != nil {
		return fmt.Errorf("set account status: %w", err)
	}
	return nil
}

// ClearAccountTokens wipes stored credentials, leaving the row and its id in
// place. See the Store interface, and Service.Disconnect for why the row has
// to survive.
func (s *SQLiteStore) ClearAccountTokens(ctx context.Context, id uuid.UUID) error {
	if err := s.q.ClearAccountTokens(ctx, gensqlite.ClearAccountTokensParams{
		ID: id.String(),
		// access_token_enc is NOT NULL, so it is emptied rather than nulled.
		AccessTokenEnc: []byte{},
		UpdatedAt:      s.fmt(s.now()),
	}); err != nil {
		return fmt.Errorf("clear account tokens: %w", err)
	}
	return nil
}

// GetAppFolderID returns the stored app folder id, or "" if none.
func (s *SQLiteStore) GetAppFolderID(ctx context.Context, id uuid.UUID) (string, error) {
	got, err := s.q.GetAppFolderID(ctx, id.String())
	if err != nil {
		return "", mapSQLiteNoRows(err, "select app folder id")
	}
	return derefString(got), nil
}

// SetAppFolderID writes the folder id only when the column is still NULL.
// Zero rows means another writer won, which the caller resolves by re-reading.
func (s *SQLiteStore) SetAppFolderID(ctx context.Context, id uuid.UUID, folderID string) (string, error) {
	got, err := s.q.SetAppFolderID(ctx, gensqlite.SetAppFolderIDParams{
		ID:          id.String(),
		AppFolderID: &folderID,
		UpdatedAt:   s.fmt(s.now()),
	})
	if err != nil {
		return "", mapSQLiteNoRows(err, "set app folder id")
	}
	return derefString(got), nil
}

// DeleteAccount unlinks an account the user owns. Not the disconnect path --
// see the note on DeleteConnectedAccount in queries/sqlite/accounts.sql.
func (s *SQLiteStore) DeleteAccount(ctx context.Context, userID, id uuid.UUID) (int64, error) {
	n, err := s.q.DeleteConnectedAccount(ctx, gensqlite.DeleteConnectedAccountParams{
		ID:     id.String(),
		UserID: userID.String(),
	})
	if err != nil {
		return 0, fmt.Errorf("delete connected account: %w", err)
	}
	return n, nil
}

// UpsertCapacity records a fresh quota reading.
func (s *SQLiteStore) UpsertCapacity(ctx context.Context, accountID uuid.UUID, total, used int64) error {
	syncedAt := s.fmt(s.now())
	if err := s.q.UpsertStorageAccount(ctx, gensqlite.UpsertStorageAccountParams{
		ConnectedAccountID: accountID.String(),
		TotalBytes:         total,
		UsedBytes:          used,
		LastSyncedAt:       &syncedAt,
	}); err != nil {
		return fmt.Errorf("upsert storage account: %w", err)
	}
	return nil
}

// SetCapacityError records why a quota refresh failed.
func (s *SQLiteStore) SetCapacityError(ctx context.Context, accountID uuid.UUID, msg string) error {
	if err := s.q.SetStorageAccountError(ctx, gensqlite.SetStorageAccountErrorParams{
		ConnectedAccountID: accountID.String(),
		LastError:          msg,
	}); err != nil {
		return fmt.Errorf("set storage account error: %w", err)
	}
	return nil
}

// ListCapacity returns per-account capacity joined to the account, in one
// query rather than a lookup per row.
func (s *SQLiteStore) ListCapacity(ctx context.Context, userID uuid.UUID) ([]Capacity, error) {
	rows, err := s.q.ListStorageAccounts(ctx, userID.String())
	if err != nil {
		return nil, fmt.Errorf("list storage accounts: %w", err)
	}
	out := make([]Capacity, 0, len(rows))
	for _, r := range rows {
		id, perr := uuid.Parse(r.ID)
		if perr != nil {
			return nil, fmt.Errorf("parse account id %q: %w", r.ID, perr)
		}
		syncedAt, terr := parseSQLiteTimePtr(r.LastSyncedAt)
		if terr != nil {
			return nil, fmt.Errorf("parse last_synced_at: %w", terr)
		}
		out = append(out, Capacity{
			Account: Account{
				ID:          id,
				UserID:      userID,
				Kind:        storage.Kind(r.Kind),
				Email:       r.Email,
				DisplayName: r.DisplayName,
				Status:      r.Status,
				LastError:   r.LastError,
				Ordinal:     int32(r.Ordinal),
			},
			TotalBytes:    toInt64(r.TotalBytes),
			UsedBytes:     toInt64(r.UsedBytes),
			ReservedBytes: toInt64(r.ReservedBytes),
			LastSyncedAt:  syncedAt,
		})
	}
	return out, nil
}

// CreateOAuthState stores the hash of a pending authorisation.
func (s *SQLiteStore) CreateOAuthState(ctx context.Context, stateHash []byte, p PendingOAuth, expiresAt time.Time) error {
	var verifier *string
	if p.PKCEVerifier != "" {
		verifier = &p.PKCEVerifier
	}
	if err := s.q.CreateOAuthState(ctx, gensqlite.CreateOAuthStateParams{
		StateHash:    stateHash,
		UserID:       p.UserID.String(),
		Kind:         string(p.Kind),
		RedirectTo:   p.RedirectTo,
		PkceVerifier: verifier,
		CreatedAt:    s.fmt(s.now()),
		ExpiresAt:    s.fmt(expiresAt),
	}); err != nil {
		return fmt.Errorf("insert oauth state: %w", err)
	}
	return nil
}

// ConsumeOAuthState reads and deletes a pending authorisation in one
// statement, which is what makes it single use. The expiry predicate stays in
// SQL so a clock-skewed application check cannot accept a stale state.
func (s *SQLiteStore) ConsumeOAuthState(ctx context.Context, stateHash []byte) (PendingOAuth, error) {
	row, err := s.q.ConsumeOAuthState(ctx, gensqlite.ConsumeOAuthStateParams{
		StateHash: stateHash,
		ExpiresAt: s.fmt(s.now()),
	})
	if err != nil {
		return PendingOAuth{}, mapSQLiteNoRows(err, "consume oauth state")
	}
	userID, perr := uuid.Parse(row.UserID)
	if perr != nil {
		return PendingOAuth{}, fmt.Errorf("parse oauth state user id: %w", perr)
	}
	return PendingOAuth{
		UserID:       userID,
		Kind:         storage.Kind(row.Kind),
		RedirectTo:   row.RedirectTo,
		PKCEVerifier: derefString(row.PkceVerifier),
	}, nil
}

// DeleteExpiredOAuthStates clears abandoned authorisations.
func (s *SQLiteStore) DeleteExpiredOAuthStates(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredOAuthStates(ctx, s.fmt(s.now()))
	if err != nil {
		return 0, fmt.Errorf("delete expired oauth states: %w", err)
	}
	return n, nil
}

// --- conversions -----------------------------------------------------------

func (s *SQLiteStore) fmt(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

func (s *SQLiteStore) fmtPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := s.fmt(*t)
	return &v
}

func toSQLiteStoredAccounts(rows []gensqlite.ConnectedAccount) ([]StoredAccount, error) {
	out := make([]StoredAccount, 0, len(rows))
	for _, r := range rows {
		a, err := toSQLiteStoredAccount(r)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func toSQLiteStoredAccount(r gensqlite.ConnectedAccount) (StoredAccount, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return StoredAccount{}, fmt.Errorf("parse account id %q: %w", r.ID, err)
	}
	userID, err := uuid.Parse(r.UserID)
	if err != nil {
		return StoredAccount{}, fmt.Errorf("parse account user_id: %w", err)
	}
	created, err := parseSQLiteTime(r.CreatedAt)
	if err != nil {
		return StoredAccount{}, fmt.Errorf("parse account created_at: %w", err)
	}
	expires, err := parseSQLiteTimePtr(r.TokenExpiresAt)
	if err != nil {
		return StoredAccount{}, fmt.Errorf("parse account token_expires_at: %w", err)
	}
	return StoredAccount{
		Account: Account{
			ID:           id,
			UserID:       userID,
			Kind:         storage.Kind(r.Kind),
			Email:        r.Email,
			DisplayName:  r.DisplayName,
			Status:       r.Status,
			LastError:    r.LastError,
			Ordinal:      int32(r.Ordinal),
			CreatedAt:    created,
			TokenExpires: expires,
		},
		ProviderAccountID: r.ProviderAccountID,
		AccessTokenEnc:    r.AccessTokenEnc,
		RefreshTokenEnc:   r.RefreshTokenEnc,
		AppFolderID:       derefString(r.AppFolderID),
	}, nil
}

func parseSQLiteTime(s string) (time.Time, error) {
	return time.Parse(sqliteTimeLayout, s)
}

func parseSQLiteTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := parseSQLiteTime(*s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// toInt64 narrows the interface{} sqlc emits for COALESCE over a nullable
// column in a LEFT JOIN. SQLite has no static type for that expression, so the
// driver hands back whatever it stored.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func mapSQLiteNoRows(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return skerr.ErrNotFound
	}
	return fmt.Errorf("%s: %w", what, err)
}

// isSQLiteUniqueViolation reports a UNIQUE constraint failure. modernc.org's
// driver does not export a usable code, and SQLite's wording here has been
// stable for years and is not localised.
func isSQLiteUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Compile-time proof that this store satisfies the interface it implements.
var _ Store = (*SQLiteStore)(nil)

// --- test support ----------------------------------------------------------
//
// Mirrors of the inspection helpers on MemoryStore, so the same test suite can
// drive either store (see storeconformance_test.go). No production caller.

// PendingStateCount returns how many OAuth states are outstanding.
func (s *SQLiteStore) PendingStateCount() int {
	n, err := s.q.PendingStateCount(context.Background())
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: count pending states: %v", err))
	}
	return int(n)
}

// HasStateHash reports whether a state is stored under exactly these bytes.
// The tests use it to prove a state is stored hashed and never raw.
func (s *SQLiteStore) HasStateHash(hash []byte) bool {
	n, err := s.q.StateHashExists(context.Background(), hash)
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: state hash lookup: %v", err))
	}
	return n > 0
}

// PendingVerifiers returns the PKCE verifier of every outstanding state.
func (s *SQLiteStore) PendingVerifiers() []string {
	rows, err := s.q.PendingVerifiers(context.Background())
	if err != nil {
		panic(fmt.Sprintf("sqlitestore: list pending verifiers: %v", err))
	}
	return rows
}

// SetReserved forces reserved_bytes, so quota maths can be exercised without
// running a real reservation.
func (s *SQLiteStore) SetReserved(accountID uuid.UUID, reserved int64) {
	if err := s.q.SetReservedBytes(context.Background(), gensqlite.SetReservedBytesParams{
		ConnectedAccountID: accountID.String(),
		ReservedBytes:      reserved,
	}); err != nil {
		panic(fmt.Sprintf("sqlitestore: set reserved bytes: %v", err))
	}
}
