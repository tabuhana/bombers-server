-- +goose Up
-- Banning a user. A ban is a mark on the account rather than a deletion, so the
-- operator can reverse it and so the user's content stays consistent for
-- everyone else (their DMs don't vanish out of other people's conversations).
--
-- Enforcement is at the DOORS — login and token refresh — not on every request:
-- access tokens are stateless and short-lived (15m), so a ban takes hold within
-- one token lifetime without adding a database read to every authed call. At
-- friends-and-family scale that trade is the right one.
ALTER TABLE users ADD COLUMN banned_at  TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN ban_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN ban_reason;
ALTER TABLE users DROP COLUMN banned_at;
