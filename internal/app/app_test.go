package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

const (
	testMasterKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	testJWTSecret = "0123456789abcdef0123456789abcdef0123456789"
)

func TestBuildWithSQLiteDoesNotRequirePostgres(t *testing.T) {
	t.Setenv("SKEIN_DATABASE_URL", "")
	t.Setenv("SKEIN_MASTER_KEY", testMasterKey)
	t.Setenv("SKEIN_JWT_SECRET", testJWTSecret)
	t.Setenv("SKEIN_LOG_LEVEL", "error")

	path := filepath.Join(t.TempDir(), "skein.db")
	a, err := Build(context.Background(), WithSQLiteDatabase(path))
	if err != nil {
		t.Fatalf("Build() with SQLite = %v", err)
	}
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
	})

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite for inspection: %v", err)
	}
	defer db.Close() //nolint:errcheck // Test cleanup.

	for _, table := range []string{
		"users",
		"connected_accounts",
		"storage_accounts",
		"folders",
		"files",
		"file_shards",
		"quota_reservations",
	} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("sqlite table %s was not migrated: %v", table, err)
		}
	}
}

// A DESKTOP RUN MUST NOT BE TOLD TO SET SERVER VARIABLES.
//
// Found 2026-08-06 by following docs/SETUP.md from a clean environment. A
// desktop start with no Google credentials printed two warnings in a row:
//
//	google oauth is not configured; drives cannot be connected
//	  fix=set SKEIN_GOOGLE_CLIENT_ID, _SECRET and _REDIRECT_URL
//	desktop oauth credentials are incomplete; ...
//
// The first names three variables the desktop build never reads. Setting all
// three changes nothing, and it sits directly above the warning naming the two
// that would actually help — so the more prominent advice is the wrong advice.
func TestWebOAuthWarningIsServerOnly(t *testing.T) {
	if shouldWarnAboutWebOAuth(true) {
		t.Error("a desktop build was told to set SKEIN_GOOGLE_CLIENT_ID/_SECRET/_REDIRECT_URL, " +
			"none of which it reads; the desktop warning names the right pair")
	}
	if !shouldWarnAboutWebOAuth(false) {
		t.Error("a server build with no Google credentials was told nothing; " +
			"drives cannot be connected and the reason must be said out loud")
	}
}
