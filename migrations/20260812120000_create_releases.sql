-- +goose Up
-- APP RELEASES: the desktop client's own installers.
--
-- The fourth store on this server and the smallest. A node, a pack and a game
-- are each an opaque bundle plus a spread of assets; a release is ONE file and a
-- signature. What makes it different is who reads it — not a person browsing a
-- catalogue, but the updater inside an app that is about to overwrite itself.
--
-- That is why `signature` is NOT NULL with no default. Every installed copy
-- carries the public half of the operator's signing key and refuses to install
-- anything it can't verify, so a row without a signature is a release nobody
-- can take. Better to make it impossible to record one.
--
-- The installer's BYTES live in object storage under releases/<version>/<file>,
-- like a pack's sounds — this table is the manifest the updater reads.
--
-- `version` is the primary key rather than (platform, version) because the
-- client is Windows-only and permanently so. `platform` is still a column so
-- the update manifest is built from data rather than a string hardcoded in
-- three places; a second platform would be a migration, which is the honest
-- cost of a decision that hasn't been made.
CREATE TABLE releases (
    version      TEXT        PRIMARY KEY,
    platform     TEXT        NOT NULL DEFAULT 'windows-x86_64',
    notes        TEXT        NOT NULL DEFAULT '',
    signature    TEXT        NOT NULL,
    artifact     TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL DEFAULT 0,
    -- What "latest" orders by, and it moves on a republish. The operator's
    -- most recent act wins, which is what lets a bad build be rolled back by
    -- publishing the previous version again.
    published_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE releases;
