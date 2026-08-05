package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sqliteInstanceTimeLayout matches the format the SQLite stores write, so a
// timestamp read back by any of them parses identically.
const sqliteInstanceTimeLayout = "2006-01-02T15:04:05.000000000Z"

func nowSQLite() string { return time.Now().UTC().Format(sqliteInstanceTimeLayout) }

// isNoRows spans both drivers, which report an empty result differently.
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows)
}

// ErrMasterKeyMismatch is returned when the supplied SKEIN_MASTER_KEY does not
// derive the key id this database was created under.
//
// It is a startup failure by design. Known issue #48: before this existed, a
// wrong key started the server perfectly and failed at the first download as a
// decryption error three layers down — and a decryption error reads as data
// corruption, so the user concludes their files are destroyed when they have
// simply restored the wrong key file. The whole value here is failing at the
// one moment the diagnosis is unambiguous, before any user data is touched.
var ErrMasterKeyMismatch = errors.New("this key belongs to a different Skein instance")

// instanceDB is the small slice of a database handle this check needs. Both
// engines are supported through one type because the statements are trivial
// and identical apart from placeholders — a full second store would be more
// code than the thing it stores.
type instanceDB struct {
	db      *sql.DB
	pool    *pgxpool.Pool
	dialect Dialect
}

// NewSQLiteInstanceStore wraps an open, migrated SQLite database.
func NewSQLiteInstanceStore(sqlDB *sql.DB) *instanceDB { //nolint:revive // returned to internal callers only
	return &instanceDB{db: sqlDB, dialect: DialectSQLite}
}

// NewPostgresInstanceStore wraps an open pgx pool.
func NewPostgresInstanceStore(pool *pgxpool.Pool) *instanceDB { //nolint:revive // returned to internal callers only
	return &instanceDB{pool: pool, dialect: DialectPostgres}
}

// MasterKeyID returns the recorded key id, and whether one has been recorded.
func (s *instanceDB) MasterKeyID(ctx context.Context) (string, bool, error) {
	const q = `SELECT key_id FROM instance_metadata WHERE id = 1`

	var keyID string
	var err error
	if s.dialect == DialectPostgres {
		err = s.pool.QueryRow(ctx, q).Scan(&keyID)
	} else {
		err = s.db.QueryRowContext(ctx, q).Scan(&keyID)
	}
	switch {
	case err == nil:
		return keyID, true, nil
	case isNoRows(err):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("read instance key id: %w", err)
	}
}

// VerifyMasterKeyID records the key id on first boot and compares it on every
// boot after that.
//
// FIRST BOOT RECORDS, IT DOES NOT REFUSE. There is nothing to compare against
// and a fresh install has to be possible; the row is written so that every
// later boot has something to check.
//
// A MISMATCH NEVER WRITES. Overwriting the stored id on a failed comparison
// would make the second attempt with the wrong key succeed, turning a
// permanent refusal into a one-time warning — and the second attempt is
// exactly what a confused operator makes.
//
// Returns adopted=true when it recorded an id rather than matching one. The
// caller must say so out loud: a database created before this check existed
// has no recorded id, so its FIRST start under ANY key adopts that key —
// including the wrong one. That window cannot be closed retroactively (there
// is nothing to fingerprint against), so the only defence is that an operator
// restoring an old database SEES the adoption happen rather than discovering
// months later that it silently accepted whatever they had to hand.
func (s *instanceDB) VerifyMasterKeyID(ctx context.Context, keyID string) (adopted bool, err error) {
	if keyID == "" {
		return false, fmt.Errorf("refusing to record an empty master key id")
	}

	stored, found, err := s.MasterKeyID(ctx)
	if err != nil {
		return false, err
	}
	if found {
		if stored != keyID {
			// Wording is load-bearing and is asserted by test. Someone reads
			// this during a recovery and must conclude "wrong key file", not
			// "my data is corrupt".
			return false, fmt.Errorf(
				"%w: this database was created with master key id %s, but the "+
					"supplied SKEIN_MASTER_KEY derives %s. Your data is intact — "+
					"this is the wrong key file. Restore the key whose exported "+
					"file names %s, or point SKEIN_SQLITE_PATH/SKEIN_DATABASE_URL "+
					"at the database this key belongs to",
				ErrMasterKeyMismatch, stored, keyID, stored)
		}
		return false, nil
	}
	if rerr := s.recordMasterKeyID(ctx, keyID); rerr != nil {
		return false, rerr
	}
	return true, nil
}

func (s *instanceDB) recordMasterKeyID(ctx context.Context, keyID string) error {
	// ON CONFLICT DO NOTHING rather than an upsert: two processes racing a
	// first boot must not end up disagreeing about which won, and there is
	// never a legitimate reason to REPLACE this value.
	if s.dialect == DialectPostgres {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO instance_metadata (id, key_id) VALUES (1, $1)
			 ON CONFLICT (id) DO NOTHING`, keyID); err != nil {
			return fmt.Errorf("record instance key id: %w", err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO instance_metadata (id, key_id, created_at)
		 VALUES (1, ?, ?)
		 ON CONFLICT (id) DO NOTHING`,
		keyID, nowSQLite()); err != nil {
		return fmt.Errorf("record instance key id: %w", err)
	}
	return nil
}
