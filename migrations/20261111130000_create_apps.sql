-- +goose Up
-- THE APP STORE — the fourth installable kind, and the one the others are
-- being retired into.
--
-- An app is what a node is becoming. The two are different things wearing the
-- same bundle envelope: a NODE is one face drawn against a DEK cell, with a
-- modal and a flyout hung off it, and its manifest declares the footprint it is
-- drawn at; an APP is an icon on the deck, a full screen you go into, and
-- optionally a widget between them, with no footprint at all and a palette of
-- its own (25 colours, all of them, or the client refuses the manifest).
--
-- A SEPARATE TABLE, NOT A `kind` COLUMN ON `nodes`. The two stores are live at
-- once on purpose — what a user has installed today arrived as a node, and the
-- old one must keep serving them right up until it is unpublished by hand — so
-- an id may exist in both and must mean two different things. A kind column
-- would make the primary key a lie the moment that happened, and `/nodes` is
-- deleted when nothing needs it rather than migrated. When that day comes this
-- table stays and that one is dropped.
--
-- The shape is `nodes` after `rework_nodes_store`, deliberately character for
-- character: an opaque {manifest, files} JSONB bundle the server never
-- interprets, with id/name/version denormalized out of it so a catalogue
-- listing never touches the heavy column.
CREATE TABLE apps (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    version    TEXT        NOT NULL DEFAULT '',
    bundle     JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- THE ASSET MANIFEST, as `create_node_assets` made it for the store before this
-- one. The BYTES live in object storage under apps/<id>/<path> and are served
-- through the same authenticated pass-through — never a bucket or presigned
-- URL. This table is what exists, so an installer fetches what it needs in one
-- round trip. Dropping an app drops its asset rows; the bytes are the handler's
-- to remove.
--
-- `apps.id` is TEXT and may carry a namespace, which is why this references it
-- rather than keying on anything derived.
CREATE TABLE app_assets (
    app_id       TEXT        NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    path         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (app_id, path)
);

-- +goose Down
DROP TABLE app_assets;
DROP TABLE apps;
