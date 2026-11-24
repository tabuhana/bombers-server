-- +goose Up
-- LOST-ANSWER GRACE for refresh-token rotation.
--
-- Until now `used_at` meant "rotated or revoked", and presenting any token with
-- it set was treated as theft and killed every refresh token the user had. If
-- the server rotated but its reply never reached the app (wifi drop, restart),
-- the app retried with the token it still held and got the whole account signed
-- out everywhere.
--
-- The columns now mean exactly one thing each:
--   used_at     = the token was ROTATED: spent by its owner. Nothing else sets it.
--   revoked_at  = the token was KILLED: by the theft kill, or cancelled because
--                 the reply that carried it was lost.
--   replaced_by = the jti of the token issued in this one's place, so a retry
--                 can find out whether the replacement was ever used.
--
-- Existing rows: tokens killed before this migration have used_at set and no
-- replaced_by, so presenting one is still theft. Nothing changes for them.
ALTER TABLE refresh_tokens
    ADD COLUMN replaced_by TEXT REFERENCES refresh_tokens(id) ON DELETE SET NULL,
    ADD COLUMN revoked_at  TIMESTAMPTZ;

-- +goose Down
ALTER TABLE refresh_tokens
    DROP COLUMN replaced_by,
    DROP COLUMN revoked_at;
