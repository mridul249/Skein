// Package auth owns registration, login, and the refresh-token lifecycle.
//
// The rotation scheme in Refresh is the reason this package exists as more
// than a thin CRUD layer: a refresh token is single use, every use issues a
// successor, and presenting a token that has already been used revokes every
// token descended from the same login.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/httpapi/middleware"
	"github.com/mridul60214/skein/internal/skerr"
)

const (
	minPasswordLen = 12
	maxPasswordLen = 256 // argon2 cost is linear in input length past this
	maxEmailLen    = 254 // RFC 5321
)

// RequestMeta is the non-secret request context attached to sessions and audit
// records. The IP comes from the RealIP middleware, never from a raw header.
type RequestMeta struct {
	IP        *netip.Addr
	UserAgent string
}

// Service implements the auth use cases.
type Service struct {
	store      Store
	tokens     *TokenIssuer
	log        *slog.Logger
	refreshTTL time.Duration
	now        func() time.Time
}

// NewService wires the auth service.
func NewService(store Store, tokens *TokenIssuer, refreshTTL time.Duration, log *slog.Logger) *Service {
	return &Service{
		store:      store,
		tokens:     tokens,
		log:        log,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}
}

// TokenPair is what a successful login or refresh produces. RefreshToken is
// the only place the plaintext exists after minting; the caller writes it
// straight to an httpOnly cookie and drops it.
type TokenPair struct {
	AccessToken  string
	AccessExpiry time.Time
	RefreshToken string
	RefreshTTL   time.Duration
	User         User
	SessionID    uuid.UUID
}

// Register creates a user and signs them in.
func (s *Service) Register(ctx context.Context, email, password string, meta RequestMeta) (TokenPair, error) {
	normalised, err := normaliseEmail(email)
	if err != nil {
		return TokenPair{}, err
	}
	if verr := validatePassword(password); verr != nil {
		return TokenPair{}, verr
	}

	hash, err := HashPassword(password)
	if err != nil {
		return TokenPair{}, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.store.CreateUser(ctx, uuid.New(), normalised, hash)
	if err != nil {
		if errors.Is(err, skerr.ErrConflict) {
			// Deliberately the same shape as a successful registration
			// would not be — but note this does confirm the address is
			// taken. Single-tenant self-hosted: the alternative
			// (silently pretending to succeed) makes the sign-up form
			// unusable, and there is no directory to enumerate.
			return TokenPair{}, skerr.Public(skerr.ErrConflict,
				"An account with that email already exists.")
		}
		return TokenPair{}, fmt.Errorf("create user: %w", err)
	}

	s.record(ctx, &user.ID, EventRegistered, nil, meta)
	return s.startSession(ctx, user, meta)
}

// Login verifies credentials and starts a new session family.
func (s *Service) Login(ctx context.Context, email, password string, meta RequestMeta) (TokenPair, error) {
	normalised, err := normaliseEmail(email)
	if err != nil {
		// Do not tell the caller their email was malformed versus
		// unknown; both are the same failure from outside.
		return TokenPair{}, errInvalidCredentials()
	}
	if utf8.RuneCountInString(password) > maxPasswordLen {
		return TokenPair{}, errInvalidCredentials()
	}

	user, err := s.store.GetUserByEmail(ctx, normalised)
	if err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			// Spend comparable work on an unknown address so response
			// time does not distinguish "no such user" from "wrong
			// password".
			SpendPasswordWork(password)
			s.record(ctx, nil, EventLoginFailed, map[string]any{"reason": "unknown_user"}, meta)
			return TokenPair{}, errInvalidCredentials()
		}
		return TokenPair{}, fmt.Errorf("look up user: %w", err)
	}

	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		s.log.ErrorContext(ctx, "stored password hash is malformed",
			slog.String("user_id", user.ID.String()))
		return TokenPair{}, errInvalidCredentials()
	}
	if !ok {
		s.record(ctx, &user.ID, EventLoginFailed, map[string]any{"reason": "bad_password"}, meta)
		return TokenPair{}, errInvalidCredentials()
	}

	// A successful login is the only moment the plaintext password is
	// available, so it is the only moment an outdated hash can be upgraded.
	if NeedsRehash(user.PasswordHash) {
		if newHash, herr := HashPassword(password); herr == nil {
			if uerr := s.store.UpdateUserPassword(ctx, user.ID, newHash); uerr != nil {
				s.log.WarnContext(ctx, "could not upgrade password hash",
					slog.String("user_id", user.ID.String()),
					slog.String("error", uerr.Error()))
			}
		}
	}

	s.record(ctx, &user.ID, EventLoginSucceeded, nil, meta)
	return s.startSession(ctx, user, meta)
}

