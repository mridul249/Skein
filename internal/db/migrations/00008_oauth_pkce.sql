-- +goose Up

-- Desktop OAuth (Phase7 Task 4.4) uses PKCE: a verifier generated per
-- connect attempt, sent as a S256 challenge in the auth URL, and presented
-- again at exchange. The verifier must come back from server-side state at
-- the callback, never from the callback's own query string — trusting a
-- caller-supplied verifier would let anyone complete anyone else's pending
-- exchange. NULL for the web flow, which does not use PKCE.
ALTER TABLE oauth_states ADD COLUMN pkce_verifier TEXT;

-- +goose Down
ALTER TABLE oauth_states DROP COLUMN pkce_verifier;
