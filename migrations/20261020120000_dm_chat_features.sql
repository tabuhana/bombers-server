-- +goose Up
-- A message can answer another, be pinned, carry reactions and be searched. This
-- is the schema for all of it in one place; the code that uses each part lands
-- separately.
--
-- Everything here is nullable, generated or a new table, so there is NO backfill:
-- an existing message is simply not a reply, not pinned and not reacted to.
--
-- Replies. reply_to_id points at the message being answered. It is SET NULL, not
-- CASCADE, because a reply must outlive the message it answers. (A deleted message
-- is a tombstone and keeps its row, so in practice the reply's preview reports it
-- deleted; SET NULL only matters if a row is ever removed outright.) The preview is
-- resolved when the reply is read and never stored, so an edit to the original
-- shows through. Nothing may UPDATE reply_to_id: what a message answers is fixed
-- when it is sent. The partial index is for the foreign key itself: without it every
-- message removed outright would scan the whole table to find its replies.
--
-- Pins. pinned_at and pinned_by are set and cleared together. A pin is a property
-- of the conversation, so either person may set or clear it; pinned_by records who
-- did, and is not returned to clients, since in a two-person conversation it says
-- nothing they need. Pinning draws a fresh messages.seq like every other change, so
-- a client that was offline learns of it through the catch-up feed. Two reads use
-- messages_pinned_idx: the list of a conversation's pins, newest first, and the
-- count that enforces the cap of 50. The index holds only pinned rows, so both read
-- the pins on the whole server rather than any conversation's history; a pin is
-- per PAIR, so a (recipient_id, sender_id, pinned_at) index would be the wrong
-- shape for a query that matches the pair in either direction.
--
-- Reactions. One row per (message, person, emoji): adding the same one twice is
-- the same row, which is what makes a repeat a no-op. CASCADE here is right, since
-- a reaction has no meaning without its message or its author. The primary key's
-- first column is message_id, so reading a message's reactions is already indexed.
-- Reactions are unicode emoji only; the shape rule that keeps the column from
-- becoming a second message body lives in code, where it can say why.
--
-- Search. body_tsv is GENERATED, so it can never drift from body: an edit updates
-- it with no trigger and nothing to remember, and a tombstone (body = '') has an
-- empty vector and can never match. The two-argument to_tsvector('english', ...) is
-- required: a generated column must be IMMUTABLE, and the one-argument form depends
-- on a setting and is rejected.
ALTER TABLE messages
    ADD COLUMN reply_to_id TEXT        REFERENCES messages(id) ON DELETE SET NULL,
    ADD COLUMN pinned_at   TIMESTAMPTZ,
    ADD COLUMN pinned_by   TEXT        REFERENCES users(id)    ON DELETE SET NULL;

CREATE INDEX messages_reply_to_idx ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;
CREATE INDEX messages_pinned_idx   ON messages (pinned_at DESC) WHERE pinned_at IS NOT NULL;

CREATE TABLE message_reactions (
    message_id TEXT        NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id    TEXT        NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    emoji      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (message_id, user_id, emoji)
);

-- The primary key already serves "this message's reactions", but its leading
-- column is message_id, so nothing indexes user_id on its own and deleting a
-- user would scan the table to find their reactions. Same reason reply_to_id is
-- indexed above.
CREATE INDEX message_reactions_user_idx ON message_reactions (user_id);

ALTER TABLE messages
    ADD COLUMN body_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', body)) STORED;

CREATE INDEX messages_body_tsv_idx ON messages USING GIN (body_tsv);

-- +goose Down
-- Reactions, pins and reply links are lost with the columns and the table; the
-- messages themselves are untouched. After a rollback every message reads as a
-- plain one again.
DROP INDEX messages_body_tsv_idx;

ALTER TABLE messages DROP COLUMN body_tsv;

DROP TABLE message_reactions;  -- its indexes go with it

DROP INDEX messages_pinned_idx;
DROP INDEX messages_reply_to_idx;

ALTER TABLE messages
    DROP COLUMN pinned_by,
    DROP COLUMN pinned_at,
    DROP COLUMN reply_to_id;
