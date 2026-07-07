-- +goose Up
-- Friend node-sharing (the clone model): a lightweight inbox of node bundles
-- sent friend-to-friend. `bundle` is the node's {manifest, files} source as an
-- opaque JSONB blob — the server never inspects it (same dumb-blob rule as
-- sync). A transfer is a ONE-WAY copy: the recipient clones it into a project
-- they own, then dismisses (deletes) the row. No live link, no ownership, and
-- separate from the public node store (`nodes`).
CREATE TABLE node_transfers (
    id           TEXT        PRIMARY KEY,
    sender_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recipient_id TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bundle       JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The inbox read path: "transfers where I'm the recipient", newest first.
CREATE INDEX node_transfers_recipient_idx ON node_transfers(recipient_id, created_at DESC);

-- +goose Down
DROP TABLE node_transfers;