// Refresh rotates a refresh token.
//
// This is the whole point of the package. Rules.md §2.8:
//
//   - A refresh token is single use. Using it mints a successor and marks the
//     presented one used, in one conditional UPDATE so two concurrent uses
//     cannot both win.
//   - Every successor keeps the family_id of the login it descends from and
//     records prev_id, so the chain is reconstructable during review.
//   - Presenting a token that is already used, already revoked, or that loses
//     the claim race revokes the entire family. Either it was stolen or the
//     client replayed one, and the server cannot tell those apart — so the
//     safe reading is the hostile one.
//   - The family deadline does not slide. A successor inherits the expiry of
//     the token it replaces, so a stolen-and-refreshed chain still dies on
//     schedule instead of living forever.
func (s *Service) Refresh(ctx context.Context, presented string, meta RequestMeta) (TokenPair, error) {
	if presented == "" {
		return TokenPair{}, errInvalidRefresh()
	}

	hash := HashRefreshToken(presented)
	sess, err := s.store.GetSessionByRefreshHash(ctx, hash)
	if err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			// Unknown token. There is no family to revoke, so this is
			// recorded and rejected without further action.
			s.record(ctx, nil, EventRefreshRejected, map[string]any{"reason": "unknown"}, meta)
			return TokenPair{}, errInvalidRefresh()
		}
		return TokenPair{}, fmt.Errorf("look up session: %w", err)
	}

	now := s.now()

	// Advisory only, and deliberately not exhaustive. These read the snapshot
	// taken above, and they cannot see a revoked FAMILY: a successor inserted
	// after its family was revoked has RevokedAt nil, so the second case below
	// does not fire for it. The claim gate is what rejects that row, and
	// revokeFamily's audit detail distinguishes it. Do not add a family read
	// here — it would put a round trip on every refresh to improve an error
	// string.
	switch {
	case sess.UsedAt != nil:
		// Reuse. The successor of this token is already out there, so
		// somebody is holding a copy they should not have.
		s.revokeFamily(ctx, sess, "token_reused", meta)
		return TokenPair{}, errInvalidRefresh()

	case sess.RevokedAt != nil:
		// The family was already torn down, most likely by an earlier
		// reuse. Record it and stop; revoking again is a no-op.
		s.record(ctx, &sess.UserID, EventRefreshRejected,
			map[string]any{"reason": "revoked", "family_id": sess.FamilyID.String()}, meta)
		return TokenPair{}, errInvalidRefresh()

	case !sess.ExpiresAt.After(now):
		s.record(ctx, &sess.UserID, EventRefreshRejected,
			map[string]any{"reason": "expired", "family_id": sess.FamilyID.String()}, meta)
		return TokenPair{}, errInvalidRefresh()
	}

	// Claim the token. The predicate lives in SQL, so two requests arriving
	// with the same token produce exactly one winner regardless of how the
	// checks above interleaved.
	claimed, err := s.store.ClaimSession(ctx, sess.ID)
	if err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			// Lost the race: another request used this same token
			// between the read above and here. Same conclusion as an
			// outright reuse.
			s.revokeFamily(ctx, sess, "concurrent_use", meta)
			return TokenPair{}, errInvalidRefresh()
		}
		return TokenPair{}, fmt.Errorf("claim session: %w", err)
	}

	user, err := s.store.GetUserByID(ctx, claimed.UserID)
	if err != nil {
		return TokenPair{}, fmt.Errorf("load user for refresh: %w", err)
	}

	token, tokenHash, err := NewRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	prev := claimed.ID
	next, err := s.store.CreateSession(ctx, NewSession{
		ID:          uuid.New(),
		UserID:      claimed.UserID,
		FamilyID:    claimed.FamilyID,
		PrevID:      &prev,
		RefreshHash: tokenHash,
		UserAgent:   meta.UserAgent,
		IP:          meta.IP,
		// Inherited, not extended: the family dies when the original
		// login would have.
		ExpiresAt: claimed.ExpiresAt,
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("create rotated session: %w", err)
	}

	access, accessExp, err := s.tokens.IssueAccessToken(user.ID, next.ID)
	if err != nil {
		return TokenPair{}, err
	}

	s.record(ctx, &user.ID, EventRefreshRotated,
		map[string]any{"family_id": next.FamilyID.String()}, meta)

	return TokenPair{
		AccessToken:  access,
		AccessExpiry: accessExp,
		RefreshToken: token,
		RefreshTTL:   time.Until(next.ExpiresAt),
		User:         user,
		SessionID:    next.ID,
	}, nil
}

