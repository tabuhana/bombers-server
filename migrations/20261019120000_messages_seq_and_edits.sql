-- +goose Up
-- Messages become editable and deletable, and a client that was offline can ask
-- "what changed since I last looked?" instead of re-reading everything.
--
-- seq is the CATCH-UP cursor and nothing else. It comes from one global sequence,
-- is assigned on insert, and is assigned AGAIN on every edit and delete, so a
-- change always sorts after the cursor a client already holds. That is exactly
-- why it can never order a conversation: an edited message would jump to the
-- bottom. A conversation is ordered by (created_at, id); the ULID id is assigned
-- once and never changes, so history paginates on that.
--
-- A delete is a tombstone, not a removal: deleted_at is set, body is cleared in
-- the database (the words are deleted, not hidden), and the row stays so the
-- catch-up feed can tell a cached client that the message is gone.
CREATE SEQUENCE messages_seq;

ALTER TABLE messages
    ADD COLUMN seq        BIGINT,
    ADD COLUMN edited_at  TIMESTAMPTZ,
    ADD COLUMN deleted_at TIMESTAMPTZ;

-- Backfill in the order the messages were actually sent, so the first catch-up
-- hands a client its history in order. A volatile DEFAULT would number the
-- existing rows in whatever order the table rewrite happened to visit them.
-- row_number() over (created_at, id) is the order; nextval() inside an UPDATE
-- gives no such guarantee, so the sequence is then moved to the highest number
-- handed out. With no rows it is left to hand out 1 first.
UPDATE messages m
SET seq = ordered.n
FROM (
    SELECT id, row_number() OVER (ORDER BY created_at, id) AS n
    FROM messages
) ordered
WHERE ordered.id = m.id;

SELECT setval('messages_seq', COALESCE(MAX(seq), 1), COUNT(*) > 0) FROM messages;

ALTER TABLE messages
    ALTER COLUMN seq SET NOT NULL,
    ALTER COLUMN seq SET DEFAULT nextval('messages_seq');

CREATE UNIQUE INDEX messages_seq_idx ON messages (seq);
-- The catch-up asks "everything involving me, newer than this cursor", which is
-- two indexed reads unioned — one for what I sent, one for what I received.
CREATE INDEX messages_recipient_seq_idx ON messages (recipient_id, seq);
CREATE INDEX messages_sender_seq_idx    ON messages (sender_id, seq);

-- +goose Down
-- Tombstones are left in place: they hold no words, and a migration that deletes
-- rows is not one anybody can roll forward from. After a rollback they read as
-- empty messages.
DROP INDEX messages_sender_seq_idx;
DROP INDEX messages_recipient_seq_idx;
DROP INDEX messages_seq_idx;

ALTER TABLE messages
    DROP COLUMN deleted_at,
    DROP COLUMN edited_at,
    DROP COLUMN seq;

DROP SEQUENCE messages_seq;
