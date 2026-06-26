-- +goose Up
-- The node store: published node bundles available for clients to install.
-- Curated / owner-published in v1. Metadata and the node.js ESM bundle bytes live
-- together; a client lists the catalog (metadata only), then downloads a bundle and
-- verifies its hash before running it. `id` is the node's stable id (e.g.
-- 'acme:cpu-meter'); `permissions` are the native capabilities it declares.
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

-- +goose Down
DROP TABLE nodes;
