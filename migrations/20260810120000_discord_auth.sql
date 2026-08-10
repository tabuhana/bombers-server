-- +goose Up
-- Discord becomes the way people sign in.
--
-- Until now an account was a username and a password hash, which meant the
-- server had to own a password reset flow it never got, and reset needs email,
-- and email needs delivery, deliverability and somebody to answer "I never got
-- the message". Discord removes all of it: the first sign-in creates the
-- account, and losing access is Discord's problem to solve, which they already
-- have.
--
-- Password login is NOT dropped here on purpose. The desktop client still uses
-- it, and removing it before the client can sign in with Discord would lock the
-- owner out of his own server. It goes when the client side lands.

-- A Discord account this Bombers account belongs to. NULL for every account
-- that predates this — including the owner's, which gets linked rather than
-- replaced, because it already owns synced content.
ALTER TABLE users ADD COLUMN discord_id TEXT UNIQUE;

-- Their handle and avatar hash, refreshed on every sign-in. Stored rather than
-- fetched because they're shown constantly and Discord is not on the critical
-- path of rendering a friends list.
--
-- The avatar is a HASH, not a URL, and not the image: Discord's CDN serves it,
-- their picture changes when they change it, and a copy in our own storage is
-- one that goes quietly stale. A Bombers-specific picture uploaded to
-- user_media still takes precedence.
ALTER TABLE users ADD COLUMN discord_username TEXT;
ALTER TABLE users ADD COLUMN discord_avatar   TEXT;

-- What they've linked to Discord — Steam, GitHub, League — as the API returned
-- it. Opaque JSONB: the server stores the snapshot and the client decides what
-- to show, so adding a service Discord starts supporting needs no migration.
-- Refreshed at each sign-in, which is the only time we hold a token.
ALTER TABLE users ADD COLUMN discord_connections JSONB;

-- An account created through Discord has no password and never will, so the
-- column stops being mandatory. Existing rows keep theirs and keep working.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

-- Who is allowed to have an account.
--
-- The server defaults to refusing anyone not on this list. That's not a
-- security boundary — it's a pace control. The owner is onboarding friends a
-- few at a time and doesn't want the door open while he does it; SIGNUP_MODE
-- opens it later without touching this table.
CREATE TABLE signup_allowlist (
    -- Discord's user id: a number that never changes. Usernames do change, and
    -- an allowlist keyed on one would silently stop matching the person it was
    -- meant for.
    discord_id TEXT PRIMARY KEY,

    -- Whatever the operator typed to remember who this is — a name, a handle.
    -- The id alone is unreadable, and an allowlist you can't read is one you
    -- can't audit.
    note       TEXT NOT NULL DEFAULT '',

    added_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS signup_allowlist;
-- Restoring NOT NULL would fail against any Discord-created account, so this
-- deliberately leaves password_hash nullable. Rolling back the schema shouldn't
-- destroy rows to satisfy a constraint.
ALTER TABLE users DROP COLUMN IF EXISTS discord_connections;
ALTER TABLE users DROP COLUMN IF EXISTS discord_avatar;
ALTER TABLE users DROP COLUMN IF EXISTS discord_username;
ALTER TABLE users DROP COLUMN IF EXISTS discord_id;
