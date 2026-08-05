package handlers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/httpapi/handlers"
)

func newKeyExportHandler(t *testing.T, token string) (*handlers.System, *skcrypto.Keyring) {
	t.Helper()
	master := make([]byte, skcrypto.KeyLen)
	for i := range master {
		master[i] = byte(i * 11)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	h := handlers.NewSystem(nil, token, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.SetKeyring(ring)
	return h, ring
}

func keyExportRequest(h *handlers.System, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/system/key-export", nil)
	if token != "" {
		req.Header.Set(handlers.BackupTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.ExportKey(rec, req)
	return rec
}

// THE TOKEN GATE IS THE ONLY THING BETWEEN A CALLER AND THE KEY.
//
// This route hands out the single secret that decrypts every file in the
// instance. It is gated exactly as the backup route is — an operator token on
// top of a session — and there is deliberately no other way in: no query
// parameter, no "just this once" bypass, no debug flag.
//
// 404 when unset, never 403: a 403 confirms the endpoint exists and is merely
// locked, which tells a scanner precisely where to come back to. Unset must be
// indistinguishable from a build without the route.
func TestKeyExportIs404WhenTheTokenIsUnset(t *testing.T) {
	h, _ := newKeyExportHandler(t, "")

	rec := keyExportRequest(h, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; anything else confirms the route exists", rec.Code)
	}
	// Even presenting a token must not change the answer, or the difference
	// itself becomes the oracle.
	if got := keyExportRequest(h, "anything").Code; got != http.StatusNotFound {
		t.Fatalf("status with a token = %d, want 404", got)
	}
}

func TestKeyExportRefusesAWrongToken(t *testing.T) {
	h, ring := newKeyExportHandler(t, "the-real-operator-token")

	for _, bad := range []string{"", "wrong", "the-real-operator-toke", "the-real-operator-tokenX"} {
		rec := keyExportRequest(h, bad)
		if rec.Code == http.StatusOK {
			t.Fatalf("token %q was accepted", bad)
		}
		if strings.Contains(rec.Body.String(), ring.KeyIDString()) {
			t.Fatalf("a refused response leaked the key id: %s", rec.Body.String())
		}
	}
}

// The happy path: the file comes back, and it is the real one.
func TestKeyExportReturnsTheKeyFile(t *testing.T) {
	const token = "the-real-operator-token"
	h, ring := newKeyExportHandler(t, token)

	rec := keyExportRequest(h, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got, err := skcrypto.ParseKeyFile(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("the exported file does not parse: %v", err)
	}
	if len(got) != skcrypto.KeyLen {
		t.Fatalf("exported key is %d bytes, want %d", len(got), skcrypto.KeyLen)
	}
	if verr := skcrypto.VerifyKeyFileMatches(rec.Body.Bytes(), ring.KeyID()); verr != nil {
		t.Fatalf("the exported file does not match its own instance: %v", verr)
	}
}

// NOTHING ABOUT THIS ROUTE MAY BE ENUMERABLE.
//
// Sequential download ids made an earlier exposure walkable (issue #43). This
// route has no identifier at all — one instance, one key — and the response
// must not introduce one. It is also never cached, never stored by a proxy,
// and always an attachment: a key rendered inline in a browser tab lands in
// history and in the rendering process.
func TestKeyExportCarriesNoIdentifierAndIsNotCacheable(t *testing.T) {
	const token = "the-real-operator-token"
	h, _ := newKeyExportHandler(t, token)

	rec := keyExportRequest(h, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store: the key must not be cached", cc)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	// The filename must not carry a counter, a user id, or anything else that
	// invites walking. The key id is fine: it is derived and public.
	if strings.ContainsAny(cd, "0123456789") && !strings.Contains(cd, "skein") {
		t.Errorf("Content-Disposition %q looks enumerable", cd)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// The key must never reach the log, at any level, on any path.
func TestKeyExportNeverLogsKeyMaterial(t *testing.T) {
	const token = "the-real-operator-token"
	master := make([]byte, skcrypto.KeyLen)
	for i := range master {
		master[i] = byte(i * 11)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	var logged strings.Builder
	h := handlers.NewSystem(nil, token, nil,
		slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	h.SetKeyring(ring)

	// Both a refusal and a success.
	keyExportRequest(h, "wrong")
	rec := keyExportRequest(h, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	out := logged.String()
	body := rec.Body.String()
	// The base64 key as it appears in the file itself.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Key:") {
			secret := strings.TrimSpace(strings.TrimPrefix(line, "Key:"))
			if secret != "" && strings.Contains(out, secret) {
				t.Fatalf("the master key reached the log:\n%s", out)
			}
		}
	}
	if strings.Contains(out, token) {
		t.Fatalf("the operator token reached the log:\n%s", out)
	}
}

// A HEAD must not deliver the key. It is a body-less method and some clients
// issue it speculatively.
func TestKeyExportRefusesHEAD(t *testing.T) {
	const token = "the-real-operator-token"
	h, _ := newKeyExportHandler(t, token)

	req := httptest.NewRequest(http.MethodHead, "/api/system/key-export", nil)
	req.Header.Set(handlers.BackupTokenHeader, token)
	rec := httptest.NewRecorder()
	h.ExportKey(rec, req)

	if strings.Contains(rec.Body.String(), "Key:") {
		t.Fatal("a HEAD request returned key material")
	}
}

// With no keyring wired the route must 404 like any other absent feature,
// rather than 500 and confirm it exists.
func TestKeyExportIs404WithoutAKeyring(t *testing.T) {
	h := handlers.NewSystem(nil, "the-real-operator-token", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := keyExportRequest(h, "the-real-operator-token").Code; got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no keyring is configured", got)
	}
}
