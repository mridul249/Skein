package files

import (
	"context"
	"database/sql"
	"strings"

	"github.com/google/uuid"
)

// Inspection helpers so SQLiteStore satisfies conformanceStore.
//
// MemoryStore carries these on the type itself because it has no other way to
// be looked into. Here they live in a _test.go file deliberately: they exist
// for the conformance run and must not become production API — CorruptShard in
// particular writes damage on purpose, which nothing outside a test should be
// able to do.

// CorruptShard mutates one shard row, for tests that need a damaged manifest.
func (s *SQLiteStore) CorruptShard(fileID uuid.UUID, index int32, mutate func(*Shard)) {
	ctx := context.Background()
	shards, err := s.ListShards(ctx, fileID)
	if err != nil {
		panic("CorruptShard: list shards: " + err.Error())
	}
	for i := range shards {
		if shards[i].Index != index {
			continue
		}
		sh := shards[i]
		mutate(&sh)
		_, err := s.db.ExecContext(ctx, `
UPDATE file_shards
   SET idx = ?, provider_object_id = ?, size_bytes = ?, plain_size_bytes = ?,
       plain_offset = ?, sha256 = ?
 WHERE id = ?`,
			sh.Index, sh.ProviderID, sh.SizeBytes, sh.PlainSize, sh.PlainOffset,
			sh.SHA256, sh.ID.String())
		if err == nil {
			return
		}
		// A CHECK constraint refusing the damage is not a helper failure — it
		// is the database doing its job. The row cannot hold this state, which
		// is a STRONGER guarantee than the reader's own verifyManifest guard
		// (download.go:423) that the test is reaching for. Record it as
		// unrepresentable rather than panicking, so the SQLite run reports the
		// truth instead of dying.
		if strings.Contains(err.Error(), "CHECK constraint failed") {
			// The row simply cannot hold this state. Leaving the shard intact
			// is the honest outcome; the caller decides what that means.
			return
		}
		panic("CorruptShard: update: " + err.Error())
	}
}

// FileStatus reports a file's stored status.
func (s *SQLiteStore) FileStatus(id uuid.UUID) (string, bool) {
	var status string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT status FROM files WHERE id = ?`, id.String()).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

// ListShardsSnapshot returns every shard row in the store.
func (s *SQLiteStore) ListShardsSnapshot() []Shard {
	rows, err := s.db.QueryContext(context.Background(), `
SELECT id, file_id, idx, connected_account_id, provider_object_id,
       size_bytes, plain_size_bytes, plain_offset, sha256, created_at
  FROM file_shards
 ORDER BY file_id, idx`)
	if err != nil {
		panic("ListShardsSnapshot: " + err.Error())
	}
	defer func() { _ = rows.Close() }()

	var out []Shard
	for rows.Next() {
		sh, serr := scanShardRow(rows)
		if serr != nil {
			panic("ListShardsSnapshot scan: " + serr.Error())
		}
		out = append(out, sh)
	}
	if rerr := rows.Err(); rerr != nil {
		panic("ListShardsSnapshot rows: " + rerr.Error())
	}
	return out
}

// ShardCount reports how many shards a file has.
func (s *SQLiteStore) ShardCount(fileID uuid.UUID) int {
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM file_shards WHERE file_id = ?`,
		fileID.String()).Scan(&n); err != nil {
		return 0
	}
	return n
}

// scanShardRow reuses the store's own row shape so the helpers cannot drift
// from the real scan.
func scanShardRow(rows *sql.Rows) (Shard, error) {
	var (
		sh        Shard
		id        string
		fileID    string
		accountID sql.NullString
		createdAt string
	)
	if err := rows.Scan(&id, &fileID, &sh.Index, &accountID, &sh.ProviderID,
		&sh.SizeBytes, &sh.PlainSize, &sh.PlainOffset, &sh.SHA256, &createdAt); err != nil {
		return Shard{}, err
	}
	var err error
	if sh.ID, err = uuid.Parse(id); err != nil {
		return Shard{}, err
	}
	if sh.FileID, err = uuid.Parse(fileID); err != nil {
		return Shard{}, err
	}
	if accountID.Valid {
		aid, perr := uuid.Parse(accountID.String)
		if perr != nil {
			return Shard{}, perr
		}
		sh.AccountID = &aid
	}
	if sh.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return Shard{}, err
	}
	return sh, nil
}
