-- +goose Up
-- The heart of sync: the server's copy of items a client has published. The
-- server is a versioned key-value store of published items, keyed by the client's
-- ULID, scoped to an account (see SERVER.md §Sync). It NEVER edits content — it
-- stores what clients push and serves it back on pull.
--
-- (owner_id, id) is the primary key: `id` is the client-generated ULID (the join
-- key that makes the same item recognizable across a user's devices and, later,
-- for sharing), scoped under its owner so two accounts can't collide.
--
-- `updated_at` is the CLIENT's timestamp and is authoritative for last-write-wins:
-- a push only overwrites the stored row when its updated_at is newer-or-equal.
-- `server_updated_at` is when the server last wrote the row, and drives
-- incremental "changed since T" pulls. `deleted` is a soft-delete tombstone so a
-- delete on one device propagates instead of the item resurrecting on another's
-- next pull. `type` is content-agnostic ('note' today; later mood/habit/event/…) —
-- the server doesn't interpret it. `content` is an opaque client-owned JSON blob.
CREATE TABLE published_items (
    owner_id          TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id                TEXT        NOT NULL,
    type              TEXT        NOT NULL,
    content           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at        TIMESTAMPTZ NOT NULL,
    deleted           BOOLEAN     NOT NULL DEFAULT FALSE,
    server_updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id, id),
    CONSTRAINT published_items_type_nonempty CHECK (type <> '')
);

-- Incremental pull is "my items where server_updated_at > since", so index it.
CREATE INDEX published_items_owner_changed_idx ON published_items (owner_id, server_updated_at);

-- Per-account sync bookkeeping: when the account last completed a push. Lets any
-- device show "last synced X ago" consistently (server-recorded, per SERVER.md).
CREATE TABLE sync_state (
    owner_id       TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    last_synced_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id)
);

-- +goose Down
DROP TABLE sync_state;
DROP TABLE published_items;
