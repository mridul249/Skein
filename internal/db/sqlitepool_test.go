package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A LIVE BUG, reported 2026-08-06: a 400 MB upload stalled at 4.1 MB and the
// log filled with
//
//	set capacity error: set storage account error: context deadline exceeded
//
// on all eight storage accounts. Drive was healthy; the failing operation was
// a DATABASE WRITE.
//
// THE MECHANISM. OpenSQLite set busy_timeout but never SetMaxOpenConns, so
// database/sql opened connections without limit. SQLite permits exactly one
// writer, so N connections means N goroutines contending for one write lock —
// and the quota worker runs under a 3s deadline (app.go) against a 5s busy
// timeout, so the deadline always lost. A long upload holds the lock across
// its CreateShard writes, the 30s quota sweep fires into that window, and
// everything else queues behind a lock it will never win.
//
// The user's second observation is the same bug from the other side: a SMALL
// upload also stalled when started while a large one was running.
//
// WHY THE FIX IS A CONNECTION LIMIT RATHER THAN A LONGER TIMEOUT. Raising
// busy_timeout alone leaves writers racing — whoever grabs the lock wins and
// the losers spin, so the failure returns under enough load. Capping the pool
// makes database/sql QUEUE writers in Go, which is fair, bounded, and turns
// lock contention into a wait rather than an error. The longer busy_timeout
// stays as a backstop for the connections SQLite still handles internally.
//
// These tests drive the real OpenSQLite, not a hand-rolled DSN, so they check
// the configuration the desktop binary actually ships.

func openDesktopDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "skein.db")
	sqlDB, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenSQLite() = %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB, path
}

// Concurrent writers under sustained write pressure must all succeed within
// the quota worker's own deadline.
//
// HONEST LIMITATION, STATED RATHER THAN IMPLIED: this test is NOT the
// load-bearing one. It passes against the unfixed pool as well — verified by
// mutation — because reproducing the live failure needs the lock held longer
// than the writers' deadline, and the production upload path does not do that
// (CreateShard is a single INSERT; the only transaction in that store is
// short). An earlier draft of this test DID hold a 4s transaction and did go
// red, but it was modelling a shape the system does not have, which is a
// fixture manufacturing a difference rather than absorbing one.
//
// What actually pins the fix is the two CONFIGURATION tests below, which go
// red under mutation. This one is kept as a regression guard against a future
// change that reintroduces long-held write transactions — it would catch that
// where the config tests could not.
func TestConcurrentWritersDoNotExhaustTheirDeadline(t *testing.T) {
	sqlDB, _ := openDesktopDB(t)
	ctx := context.Background()

	if _, err := sqlDB.ExecContext(ctx,
		`CREATE TABLE contention (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO contention (id, v) VALUES (1, 'seed')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// SUSTAINED WRITE PRESSURE, which is what an upload actually produces.
	//
	// The production upload path does NOT hold a long transaction — it issues
	// one INSERT per shard (files/sqlitestore.go CreateShard), and the only
	// transaction in that store is SoftDeleteFolderTree, which is short. So
	// modelling this as "one goroutine holds the lock for four seconds" would
	// be testing a shape the system does not have. Checked rather than
	// assumed.
	//
	// What it DOES produce is a steady stream of short writes for the length
	// of a multi-minute transfer, with the 30s quota sweep firing into it. The
	// writer below reproduces that.
	stop := make(chan struct{})
	var pressure sync.WaitGroup
	pressure.Add(1)
	go func() {
		defer pressure.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = sqlDB.ExecContext(ctx, `UPDATE contention SET v = 'upload' WHERE id = 1`)
		}
	}()
	t.Cleanup(func() { close(stop); pressure.Wait() })

	// Eight writers under the quota worker's 3s deadline, one per storage
	// account, as the live failure had.
	const writers = 8
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []error
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second) // the quota worker's own deadline
			defer cancel()
			_, err := sqlDB.ExecContext(wctx,
				`UPDATE contention SET v = ? WHERE id = 1`, "writer")
			if err != nil {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(failed) > 0 {
		t.Errorf("%d of %d concurrent writers failed while one held the write lock; "+
			"first error: %v\n"+
			"This is the live failure: quota sync could not write while an upload "+
			"held the lock, and the upload stalled.", len(failed), writers, failed[0])
		for _, e := range failed {
			if errors.Is(e, context.DeadlineExceeded) {
				t.Log("  (deadline exceeded — the writer never got the lock)")
				break
			}
		}
	}
}

// The pool is capped, asserted directly so the guarantee is on the
// CONFIGURATION rather than inferred from a timing test that could pass on a
// fast machine for the wrong reason.
func TestTheDesktopPoolIsBoundedForSQLitesSingleWriter(t *testing.T) {
	sqlDB, _ := openDesktopDB(t)

	// Force many concurrent operations so database/sql would open as many
	// connections as it is permitted to.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			_ = sqlDB.QueryRow(`SELECT count(*) FROM goose_db_version`).Scan(&n)
		}()
	}
	wg.Wait()

	if got := sqlDB.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1.\n"+
			"SQLite permits ONE writer. An unbounded pool turns that into N "+
			"goroutines racing a busy_timeout, which is how a 400 MB upload "+
			"stalled at 4.1 MB while quota sync failed on every account.", got)
	}
	if got := sqlDB.Stats().OpenConnections; got > 1 {
		t.Errorf("OpenConnections = %d after 32 concurrent reads, want at most 1", got)
	}
}

// The busy timeout is a backstop, not the primary defence, but it must still
// be generous enough that a checkpoint or a slow write does not surface as an
// error. Asserted by reading it back from the connection.
func TestTheBusyTimeoutIsGenerous(t *testing.T) {
	sqlDB, _ := openDesktopDB(t)

	var ms int
	if err := sqlDB.QueryRow(`PRAGMA busy_timeout`).Scan(&ms); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if ms < 15000 {
		t.Errorf("busy_timeout = %dms, want at least 15000.\n"+
			"5s was below the quota worker's own 3s deadline in practice, so a "+
			"contended write reported failure rather than waiting.", ms)
	}
}
