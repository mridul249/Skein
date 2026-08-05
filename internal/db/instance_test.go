package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Known issue #48.
//
// SKEIN_MASTER_KEY had no in-band validation: the wrong key started the server
// fine and failed at the first download, three layers down, as a decryption
// error. That failure reads as data corruption, which is the worst possible
// misdiagnosis — a user concludes their files are gone when they have simply
// restored the wrong key file.
//
// Block 2 shipped export and recovery against that constraint and documented a
// manual comparison of two hex strings, performed by a human, during a
// recovery, under stress. These tests pin the automatic version.

func migratedSQLite(t *testing.T) *instanceDB {
	t.Helper()
	dbh := openMigrationDB(t)
	if err := goose.UpContext(context.Background(), dbh, "migrations/sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &instanceDB{db: dbh, dialect: DialectSQLite}
}

// First boot records the key id. There is nothing to compare against yet, and
// refusing to start would make a fresh install impossible.
func TestVerifyMasterKeyIDRecordsOnFirstBoot(t *testing.T) {
	store := migratedSQLite(t)
	ctx := context.Background()

	adopted, err := store.VerifyMasterKeyID(ctx, "723bcc0a")
	if err != nil {
		t.Fatalf("first boot refused: %v", err)
	}
	if !adopted {
		t.Error("first boot did not report adopting the key, so startup cannot warn about it")
	}

	got, ok, err := store.MasterKeyID(ctx)
	if err != nil {
		t.Fatalf("MasterKeyID() = %v", err)
	}
	if !ok {
		t.Fatal("first boot did not record the key id, so the next boot has nothing to check")
	}
	if got != "723bcc0a" {
		t.Errorf("recorded key id = %q, want %q", got, "723bcc0a")
	}
}

// The same key on every subsequent boot is accepted, and recording is
// idempotent — a second row would make "which one is authoritative" a real
// question, which is why the schema pins it to one.
func TestVerifyMasterKeyIDAcceptsTheSameKey(t *testing.T) {
	store := migratedSQLite(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		adopted, err := store.VerifyMasterKeyID(ctx, "723bcc0a")
		if err != nil {
			t.Fatalf("boot %d refused the correct key: %v", i+1, err)
		}
		if adopted != (i == 0) {
			t.Errorf("boot %d reported adopted=%v; only the FIRST boot adopts", i+1, adopted)
		}
	}

	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM instance_metadata`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("instance_metadata has %d rows, want 1", rows)
	}
}

// THE POINT OF THE WHOLE FEATURE.
//
// A different key is refused at startup, before a single byte of user data is
// read, and the message names the actual cause.
func TestVerifyMasterKeyIDRefusesADifferentKey(t *testing.T) {
	store := migratedSQLite(t)
	ctx := context.Background()

	if _, err := store.VerifyMasterKeyID(ctx, "723bcc0a"); err != nil {
		t.Fatalf("first boot: %v", err)
	}

	_, err := store.VerifyMasterKeyID(ctx, "6731d8dc")
	if err == nil {
		t.Fatal("a DIFFERENT master key was accepted; recovery would proceed and " +
			"fail later as a decryption error, which reads as data corruption")
	}
	if !errors.Is(err, ErrMasterKeyMismatch) {
		t.Errorf("error = %v, want ErrMasterKeyMismatch", err)
	}

	// The wording is load-bearing: it is read by someone under stress who must
	// conclude "wrong key file", not "my data is corrupt".
	msg := err.Error()
	if !strings.Contains(msg, "different Skein instance") {
		t.Errorf("message %q does not say the key belongs to a different instance", msg)
	}
	// Both ids appear, so the operator can see which file they grabbed.
	for _, want := range []string{"723bcc0a", "6731d8dc"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry key id %s", msg, want)
		}
	}
	// And it must not read as a data problem.
	for _, forbidden := range []string{"corrupt", "decrypt"} {
		if strings.Contains(strings.ToLower(msg), forbidden) {
			t.Errorf("message %q suggests %q, which is the misdiagnosis this exists to prevent",
				msg, forbidden)
		}
	}
}

// A mismatch must not overwrite the stored id. Recording the wrong key would
// make the SECOND attempt with the wrong key succeed, turning a permanent
// refusal into a one-time warning.
func TestAMismatchDoesNotOverwriteTheStoredKeyID(t *testing.T) {
	store := migratedSQLite(t)
	ctx := context.Background()

	if _, err := store.VerifyMasterKeyID(ctx, "723bcc0a"); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	_, _ = store.VerifyMasterKeyID(ctx, "6731d8dc")

	if _, err := store.VerifyMasterKeyID(ctx, "6731d8dc"); err == nil {
		t.Fatal("the wrong key was accepted on a second attempt: the mismatch " +
			"overwrote the stored id, so the check is a one-time warning")
	}
	got, _, err := store.MasterKeyID(ctx)
	if err != nil {
		t.Fatalf("MasterKeyID() = %v", err)
	}
	if got != "723bcc0a" {
		t.Errorf("stored key id = %q after a rejected boot, want the original %q",
			got, "723bcc0a")
	}
}
