-- +goose Up
-- Settings an operator changes while the server is running.
--
-- Configuration used to be env-only, which is right for anything a deploy
-- decides: a port, a database URL, where media lives. It's wrong for anything an
-- OPERATOR decides, because the only way to change one was to edit a file and
-- restart — and on a wizard-configured install there was no file at all, since
-- config.json can only carry what the wizard knows how to ask.
--
-- These live in the database instead, so the console can change them and the
-- next request sees it. Environment variables still WIN over this table: a
-- container deploy passing DISCORD_CLIENT_ID keeps behaving exactly as it did,
-- and an operator locked out of the console can always override from outside.
--
-- Deliberately a key/value table rather than columns. Every setting that gets
-- added otherwise costs a migration, and these are strings an operator types.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS settings;
