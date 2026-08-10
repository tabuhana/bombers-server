-- +goose Up
-- Who tried to sign in and had no account.
--
-- The only way into Bombers is the website terminal, and the only way to the
-- installer is having finished that. So a Discord identity presenting itself at
-- a login with no account behind it did not follow the one path that exists.
-- The server already learns exactly who they are during the OAuth exchange, and
-- until now threw it away — which meant a leaked installer was invisible.
--
-- One row per identity rather than one per attempt: the question is "who is
-- doing this", not "how many times on Tuesday", and a counter answers it
-- without a table that grows forever and needs pruning.
CREATE TABLE signin_attempts (
    -- Discord's user id. Not a foreign key — the whole point is that no account
    -- exists for it.
    discord_id       TEXT PRIMARY KEY,

    -- Their handle at the time, so the operator reads a name instead of a
    -- number. Refreshed on each attempt, since handles change.
    discord_username TEXT NOT NULL DEFAULT '',

    attempts         INTEGER NOT NULL DEFAULT 1,
    first_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Set to refuse this identity outright, ahead of the allowlist. A row can
    -- exist with zero attempts purely to block somebody in advance, and a
    -- blocked identity stays refused even if it's later added to the allowlist
    -- by mistake — being on two lists shouldn't come down to which one is
    -- checked first.
    blocked_at       TIMESTAMPTZ,

    -- Why, in the operator's words.
    block_reason     TEXT
);

-- The console lists recent attempts, so that ordering gets an index rather than
-- a sort over the table.
CREATE INDEX idx_signin_attempts_last ON signin_attempts (last_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_signin_attempts_last;
DROP TABLE IF EXISTS signin_attempts;
