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
