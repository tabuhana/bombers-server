-- +goose Up
-- Rework the node store for the SDK era: the old single-file node.js catalog
-- (bytea bundle, client-declared permissions, any-user POST /nodes publish) is
-- replaced by OPERATOR-published SDK bundles — the same opaque {manifest,
-- files} JSONB shape node_transfers carries. id/name/version are denormalized
-- out of the manifest so catalog listings never touch the heavy bundle.
-- Publishing happens via the server console only (no HTTP publish endpoint).
-- Nothing was ever published in the old format, so drop + recreate.
DROP TABLE nodes;
CREATE TABLE nodes (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    version    TEXT        NOT NULL DEFAULT '',
    bundle     JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
-- Restore the old single-file shape (empty — the store carried no data).
DROP TABLE nodes;
CREATE TABLE nodes (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    version     TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL DEFAULT '',
    width       INTEGER NOT NULL,
    height      INTEGER NOT NULL,
    can_rotate  BOOLEAN NOT NULL DEFAULT FALSE,
    permissions TEXT[] NOT NULL DEFAULT '{}',
    hash        TEXT NOT NULL,
    bundle      BYTEA NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
