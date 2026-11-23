-- +goose Up
-- THE ENGINE STORE — the fifth installable kind.
--
-- An engine is the code a game runs on: the table, the wire, the fixed step,
-- smooth online motion, controller mapping. It never owns a game's look; a game
-- keeps its own art and rules and leans on an engine for the machinery
-- underneath. Engines ship from this server the way apps and games do, so they
-- need a store of their own.
--
-- A SEPARATE TABLE, NOT A `kind` COLUMN ON `apps`. A new kind is its own table
-- and route family, for the reason `create_apps` gave: an id in one store must
-- never be able to mean two things, and a kind column would make the primary key
-- a lie the moment an app and an engine shared a name. Engines and apps stay
-- independent, each can be unpublished without touching the other.
--
-- A GAME NAMES THE ENGINE IT NEEDS IN ITS OWN MANIFEST (`requiresEngine`), and
-- the activities listing reads that out of the game's stored bundle. Nothing
-- here points at games, so there is no foreign key either way and nothing to
-- migrate when a game starts or stops needing an engine.
--
-- The shape is `apps` character for character: an opaque {manifest, files}
-- JSONB bundle the server never interprets, with id/name/version denormalized
-- out of it so a catalogue listing never touches the heavy column.
CREATE TABLE engines (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    version    TEXT        NOT NULL DEFAULT '',
    bundle     JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- THE ASSET MANIFEST, as `create_apps` made it for the store before this one.
-- The BYTES live in object storage under engines/<id>/<path> and are served
-- through the same authenticated pass-through — never a bucket or presigned
-- URL. This table is what exists, so an installer fetches what it needs in one
-- round trip. Dropping an engine drops its asset rows; the bytes are the
-- handler's to remove.
--
-- `engines.id` is TEXT and may carry a namespace, which is why this references
-- it rather than keying on anything derived.
CREATE TABLE engine_assets (
    engine_id    TEXT        NOT NULL REFERENCES engines(id) ON DELETE CASCADE,
    path         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (engine_id, path)
);

-- +goose Down
DROP TABLE engine_assets;
DROP TABLE engines;
