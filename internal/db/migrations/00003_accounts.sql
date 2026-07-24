-- +goose Up

-- One row per connected provider account.
--
-- Tokens are stored as ciphertext produced by internal/crypto: a versioned
-- envelope carrying its own key id, never a raw token and never a token
-- encrypted under a key derived by hashing a passphrase.
CREATE TABLE connected_accounts (
    id                  UUID PRIMARY KEY,
    user_id             UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind                TEXT        NOT NULL,
    -- The provider's own stable identifier for the account. Linking is keyed
    -- on this, never on the email address: an address can change hands, and
    -- linking by email alone is how account takeover happens.
    provider_account_id TEXT        NOT NULL,
    email               CITEXT      NOT NULL,
    display_name        TEXT        NOT NULL DEFAULT '',
    access_token_enc    BYTEA       NOT NULL,
    refresh_token_enc   BYTEA,
    token_expires_at    TIMESTAMPTZ,
    status              TEXT        NOT NULL DEFAULT 'active',
    last_error          TEXT        NOT NULL DEFAULT '',
    -- Position in the account colour ramp and the routing order. Assigned on
    -- connect and stable thereafter, because the colour is the account's
    -- identity across the whole interface.
    ordinal             INT         NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT connected_accounts_kind_check
        CHECK (kind IN ('gdrive', 'local', 's3')),
    CONSTRAINT connected_accounts_status_check
        CHECK (status IN ('active', 'needs_reauth', 'disabled')),
    CONSTRAINT connected_accounts_unique_provider
        UNIQUE (user_id, kind, provider_account_id)
);

CREATE INDEX connected_accounts_user_id_idx ON connected_accounts (user_id);

-- Capacity, tracked separately from the account so the quota ticker updates a
-- narrow row rather than contending with token refreshes.
--
-- reserved_bytes is the in-flight commitment made by the atomic reservation in
-- Architecture.md §5. Free space is total - used - reserved, so two concurrent
-- uploads cannot both be told the same bytes are available.
CREATE TABLE storage_accounts (
    connected_account_id UUID PRIMARY KEY
        REFERENCES connected_accounts (id) ON DELETE CASCADE,
    total_bytes    BIGINT      NOT NULL DEFAULT 0,
    used_bytes     BIGINT      NOT NULL DEFAULT 0,
    reserved_bytes BIGINT      NOT NULL DEFAULT 0,
    last_synced_at TIMESTAMPTZ,
    last_error     TEXT        NOT NULL DEFAULT '',

    CONSTRAINT storage_accounts_non_negative
        CHECK (total_bytes >= 0 AND used_bytes >= 0 AND reserved_bytes >= 0)
);

-- Pending OAuth authorisations.
--
-- Only the SHA-256 of the state parameter is stored, for the same reason only
-- token hashes are: a database dump must not hand over anything replayable.
-- Rows are single use and short lived; consuming one deletes it.
CREATE TABLE oauth_states (
    state_hash   BYTEA PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL,
    redirect_to  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX oauth_states_expires_at_idx ON oauth_states (expires_at);

-- +goose Down
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS storage_accounts;
DROP TABLE IF EXISTS connected_accounts;