// Logout revokes the family the presented refresh token belongs to, so signing
// out on one device does not leave a rotated successor alive.
func (s *Service) Logout(ctx context.Context, presented string, meta RequestMeta) error {
	if presented == "" {
		return nil // nothing to revoke; logging out twice is not an error
	}
	sess, err := s.store.GetSessionByRefreshHash(ctx, HashRefreshToken(presented))
	if err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("look up session: %w", err)
	}
	// Same ordering and the same reasoning as revokeFamily: the family marker
	// enforces, the session sweep records. Known issue #17 — Logout raced a
	// concurrent refresh in exactly the way #11 did, and the family marker
	// closes both with the same code.
	if _, err := s.store.RevokeTokenFamily(ctx, sess.FamilyID); err != nil {
		return fmt.Errorf("revoke token family: %w", err)
	}
	if _, err := s.store.RevokeSessionFamily(ctx, sess.FamilyID); err != nil {
		return fmt.Errorf("revoke family: %w", err)
	}
	s.record(ctx, &sess.UserID, EventLogout,
		map[string]any{"family_id": sess.FamilyID.String()}, meta)
	return nil
}

// Me returns the authenticated user.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (User, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			return User{}, skerr.ErrUnauthorized
		}
		return User{}, fmt.Errorf("load user: %w", err)
	}
	return u, nil
}

// VerifyAccessToken satisfies middleware.TokenVerifier.
func (s *Service) VerifyAccessToken(token string) (middleware.Principal, error) {
	return s.tokens.VerifyAccessToken(token)
}

// PurgeExpiredSessions deletes sessions long past expiry. Rows are kept for a
// grace period after expiry so a reuse attempt with a recently expired token
// is still recognisable rather than looking like an unknown token.
func (s *Service) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	n, err := s.store.DeleteExpiredSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	return n, nil
}

func (s *Service) startSession(ctx context.Context, user User, meta RequestMeta) (TokenPair, error) {
	token, hash, err := NewRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}
	familyID := uuid.New()

	// Family first, then the session: sessions.family_id references
	// token_families, so the reverse order is an FK violation. This order
	// risks at worst an orphan family row, which is harmless.
	if err := s.store.CreateTokenFamily(ctx, familyID, user.ID); err != nil {
		return TokenPair{}, fmt.Errorf("create token family: %w", err)
	}

	sess, err := s.store.CreateSession(ctx, NewSession{
		ID:          uuid.New(),
		UserID:      user.ID,
		FamilyID:    familyID,
		PrevID:      nil,
		RefreshHash: hash,
		UserAgent:   meta.UserAgent,
		IP:          meta.IP,
		ExpiresAt:   s.now().Add(s.refreshTTL),
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("create session: %w", err)
	}

	access, accessExp, err := s.tokens.IssueAccessToken(user.ID, sess.ID)
	if err != nil {
		return TokenPair{}, err
	}

	return TokenPair{
		AccessToken:  access,
		AccessExpiry: accessExp,
		RefreshToken: token,
		RefreshTTL:   time.Until(sess.ExpiresAt),
		User:         user,
		SessionID:    sess.ID,
	}, nil
}

