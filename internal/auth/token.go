package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/httpapi/middleware"
)

// refreshTokenLen is 32 bytes of crypto/rand per Rules.md §2.8.
const refreshTokenLen = 32

// ErrInvalidToken reports any access token that fails validation. The reason
// is deliberately not distinguished: expired, forged and malformed all look
// the same to a caller, so nothing is leaked by probing.
var ErrInvalidToken = errors.New("auth: invalid access token")

// Claims is the access token payload. Rules.md §2.8: sub and sid only. No
// email, no role, no quota — anything else here is data that cannot be revoked
// for fifteen minutes.
type Claims struct {
	jwt.RegisteredClaims
	SessionID string `json:"sid"`
}

// TokenIssuer mints and verifies access tokens.
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewTokenIssuer builds an issuer. secret must be at least 32 bytes; config
// validation guarantees that before this is reached.
func NewTokenIssuer(secret string, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// IssueAccessToken returns a signed access token and its expiry.
func (t *TokenIssuer) IssueAccessToken(userID, sessionID uuid.UUID) (string, time.Time, error) {
	now := t.now()
	exp := now.Add(t.ttl)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
		SessionID: sessionID.String(),
	})

	signed, err := tok.SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, exp, nil
}

// VerifyAccessToken validates a token and returns its principal. The signing
// method is pinned to HS256: accepting whatever the token header claims is how
// alg=none forgeries get in.
func (t *TokenIssuer) VerifyAccessToken(raw string) (middleware.Principal, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return middleware.Principal{}, ErrInvalidToken
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return middleware.Principal{}, ErrInvalidToken
	}
	sessionID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return middleware.Principal{}, ErrInvalidToken
	}
	return middleware.Principal{UserID: userID, SessionID: sessionID}, nil
}

// NewRefreshToken returns an opaque refresh token and the SHA-256 hash that is
// stored in its place. The plaintext exists only long enough to be written to
// the response cookie.
func NewRefreshToken() (token string, hash []byte, err error) {
	b := make([]byte, refreshTokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("read refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken returns the stored form of a refresh token.
//
// A plain SHA-256 is correct here and a password KDF would not be: the input
// is 256 bits of uniform randomness, so there is no dictionary to slow down,
// and every login would otherwise pay 64 MB of argon2 for nothing.
func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
