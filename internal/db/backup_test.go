package db_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `make backup` is the documented disaster-recovery path (docs/BACKUP.md), and
// its failure mode used to be the worst one a backup tool can have: silent
// success. The recipe was
//
//	pg_dump ... | gzip > "$out"; echo "wrote $out"
//
// and sh reports the exit status of the LAST command in a pipeline, so a
// pg_dump that never connected still produced a valid, empty .gz and printed
// "wrote backups/skein-...sql.gz (20 bytes)". Nothing failed. You would find
// out at restore time.
//
// These tests drive the real recipe through make rather than re-implementing
// it, because the bug lived in the shell semantics, not in any Go code — a
// reimplementation would have been written with pipefail and proved nothing.
//
// Skipped under -short and when psql/pg_dump are absent: this shells out to
// real tooling. It needs no live database — the failure path is the point, and
// it is driven with a URL that deliberately cannot connect.
func TestBackupFailsLoudlyWhenPgDumpFails(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to make/pg_dump")
	}
	requireTools(t)
	repo := repoRoot(t)

	// A backups/ entry created by this run would be the bug reappearing, so
	// record what was there first and compare after.
	before := backupFiles(t, repo)

	cmd := exec.Command("make", "backup")
	cmd.Dir = repo
	// An unreachable database. PGCONNECT_TIMEOUT keeps a DNS/connect stall
	// from hanging the suite, and PGPASSWORD stops psql prompting on a TTY.
	cmd.Env = append(os.Environ(),
		"SKEIN_DATABASE_URL=postgres://nobody@127.0.0.1:1/does_not_exist?sslmode=disable",
		"PGCONNECT_TIMEOUT=5",
		"PGPASSWORD=",
	)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Errorf("make backup succeeded against an unreachable database; output:\n%s", out)
	}
	if !strings.Contains(string(out), "backup FAILED") {
		t.Errorf("output does not say the backup failed; a silent failure is the bug:\n%s", out)
	}
	// "wrote ..." is the success line. It must not appear on a failed run.
	if strings.Contains(string(out), "wrote backups/") {
		t.Errorf("failed backup still reported writing a file:\n%s", out)
	}

	after := backupFiles(t, repo)
	if len(after) != len(before) {
		t.Errorf("backups/ gained %d file(s) from a failed run: %v",
			len(after)-len(before), diff(before, after))
	}
	// A half-written dump must not be left behind under any name.
	for _, f := range after {
		if strings.Contains(f, ".partial") {
			t.Errorf("failed run left a partial file: %s", f)
		}
	}
}

// The guard above must not have been bought by breaking the success path — a
// backup command that always fails would pass the first test perfectly.
//
// Needs a reachable database, so it skips (rather than fails) when
// SKEIN_DATABASE_URL is unset or unreachable: most contributors will not have
// one, and the failure-path test above carries the regression guard.
func TestBackupWritesARestorableDumpOnSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to make/pg_dump")
	}
	requireTools(t)
	repo := repoRoot(t)

	url := databaseURL(t, repo)
	if url == "" {
		t.Skip("no SKEIN_DATABASE_URL configured")
	}
	if err := exec.Command("psql", url, "-qAt", "-c", "SELECT 1").Run(); err != nil {
		t.Skipf("database not reachable: %v", err)
	}

	before := backupFiles(t, repo)

	cmd := exec.Command("make", "backup")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "SKEIN_DATABASE_URL="+url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make backup = %v; output:\n%s", err, out)
	}

	after := backupFiles(t, repo)
	created := diff(before, after)
	if len(created) != 1 {
		t.Fatalf("expected exactly one new backup, got %v", created)
	}
	// Clean up: this is a real dump of the developer's database.
	path := filepath.Join(repo, "backups", created[0])
	t.Cleanup(func() { _ = os.Remove(path) })

	// The bug produced a *valid* empty gzip, so "the file exists" and even
	// "it gunzips" are both true of the broken output. Assert on content.
	dump, err := exec.Command("gunzip", "-c", path).Output()
	if err != nil {
		t.Fatalf("dump is not readable gzip: %v", err)
	}
	if len(dump) == 0 {
		t.Fatal("dump is empty; this is exactly the silent failure the fix targets")
	}
	// docs/BACKUP.md promises these specifically: the citext extension (the
	// restore fails without it) and goose_db_version *with its rows*, so a
	// restore lands at a known version instead of re-running every migration.
	for _, want := range []string{
		"CREATE EXTENSION IF NOT EXISTS citext",
		"COPY public.goose_db_version",
	} {
		if !strings.Contains(string(dump), want) {
			t.Errorf("dump is missing %q, which docs/BACKUP.md says restoring depends on", want)
		}
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"make", "psql", "pg_dump", "gunzip"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// databaseURL reads SKEIN_DATABASE_URL from the environment, falling back to
// .env the same way the Makefile does.
func databaseURL(t *testing.T, repo string) string {
	t.Helper()
	if v := os.Getenv("SKEIN_DATABASE_URL"); v != "" {
		return v
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SKEIN_DATABASE_URL="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func backupFiles(t *testing.T, repo string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repo, "backups"))
	if err != nil {
		return nil // no backups/ yet is fine; make creates it
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// diff returns names in after that are not in before.
func diff(before, after []string) []string {
	seen := make(map[string]bool, len(before))
	for _, b := range before {
		seen[b] = true
	}
	var out []string
	for _, a := range after {
		if !seen[a] {
			out = append(out, a)
		}
	}
	return out
}
