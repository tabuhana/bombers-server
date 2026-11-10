-- +goose Up
-- NODE ASSETS — the same table the game store has had since `create_activities`,
-- for the store that was the oversight rather than the deliberate limit.
--
-- A node could not ship a byte of art: `GET /nodes/{id}/bundle` hands back an
-- opaque {manifest, files} bundle of TEXT, so a node that declared an icon image
-- had no way to deliver it and the desktop client's publish said so out loud
-- ("a node ships no assets, so they stay behind"). An app's `art` icon
-- (APPS_PLAN phase 2 step 1) therefore could not reach a user at all — only a
-- colour and a glyph could.
--
-- The BYTES live in object storage under nodes/<id>/<path>, exactly as a game's
-- live under activities/<id>/<path>, and are served through the same
-- authenticated pass-through — never a bucket or presigned URL. This table is
-- the manifest of what exists, so the installer can fetch what it needs in one
-- round trip. Dropping a node drops its asset rows; the bytes are the handler's
-- to remove.
--
-- `nodes.id` is TEXT and carries a namespace (`core:weather`), which is why this
-- references it rather than keying on anything derived.
CREATE TABLE node_assets (
    node_id      TEXT        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    path         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (node_id, path)
);

-- +goose Down
DROP TABLE node_assets;
