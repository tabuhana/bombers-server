-- +goose Up
-- The ACTIVITY STORE: games this server publishes, in the same shape as the node
-- store (an opaque {manifest, files} bundle the server never interprets). A
-- client browses the catalogue and installs; the operator curates via the
-- console, exactly as with nodes — there is no HTTP publish.
--
-- Activities are NOT nodes: they get their own screen, their own registry, and —
-- the reason this table isn't just `nodes` with a flag — assets.
CREATE TABLE activities (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    version    TEXT        NOT NULL DEFAULT '',
    bundle     JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One row per asset file a game ships (sprites, audio, later models/textures).
-- The BYTES live in object storage under activities/<id>/<path>; this table is
-- the manifest of what exists, so the catalogue and the installer can describe a
-- download without touching the store. Dropping an activity drops its assets.
CREATE TABLE activity_assets (
    activity_id  TEXT        NOT NULL REFERENCES activities(id) ON DELETE CASCADE,
    path         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (activity_id, path)
);

-- +goose Down
DROP TABLE activity_assets;
DROP TABLE activities;
