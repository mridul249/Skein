-- +goose Up

-- Four changes bundled into one migration and one sqlc regeneration, because
-- each alone would cost a full pass over both dialects and the generated code.
-- They are unrelated in purpose and deliberately kept separate in the file.

-- ---------------------------------------------------------------------------
-- 1. Per-user session epoch (known issue #18).
-- ---------------------------------------------------------------------------
--
-- RevokeAllUserSessions enumerates the sessions that exist at one instant and
-- therefore cannot bind a session INSERTed after the sweep: a refresh that has
-- claimed its parent but not yet inserted its successor produces a live
-- session that outlives the revocation. token_families does not cover this — a
-- family is one login, and a user has one family per device.
--
-- The fix is validity state a racing insert INHERITS rather than re-reads.
-- users.session_epoch is bumped by a revocation; every session records the
-- epoch it was born under; a session is valid only while its epoch matches its
-- user's current one.
--
-- Inheritance is the whole mechanism and it is why re-reading is wrong: a
-- successor that re-read the epoch at insert time would read the NEW value if
-- the revocation landed first, and be born valid — reproducing the identical
-- race one scope up. The successor must copy the epoch from the parent row it
-- claimed, so a revocation that happened in between leaves the successor
-- carrying a stale epoch and therefore dead.
ALTER TABLE users ADD COLUMN session_epoch BIGINT NOT NULL DEFAULT 1;

-- Existing sessions belong to the current epoch: a migration must not sign
-- everyone out.
ALTER TABLE sessions ADD COLUMN epoch BIGINT NOT NULL DEFAULT 1;

-- ---------------------------------------------------------------------------
-- 2. files.status admits the two reconciled damage states (known issue #42).
-- ---------------------------------------------------------------------------
--
-- Reconcile derives partially_missing and corrupted per run, but the CHECK
-- constraint admitted only pending/ready/failed, so the state could not be
-- persisted and the damaged badge died on every page load.
--
-- Neither new value is ever written by the upload path; both are written only
-- by a COMPLETE reconcile run. See reconciled_at below.
ALTER TABLE files DROP CONSTRAINT files_status_check;
ALTER TABLE files ADD CONSTRAINT files_status_check
    CHECK (status IN ('pending', 'ready', 'failed', 'partially_missing', 'corrupted'));

-- ---------------------------------------------------------------------------
-- 3. files.reconciled_at — when this row's damage state was last established.
-- ---------------------------------------------------------------------------
--
-- Two jobs. It lets the UI say how stale the evidence behind a badge is, and
-- it lets an INCOMPLETE reconcile run decline to record anything: a run with
-- any indeterminate shard check must not stamp this column, because a partial
-- result presented as complete is how a healthy file gets purged.
--
-- NULL means "never reconciled", which is distinct from "reconciled and
-- found healthy" — the latter carries a timestamp with status = 'ready'.
ALTER TABLE files ADD COLUMN reconciled_at TIMESTAMPTZ;

-- ---------------------------------------------------------------------------
-- 4. The instance's master key id (known issue #48).
-- ---------------------------------------------------------------------------
--
-- SKEIN_MASTER_KEY has no in-band validation at startup: a wrong key starts
-- the server fine and fails at the first download, three layers down, as a
-- decryption error. Recording the key id at first boot turns that into a
-- refusal at startup naming the actual cause.
--
-- A single-row table rather than a column on users: the key is a property of
-- the INSTANCE, not of any user, and every user's data is encrypted under the
-- same master key. The CHECK pins it to exactly one row so there is no
-- ambiguity about which record is authoritative.
--
-- The key id is a non-secret 4-byte HKDF output over the master key
-- (crypto/kdf.go, skein-key-id-v1) and already travels in the clear in every
-- ciphertext envelope, so storing it discloses nothing new.
CREATE TABLE instance_metadata (
    id         SMALLINT    PRIMARY KEY DEFAULT 1,
    key_id     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT instance_metadata_singleton CHECK (id = 1)
);

-- +goose Down

DROP TABLE IF EXISTS instance_metadata;

ALTER TABLE files DROP COLUMN reconciled_at;

-- Rolling back the constraint means rows carrying the new states would violate
-- it, so they are returned to 'ready'. The damage was never in the status
-- column — it is in the drives — and a re-run of reconcile re-derives it.
UPDATE files SET status = 'ready'
 WHERE status IN ('partially_missing', 'corrupted');

ALTER TABLE files DROP CONSTRAINT files_status_check;
ALTER TABLE files ADD CONSTRAINT files_status_check
    CHECK (status IN ('pending', 'ready', 'failed'));

ALTER TABLE sessions DROP COLUMN epoch;
ALTER TABLE users DROP COLUMN session_epoch;
