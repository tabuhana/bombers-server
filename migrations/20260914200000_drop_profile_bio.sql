-- +goose Up
-- The bio was removed. Nothing in the client shows or edits one any more — a
-- card is its facts — so the column goes, and whatever was saved in it with it.
ALTER TABLE profiles DROP COLUMN IF EXISTS bio;

-- +goose Down
ALTER TABLE profiles ADD COLUMN bio TEXT NOT NULL DEFAULT '';
