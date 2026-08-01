-- +goose Up
-- Long-lived, scoped, revocable credentials — the thing that makes a consumer
-- other than the desktop app possible.
--
-- Until now the only credentials were a 15-minute access token and a rotating
-- refresh token designed for one interactive client. Nothing a script, a
-- mini-client, or an agent could hold, and nothing you could hand out narrowly
-- or take back. This is that: you name a token, choose what it may do, and
-- revoke it without touching your password or your other sessions.
CREATE TABLE api_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- What it's for, in the owner's words ("my note agent"). The only way to
    -- tell two tokens apart when deciding which to revoke.
    name        TEXT NOT NULL,

    -- SHA-256 of the secret, never the secret. The plaintext is shown once at
    -- creation and is unrecoverable afterwards — a leaked database gives an
    -- attacker hashes, and a lost token is replaced rather than looked up.
    token_hash  TEXT NOT NULL UNIQUE,

    -- What it may do. An empty array can do nothing, which makes the failure
    -- mode of a bug in scope handling "refuses everything" rather than
    -- "allows everything".
    scopes      TEXT[] NOT NULL DEFAULT '{}',

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Optional lifetime. NULL = until revoked.
    expires_at  TIMESTAMPTZ,
    -- Set instead of deleting the row, so a token you handed to something that
    -- misbehaved leaves evidence it existed.
    revoked_at  TIMESTAMPTZ,
    -- Roughly when it was last used, for spotting one you forgot about.
    last_used_at TIMESTAMPTZ
);

-- Every authenticated request presenting an API token looks it up by hash, so
-- that path must be an index seek. UNIQUE on token_hash already provides one.
CREATE INDEX idx_api_tokens_user ON api_tokens (user_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_api_tokens_user;
DROP TABLE IF EXISTS api_tokens;
