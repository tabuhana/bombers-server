-- +goose Up
-- THE PATCH-NOTES ARCHIVE — what changed in every release of the desktop app.
--
-- ITS OWN TABLE, NOT A COLUMN ON `releases`. `POST /releases` prunes to the
-- newest three, and the app's patch-notes screen lists EVERY release that ever
-- shipped. Notes are therefore never pruned: nothing deletes a row here but an
-- operator's explicit DELETE.
--
-- ORDER BY THE VERSION NUMBERS, never by upload time and never by the text. The
-- archive (0.1.0 onwards) is uploaded in one go, so upload order means nothing;
-- a reworded old note is a republish that must not jump to the top; and text
-- order would put 0.1.9 above 0.1.10. That is why major/minor/patch are real
-- integer columns beside the canonical `version` string, which stays the key.
--
-- `parts` is a JSONB array of {title, body}. The server validates its shape in
-- Go; the database does not.
CREATE TABLE patch_notes (
    version    TEXT        PRIMARY KEY,
    major      INTEGER     NOT NULL,
    minor      INTEGER     NOT NULL,
    patch      INTEGER     NOT NULL,
    parts      JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (major, minor, patch)
);

-- No separate index for the list order: the UNIQUE index on (major, minor,
-- patch) is a btree on exactly those columns and Postgres scans it backwards
-- for `ORDER BY major DESC, minor DESC, patch DESC`.

-- +goose Down
DROP TABLE patch_notes;