// revokeFamily tears down every token descended from one login and records why.
//
// The order of the two writes is load-bearing and they look interchangeable.
// PGStore has no transactions — each call is its own implicit transaction — so a
// partial failure is possible and ordering is the only control available.
//
//	RevokeTokenFamily is the write that ENFORCES. It is what makes a
//	successor inserted after this point unclaimable, and it must not be
//	skipped or reordered after the sweep.
//
//	RevokeSessionFamily is the AUDIT RECORD. It keeps revoked_at meaningful
//	for inspection and covers the sessions that already exist, but it is no
//	longer the mechanism.
//
// Sweeping first and then failing to mark the family silently reproduces known
// issue #11 exactly: sessions revoked, family live, successor usable.
func (s *Service) revokeFamily(ctx context.Context, sess Session, reason string, meta RequestMeta) {
	// RevokeTokenFamily is idempotent and reports zero rows when the family
	// was already dead. That rowcount is free and distinguishes a fresh
	// detection from a replay against an already-revoked chain, which is the
	// case the advisory switch above structurally cannot report.
	families, err := s.store.RevokeTokenFamily(ctx, sess.FamilyID)
	if err != nil {
		// Loud, because this is the write that actually stops the attacker.
		s.log.ErrorContext(ctx, "could not revoke token family; the chain is still live",
			slog.String("family_id", sess.FamilyID.String()),
			slog.String("error", err.Error()))
	}
	firstDetection := families > 0

	n, serr := s.store.RevokeSessionFamily(ctx, sess.FamilyID)
	if serr != nil {
		s.log.ErrorContext(ctx, "could not revoke session family",
			slog.String("family_id", sess.FamilyID.String()),
			slog.String("error", serr.Error()))
	}
	// Logged at Warn, not Info: this is the signal an operator is meant to
	// notice. The token itself is never included.
	s.log.WarnContext(ctx, "refresh token reuse detected; session family revoked",
		slog.String("user_id", sess.UserID.String()),
		slog.String("family_id", sess.FamilyID.String()),
		slog.String("reason", reason),
		slog.Int64("sessions_revoked", n),
		slog.Bool("first_detection", firstDetection))

	s.record(ctx, &sess.UserID, EventRefreshReuse, map[string]any{
		"reason":           reason,
		"family_id":        sess.FamilyID.String(),
		"sessions_revoked": n,
		// False means the family was already revoked, so this is a replay
		// against a dead chain rather than a newly detected theft.
		"first_detection": firstDetection,
	}, meta)
}

func (s *Service) record(ctx context.Context, userID *uuid.UUID, kind string, detail map[string]any, meta RequestMeta) {
	if detail == nil {
		detail = map[string]any{}
	}
	err := s.store.RecordSecurityEvent(ctx, SecurityEvent{
		UserID:    userID,
		Kind:      kind,
		Detail:    detail,
		IP:        meta.IP,
		UserAgent: meta.UserAgent,
	})
	if err != nil {
		// An audit write failing must not fail the request it describes,
		// but it must be loud.
		s.log.ErrorContext(ctx, "could not record security event",
			slog.String("kind", kind),
			slog.String("error", err.Error()))
	}
}

func errInvalidCredentials() error {
	return skerr.Public(skerr.ErrUnauthorized, "Email or password is incorrect.")
}

func errInvalidRefresh() error {
	return skerr.Public(skerr.ErrUnauthorized, "Your session is no longer valid. Sign in again.")
}

func normaliseEmail(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	if e == "" || len(e) > maxEmailLen {
		return "", skerr.Invalid(map[string]string{"email": "Enter an email address."})
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return "", skerr.Invalid(map[string]string{"email": "That does not look like an email address."})
	}
	// Lowercased for storage as well as the citext column, so exported data
	// and log lines agree with the database's comparison semantics.
	return strings.ToLower(addr.Address), nil
}

func validatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minPasswordLen:
		return skerr.Invalid(map[string]string{
			"password": fmt.Sprintf("Use at least %d characters.", minPasswordLen),
		})
	case n > maxPasswordLen:
		return skerr.Invalid(map[string]string{
			"password": fmt.Sprintf("Use at most %d characters.", maxPasswordLen),
		})
	}
	// No composition rules. Length is what matters, and character-class
	// requirements push people toward Passw0rd!.
	return nil
}
