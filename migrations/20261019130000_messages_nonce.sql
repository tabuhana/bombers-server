-- +goose Up
-- A send can be retried safely. The client draws a message the moment it is sent
-- and, if the request fails, offers a retry that reuses the same nonce. A failure
-- is not always a message that never arrived: the request can reach the server
-- and only the RESPONSE be lost, and then the retry is a second copy of a message
-- the recipient already has. The nonce is the sender's own name for one send, so
-- storing it lets the server answer a repeat with the message it already made.
--
-- The key is (sender_id, recipient_id, nonce), not the nonce alone: a nonce is
-- invented by a client, and two people's clients may invent the same one. One
-- person's nonce must never be able to swallow, or reveal, another person's
-- message. The recipient is part of the key because a retry always has the same
-- recipient as the send it repeats; without it, the same nonce sent to a second
-- person would be answered with the first person's message and never delivered.
--
-- The index is PARTIAL because the nonce is optional and almost every row has
-- none; a full unique index over a column that is NULL nearly everywhere would be
-- weight carried for nothing. (NULLs never collide in a unique index anyway, so
-- two sends with no nonce stay two messages.) The nonce is write-only: no
-- SELECT list reads it back.
ALTER TABLE messages ADD COLUMN nonce TEXT;

CREATE UNIQUE INDEX messages_sender_recipient_nonce_idx ON messages (sender_id, recipient_id, nonce) WHERE nonce IS NOT NULL;

-- +goose Down
-- The nonces go with the column; the messages themselves are untouched. After a
-- rollback a retried send is a second message again.
DROP INDEX messages_sender_recipient_nonce_idx;

ALTER TABLE messages DROP COLUMN nonce;
