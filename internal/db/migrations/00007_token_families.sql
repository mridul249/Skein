-- +goose Up

-- Known issue #11. A refresh-token family was only ever revoked by enumerating
-- the sessions that existed at that instant:
--
--     UPDATE sessions SET revoked_at = now()
--      WHERE family_id = $1 AND revoked_at IS NULL
--
-- A concurrent refresh claims the presented token, then inserts its successor a
-- round trip later. If the revocation lands between those two writes it sweeps
-- only the original, and the successor is inserted afterwards with revoked_at
-- NULL. Rules.md §2.8 requires reuse to revoke the *entire* family; this
-- revoked the family as enumerated, which is not the same thing. The defence
-- reported success — Warn log, refresh.reuse_detected audit row — while a live
-- chain survived.
--
-- The rule this table exists to satisfy: validity must derive from state the
-- racing insert cannot avoid inheriting. family_id is copied from the claimed
-- parent on every rotation, so a successor cannot escape into a different
-- family, and a marker on the family therefore binds rows that do not exist yet.
CREATE TABLE token_families (
    id         UUID PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set once, by the first revocation. Session validity is now two-part:
    -- the session is not revoked AND its family is not revoked.
    revoked_at TIMESTAMPTZ
);

CREATE INDEX token_families_user_idx ON token_families (user_id);

-- Backfill before the constraint, or it fails on every existing family.
--
-- This is not optional in a second sense: validity is an AND, so a session
-- whose family row is missing would evaluate as invalid. A skipped backfill
-- would silently sign out every existing session.
--
-- GROUP BY family_id, user_id rather than family_id alone is deliberate. A
-- family descends from one login and cannot span users; if one ever did, this
-- fails loudly on the primary key instead of silently picking a user.
INSERT INTO token_families (id, user_id, created_at)
SELECT family_id, user_id, MIN(created_at)
  FROM sessions
 GROUP BY family_id, user_id;

-- revoked_at is deliberately NOT backfilled. Already-revoked sessions stay
-- revoked through their own column, so leaving historical families NULL cannot
-- resurrect anything.
ALTER TABLE sessions
    ADD CONSTRAINT sessions_family_id_fkey
    FOREIGN KEY (family_id) REFERENCES token_families (id);

-- +goose Down
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_family_id_fkey;
DROP TABLE IF EXISTS token_families;
