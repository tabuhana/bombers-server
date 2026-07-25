-- +goose Up
-- The PACK STORE: downloadable look-and-feel bundles for the client — a theme
-- (colors / fonts / roundness), a set of sounds, an optional wallpaper, or any
-- mix. Same operator-curated model as the node and activity stores: an opaque
-- bundle the server never interprets, published from the console, no HTTP
-- publish. A "theme with its own sounds" is just a pack that carries both.
--
-- `bundle` is the pack's manifest as JSON — id/name/author + the theme's CSS
-- variables. The server reads a few descriptive keys for the catalogue and
-- passes the rest through untouched. Sounds and the wallpaper are ASSETS whose
-- bytes live in object storage under packs/<id>/<path>.
CREATE TABLE packs (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    version    TEXT        NOT NULL DEFAULT '',
    bundle     JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE pack_assets (
    pack_id      TEXT        NOT NULL REFERENCES packs(id) ON DELETE CASCADE,
    path         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pack_id, path)
);

-- +goose Down
DROP TABLE pack_assets;
DROP TABLE packs;
