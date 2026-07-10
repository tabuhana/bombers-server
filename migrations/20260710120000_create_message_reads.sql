-- +goose Up
-- Per-(user, peer) last-read marker for direct messages: the server-authoritative
-- read state that lets any device answer "what's unread?" in one request and clear
-- a conversation for ALL of a user's devices at once (read on the phone → cleared on
-- the desktop). One row per (reader, peer) pair; last_read_at advances monotonically
-- (never rewinds — see markRead's GREATEST upsert), and a received message is unread
-- when its created_at is newer than the reader's marker, or no marker exists yet.
-- FKs cascade on user delete, matching the messages table's sender/recipient FKs.
CREATE TABLE message_reads (
    user_id      TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    peer_id      TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    last_read_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, peer_id)
);

-- +goose Down
DROP TABLE message_reads;
