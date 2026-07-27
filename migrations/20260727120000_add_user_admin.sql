-- +goose Up
-- The admin flag. Until now "operator" meant "whoever holds the server console"
-- — privilege by physical access, with no way to authorize an HTTP call. This
-- adds the role so operator actions (publishing to the node store, and later the
-- rest of the console surface) can be driven from the client over the API.
--
-- Default FALSE: every existing account stays a normal user. The first admin is
-- granted deliberately, either by the console's `admin <user>` command or by the
-- ADMIN_USERNAME env var applied at boot.
ALTER TABLE users ADD COLUMN is_admin BOOLEAN NOT NULL DEFAULT FALSE;

-- Admin checks run on every admin-gated request, and admins are a handful of
-- rows at most — a partial index keeps that lookup off a sequential scan
-- without indexing the overwhelming majority of FALSE rows.
CREATE INDEX idx_users_is_admin ON users (id) WHERE is_admin;

-- +goose Down
DROP INDEX IF EXISTS idx_users_is_admin;
ALTER TABLE users DROP COLUMN is_admin;
