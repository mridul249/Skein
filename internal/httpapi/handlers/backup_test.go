package handlers_test

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/mridul249/Skein/internal/db"
	"github.com/mridul249/Skein/internal/httpapi/handlers"
)

const testBackupToken = "an-operator-token-value"

// newBackupHandler builds a System over a real SQLite database.
func newBackupHandler(t *testing.T, token string) (*handlers.System, *sql.DB) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "backup.db")
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	for _, q := range []string{
		`CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY,
			version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL,
			tstamp TEXT NOT NULL)`,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp)
			VALUES (4, 1, '2026-08-05T00:00:00Z')`,
		`CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT NOT NULL)`,
		`INSERT INTO users VALUES ('u1', 'a@example.com')`,
	} {
		if _, eerr := sqlDB.Exec(q); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}

	dumper := db.NewDumper(db.DialectSQLite, "", path)
	return handlers.NewSystem(dumper, token, sqlDB,
		slog.New(slog.NewTextHandler(io.Discard, nil))), sqlDB
}

func backupRequest(t *testing.T, h *handlers.System, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/system/backup", nil)
	if token != "" {
		req.Header.Set(handlers.BackupTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.Backup(rec, req)
	return rec
}

// Unset token: the route does not exist. 404 rather than 403, because a 403
// confirms the endpoint is there and merely locked — which tells a scanner
// exactly where to come back to.
func TestBackupIs404WhenTokenUnset(t *testing.T) {
	h, _ := newBackupHandler(t, "")

	for _, presented := range []string{"", "guessed-token", testBackupToken} {
		rec := backupRequest(t, h, presented)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d with header %q, want 404", rec.Code, presented)
		}
		if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/gzip" {
			t.Error("a disabled route served an archive")
		}
	}
}

// Wrong or missing token with the feature enabled: refused, and no archive.
func TestBackupRejectsAWrongToken(t *testing.T) {
	h, _ := newBackupHandler(t, testBackupToken)

	for _, tc := range []struct{ name, token string }{
		{"missing", ""},
		{"wrong", "not-the-token"},
		{"prefix of the real token", testBackupToken[:8]},
		{"real token plus a suffix", testBackupToken + "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := backupRequest(t, h, tc.token)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct == "application/gzip" {
				t.Error("a rejected request received an archive")
			}
			if cd := rec.Header().Get("Content-Disposition"); cd != "" {
				t.Errorf("rejected request carried Content-Disposition %q", cd)
			}
		})
	}
}

// The right token, and only the right token, produces a dump.
func TestBackupSucceedsWithTheCorrectToken(t *testing.T) {
	h, _ := newBackupHandler(t, testBackupToken)

	rec := backupRequest(t, h, testBackupToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	// The filename names the dialect and the schema version, so the Postgres
	// v8 / SQLite v4 difference is visible rather than looking like drift.
	cd := rec.Header().Get("Content-Disposition")
	for _, want := range []string{"skein-backup-", "sqlite", "v4", ".sql.gz"} {
		if !contains(cd, want) {
			t.Errorf("Content-Disposition %q does not contain %q", cd, want)
		}
	}
	if rec.Body.Len() == 0 {
		t.Error("a 200 carried an empty body")
	}
}

// THE MANDATORY REGRESSION TEST, AT THE HTTP LAYER.
//
// Point the dump at a database that cannot be opened and assert the two things
// 7b23f20 got wrong: an error status, and NO archive body. A valid .gz reaching
// the client is the bug returning.
func TestBackupFailureServesAnErrorAndNoArchive(t *testing.T) {
	dumper := db.NewDumper(db.DialectSQLite, "",
		filepath.Join(t.TempDir(), "missing-dir", "does-not-exist.db"))
	h := handlers.NewSystem(dumper, testBackupToken, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := backupRequest(t, h, testBackupToken)

	if rec.Code == http.StatusOK {
		t.Fatal("a failed dump returned 200; the silent-success bug is back")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "application/gzip" {
		t.Error("a failed dump served Content-Type: application/gzip")
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("a failed dump offered a download: %q", cd)
	}
	// Whatever the body is, it must not be a usable archive.
	if gzipIsValidBody(rec.Body.Bytes()) {
		t.Fatal("a failed dump produced a VALID gzip archive; " +
			"the client would save it as a working backup")
	}
	// And the connection details never reach the client.
	if contains(rec.Body.String(), "does-not-exist.db") {
		t.Error("the error body leaked the database path")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func gzipIsValidBody(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return false
	}
	defer func() { _ = r.Close() }()
	_, err = io.ReadAll(r)
	return err == nil
}
