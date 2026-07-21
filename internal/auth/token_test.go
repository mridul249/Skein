package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func testIssuer() *TokenIssuer {
	return NewTokenIssuer(strings.Repeat("k", 48), 15*time.Minute)
}

func TestIssueAndVerifyAccessToken(t *testing.T) {
	iss := testIssuer()
	userID, sessionID := uuid.New(), uuid.New()

	tok, exp, err := iss.IssueAccessToken(userID, sessionID)
	if err != nil {
		t.Fatalf("IssueAccessToken() = %v", err)
	}
	if !exp.After(time.Now()) {
		t.Error("token already expired at issue")
	}

	p, err := iss.VerifyAccessToken(tok)
	if err != nil {
		t.Fatalf("VerifyAccessToken() = %v", err)
	}
	if p.UserID != userID {
		t.Errorf("UserID = %v, want %v", p.UserID, userID)
	}
	if p.SessionID != sessionID {
		t.Errorf("SessionID = %v, want %v", p.SessionID, sessionID)
	}
}

// Rules.md §2.8: the access token carries sub and sid only.
func TestAccessTokenCarriesNothingExtra(t *testing.T) {
	tok, _, err := testIssuer().IssueAccessToken(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("IssueAccessToken() = %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	allowed := map[string]bool{"sub": true, "sid": true, "iat": true, "nbf": true, "exp": true, "jti": true}
	for k := range payload {
		if !allowed[k] {
			t.Errorf("unexpected claim %q in the access token", k)
		}
	}
}

func TestVerifyAccessTokenRejectsBadTokens(t *testing.T) {
	iss := testIssuer()
	other := NewTokenIssuer(strings.Repeat("z", 48), 15*time.Minute)

	valid, _, err := iss.IssueAccessToken(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("IssueAccessToken() = %v", err)
	}
	fromOther, _, err := other.IssueAccessToken(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("IssueAccessToken() = %v", err)
	}

	// alg=none is the classic JWT forgery; ParseWithClaims pins HS256.
	noneToken := forgeAlgNone(t)

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not.a.token"},
		{"tampered payload", tamper(valid)},
		{"signed with another key", fromOther},
		{"alg none", noneToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := iss.VerifyAccessToken(tc.token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("VerifyAccessToken() = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerifyAccessTokenRejectsExpired(t *testing.T) {
	iss := testIssuer()
	tok, _, err := iss.IssueAccessToken(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("IssueAccessToken() = %v", err)
	}

	// Move the issuer's clock past the expiry rather than sleeping.
	iss.now = func() time.Time { return time.Now().Add(16 * time.Minute) }

	if _, err := iss.VerifyAccessToken(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("VerifyAccessToken() = %v, want ErrInvalidToken for an expired token", err)
	}
}

func TestNewRefreshTokenIsRandomAndHashed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, hash, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("NewRefreshToken() = %v", err)
		}
		if seen[tok] {
			t.Fatal("NewRefreshToken() repeated a token")
		}
		seen[tok] = true

		want := sha256.Sum256([]byte(tok))
		if string(hash) != string(want[:]) {
			t.Fatal("returned hash is not SHA-256 of the token")
		}
		if strings.Contains(string(hash), tok) {
			t.Fatal("the hash contains the token")
		}

		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token is not raw-url base64: %v", err)
		}
		if len(raw) != refreshTokenLen {
			t.Fatalf("token entropy = %d bytes, want %d", len(raw), refreshTokenLen)
		}
	}
}

func tamper(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return token
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return token
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) == nil {
		claims["sub"] = uuid.NewString()
		if b, merr := json.Marshal(claims); merr == nil {
			parts[1] = base64.RawURLEncoding.EncodeToString(b)
		}
	}
	return strings.Join(parts, ".")
}

func forgeAlgNone(t *testing.T) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		SessionID: uuid.NewString(),
	})
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("forge alg=none token: %v", err)
	}
	return signed
}
