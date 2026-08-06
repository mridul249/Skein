package handlers

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
)

// The manifest routes drop the operator token on the DESKTOP build only.
//
// WHY THE DIFFERENCE IS SAFE, and why it is not a general relaxation: the
// server build is reachable by anyone who can hit the port and registration is
// open, so a token is the only thing standing between a stranger and writes to
// every connected drive. The desktop build binds loopback on a random port
// inside the user's own session, and the person clicking the button IS the
// operator.
//
// The cost of NOT doing this was concrete: a desktop install has no reason to
// set SKEIN_BACKUP_TOKEN, so the Recovery UI could only explain why it was
// unable to act, and repairing manifests needed a separate command-line tool —
// a bad procedure for something run during a disaster.
func TestManifestRoutesRequireTheTokenOnTheServerBuildOnly(t *testing.T) {
	t.Run("server build without a token refuses", func(t *testing.T) {
		h := &System{}
		if h.manifestsGateOpen(httptest.NewRequest("POST", "/", nil)) {
			t.Error("the gate opened with no token and no desktop flag")
		}
	})

	t.Run("server build with the wrong token refuses", func(t *testing.T) {
		h := &System{token: "correct"}
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set(BackupTokenHeader, "wrong")
		if h.manifestsGateOpen(r) {
			t.Error("a wrong token opened the gate")
		}
	})

	t.Run("server build with the right token allows", func(t *testing.T) {
		h := &System{token: "correct"}
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set(BackupTokenHeader, "correct")
		if !h.manifestsGateOpen(r) {
			t.Error("the correct token did not open the gate")
		}
	})

	t.Run("desktop build allows without a token", func(t *testing.T) {
		h := &System{}
		h.AllowWithoutOperatorToken()
		if !h.manifestsGateOpen(httptest.NewRequest("POST", "/", nil)) {
			t.Error("the desktop build still demanded a token; the Recovery UI " +
				"cannot write manifests and the user is sent to a CLI")
		}
	})
}

// THE RELAXATION IS SCOPED TO MANIFESTS. Key export and the database dump hand
// over a secret, and the "the user is the operator" argument does not extend to
// them: a desktop process is still reachable by anything else running as that
// user, and those two routes are the ones worth stealing.
func TestTheDesktopRelaxationDoesNotReachTheSecretRoutes(t *testing.T) {
	master := make([]byte, skcrypto.KeyLen)
	for i := range master {
		master[i] = 9
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	// A KEYRING IS WIRED, deliberately. Without one ExportKey 404s because the
	// feature is unavailable, and the test would pass whatever the gate did —
	// which is exactly what happened the first time this was written: the
	// mutation that opened key export to the desktop flag did NOT turn it red.
	h := &System{keyring: ring, log: discardLogger()}
	h.AllowWithoutOperatorToken()

	if h.token != "" {
		t.Fatal("AllowWithoutOperatorToken must not invent a token")
	}

	rec := httptest.NewRecorder()
	h.ExportKey(rec, httptest.NewRequest("GET", "/api/system/key-export", nil))
	if rec.Code != 404 {
		t.Errorf("key export returned %d on a desktop build with no token, want 404; "+
			"the manifest relaxation has leaked into a secret-bearing route", rec.Code)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
