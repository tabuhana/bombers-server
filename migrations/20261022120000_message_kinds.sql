-- +goose Up
-- A message can be something other than text. Today a message is words and
-- nothing else, which leaves an invite with nowhere to be a message and a
-- picture with nowhere to live at all. One change serves both.
--
-- kind is a small CLOSED set, and the CHECK is the reason it is closed: a column
-- that can hold any word will, one day, hold a word nothing renders. A new kind
-- is a migration that widens this list, which is a decision made on purpose.
-- Every existing row takes the default and is therefore a text message; nothing
-- is backfilled and nothing is rewritten.
--
-- payload is what a non-text kind carries (an invite's room, a picture's media
-- id). It is NULL for text. The server does not read it beyond checking the shape
-- of the kind it is writing; the shape is checked in code, where it can say why.
-- A message that is deleted keeps its kind and loses its payload, the way it
-- loses its body: a tombstone says nothing.
--
-- NOTHING BUT THE SERVER WRITES A NON-TEXT KIND. POST /messages is text-only and
-- refuses a client that names another kind, because a client able to declare an
-- invite could put a room id into any message and an invite would be a claim in
-- a message again - the shortcut that was deleted. The server mints invite
-- messages itself, when it makes the invite.
--
-- Existing messages are not touched, so there is nothing to migrate.
ALTER TABLE messages
    ADD COLUMN kind    TEXT  NOT NULL DEFAULT 'text',
    ADD COLUMN payload JSONB;

ALTER TABLE messages
    ADD CONSTRAINT messages_kind_check CHECK (kind IN ('text', 'invite', 'image'));

-- +goose Down
-- The kinds and payloads go with the columns; the messages themselves stay, and
-- an invite message becomes an empty text message.
ALTER TABLE messages DROP CONSTRAINT messages_kind_check;

ALTER TABLE messages
    DROP COLUMN payload,
    DROP COLUMN kind;
