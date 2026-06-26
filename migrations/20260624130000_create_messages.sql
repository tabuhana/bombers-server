-- +goose Up
-- Direct messages: text DMs between two users, persisted INDEFINITELY (DMs are
-- not ephemeral; rooms are — see SERVER.md). One row per message, keyed by ULID.
-- Image/file attachments are deferred to the later S3 phase, so v1 stores text
-- only. Real-time delivery (WebSocket) is a later layer; this table is the
-- durable source of truth that any device pulls history from.
CREATE TABLE messages (
    id           TEXT        NOT NULL,
    sender_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recipient_id TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id),
    CONSTRAINT messages_no_self CHECK (sender_id <> recipient_id)
);

-- A conversation is all messages between an unordered pair, read newest-first
-- (history loads the recent tail, then renders ascending). The pair is matched
-- in either direction, so both orderings are indexed. created_at DESC supports
-- the "most recent N" tail query and keyset pagination by (created_at, id).
CREATE INDEX messages_sender_recipient_idx ON messages (sender_id, recipient_id, created_at DESC);
CREATE INDEX messages_recipient_sender_idx ON messages (recipient_id, sender_id, created_at DESC);

-- +goose Down
DROP TABLE messages;
