-- +goose Up

-- Where this account's shards live at the provider.
--
-- Shards used to land at Drive root, where they look like junk with names
-- nobody recognises. People delete junk, and deleting a shard destroys the
-- file it belongs to — silently, because nothing tells Skein until someone
-- tries to read it. A named folder with a README is the cheapest possible
-- defence against that.
--
-- Nullable: an account connected before this migration has no folder yet, and
-- one is created lazily on its next upload. NULL means "not yet established",
-- never "root".
ALTER TABLE connected_accounts ADD COLUMN app_folder_id TEXT;

-- +goose Down
ALTER TABLE connected_accounts DROP COLUMN app_folder_id;
