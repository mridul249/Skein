-- +goose Up
-- citext gives case-insensitive email uniqueness in the database rather than
-- by remembering to lower() at every call site.
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
DROP EXTENSION IF EXISTS citext;
