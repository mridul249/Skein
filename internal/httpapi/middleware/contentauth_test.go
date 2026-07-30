package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/capability"
	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/httpapi/httpx"
	"github.com/mridul60214/skein/internal/httpapi/middleware"
)

// stubVerifier accepts exactly one bearer token.
type stubVerifier struct{ user uuid.UUID }

func (s stubVerifier) VerifyAccessToken(token string) (middleware.Principal, error) {
	if token == "valid" {
		return middleware.Principal{UserID: s.user, SessionID: uuid.New()}, nil
	}
	return middleware.Principal{}, http.ErrNoCookie
}

func newSigner(t *testing.T) *capability.Signer {
	t.Helper()
	master := make([]byte, skcrypto.KeyLen)
	for i := range master {
		master[i] = byte(i * 7)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	s, err := capability.NewSigner(ring)
	if err != nil {
		t.Fatalf("NewSigner() = %v", err)
	}
	return s
}

// contentRoute builds the middleware over a handler that reports which user it
// was reached as, with the file id taken from the last path segment the way the
// real route takes it from chi.
func contentRoute(t *testing.T, v middleware.TokenVerifier, caps middleware.CapabilityVerifier) http.Handler {
	t.Helper()
	fileID := func(r *http.Request) (uuid.UUID, bool) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 {
			return uuid.Nil, false
		}
		id, err := uuid.Parse(parts[len(parts)-2])
		if err != nil {
			return uuid.Nil, false
		}
		return id, true
	}
	return middleware.ContentAuth(v, caps, fileID, httpx.WriteError)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, _ := middleware.UserIDFrom(r.Context())
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("served to " + uid.String()))
		}))
}

func contentURL(fileID uuid.UUID, q url.Values) string {
	return "/api/files/" + fileID.String() + "/content?" + q.Encode()
}

// TestContentAuthAcceptsBothCredentials pins the two ways in and, more
// importantly, every way that must stay shut.
func TestContentAuthAcceptsBothCredentials(t *testing.T) {
	user := uuid.New()
	signer := newSigner(t)
	h := contentRoute(t, stubVerifier{user: user}, signer)

	fileA, fileB := uuid.New(), uuid.New()
	grantA := signer.Sign(fileA, user, time.Now().Add(capability.TTL))

	tests := []struct {
		name   string
		method string
		target string
		bearer string
		want   int
	}{
		{"bearer token, no grant", http.MethodGet,
			"/api/files/" + fileA.String() + "/content", "valid", http.StatusOK},
		{"valid grant, no bearer", http.MethodGet,
			contentURL(fileA, grantA), "", http.StatusOK},
		{"valid grant on a HEAD", http.MethodHead,
			contentURL(fileA, grantA), "", http.StatusOK},

		{"no credential at all", http.MethodGet,
			"/api/files/" + fileA.String() + "/content", "", http.StatusUnauthorized},
		{"bad bearer beats a valid grant", http.MethodGet,
			contentURL(fileA, grantA), "wrong", http.StatusUnauthorized},

		// The property the exit criteria name: A's grant at B's URL.
		{"grant for file A presented at file B", http.MethodGet,
			contentURL(fileB, grantA), "", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.RemoteAddr = "198.51.100.5:1234"
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && tc.method == http.MethodGet {
				if got := rec.Body.String(); got != "served to "+user.String() {
					t.Errorf("reached handler as %q, want user %s", got, user)
				}
			}
		})
	}
}

// TestExpiredGrantIsRefused covers the time dimension at the HTTP layer, since
// the middleware is where "verified once, at request start" actually happens.
func TestExpiredGrantIsRefused(t *testing.T) {
	user := uuid.New()
	signer := newSigner(t)
	h := contentRoute(t, stubVerifier{user: user}, signer)
	fileID := uuid.New()

	expired := signer.Sign(fileID, user, time.Now().Add(-time.Second))
	req := httptest.NewRequest(http.MethodGet, contentURL(fileID, expired), nil)
	req.RemoteAddr = "198.51.100.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired grant: status = %d, want 401", rec.Code)
	}
}

// TestBadGrantsAreIndistinguishable is the reason the errors are not
// differentiated: the response to an expired grant must not tell an attacker
// that their forgery was otherwise well formed.
func TestBadGrantsAreIndistinguishable(t *testing.T) {
	user := uuid.New()
	signer := newSigner(t)
	h := contentRoute(t, stubVerifier{user: user}, signer)
	fileID := uuid.New()

	expired := signer.Sign(fileID, user, time.Now().Add(-time.Second))

	forged := signer.Sign(fileID, user, time.Now().Add(capability.TTL))
	forged.Set(capability.ParamSignature, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	tampered := signer.Sign(fileID, user, time.Now().Add(capability.TTL))
	tampered.Set(capability.ParamExpires, "99999999999")

	bodies := map[string]string{}
	for name, q := range map[string]url.Values{
		"expired": expired, "forged": forged, "tampered expiry": tampered,
	} {
		req := httptest.NewRequest(http.MethodGet, contentURL(fileID, q), nil)
		req.RemoteAddr = "198.51.100.5:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rec.Code)
		}
		bodies[name] = rec.Body.String()
	}

	var first string
	for name, b := range bodies {
		if first == "" {
			first = b
			continue
		}
		if b != first {
			t.Errorf("%s answered %q, but another case answered %q; the two are distinguishable",
				name, b, first)
		}
	}
}

// TestAccessLogNeverCarriesTheSignature is the scrubbing requirement. The log
// records the path and never the query, so a signature cannot be lifted out of
// a log file and replayed inside its fifteen minutes.
func TestAccessLogNeverCarriesTheSignature(t *testing.T) {
	user := uuid.New()
	signer := newSigner(t)
	fileID := uuid.New()
	grant := signer.Sign(fileID, user, time.Now().Add(capability.TTL))
	sig := grant.Get(capability.ParamSignature)

	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := middleware.RequestID(middleware.AccessLog(lg)(
		contentRoute(t, stubVerifier{user: user}, signer)))

	req := httptest.NewRequest(http.MethodGet, contentURL(fileID, grant), nil)
	req.RemoteAddr = "198.51.100.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("nothing was logged; this test would pass vacuously")
	}
	// Confirm the line really is about this request before asserting on
	// what it omits.
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, logged)
	}
	if line["path"] != "/api/files/"+fileID.String()+"/content" {
		t.Fatalf("logged path = %v, want the content path", line["path"])
	}

	if strings.Contains(logged, sig) {
		t.Errorf("the access log contains the capability signature:\n%s", logged)
	}
	for _, param := range []string{capability.ParamSignature + "=", "?"} {
		if strings.Contains(logged, param) {
			t.Errorf("the access log contains query material (%q):\n%s", param, logged)
		}
	}
}
