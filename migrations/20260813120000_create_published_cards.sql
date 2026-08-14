-- +goose Up
-- The person card, as published to ONE viewer.
--
-- This replaces per-field grants (profile_shares, dropped below) with a simpler
-- division of labour, and the reason is the notes.
--
-- FACTS — birthday, city, nickname — are the same for everyone you're linked to.
-- There is no per-friend choice to make, so they stay as ordinary columns on
-- `profiles` and any accepted friend reads them. That is the whole of the facts
-- story now; the redaction that used to sit in front of them is gone.
--
-- NOTES are the opposite. You share them per category, and any single note can
-- depart from its category's audience. Expressing that as server-side grants
-- would mean the server learning what a note is and holding a row per note per
-- viewer — a permission model that has to understand your writing.
--
-- So the client does the deciding. It works out what each viewer should see and
-- publishes that, one blob per viewer, and the server hands out whichever blob
-- belongs to the reader. `content` is opaque JSONB: never parsed, never
-- validated, same contract as a sync item.
--
-- The cost is honest and worth naming: a sharing change is a re-publish rather
-- than a permission flip, and an owner with twenty friends writes twenty rows.
-- At the scale this server is for, that is nothing.
--
-- A publish REPLACES the whole set for an owner, which is what makes revocation
-- free: a viewer who is no longer in the map has no row, and no row is no
-- access. Both sides cascade on user delete, like every other cross-user table.
CREATE TABLE published_cards (
    owner_id   TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    viewer_id  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content    JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id, viewer_id)
);

-- The read path is always "what has this owner published for me?", which the
-- primary key already serves. This covers the other direction — everything
-- published for me — which is how a client refreshes every card at once.
CREATE INDEX published_cards_viewer_idx ON published_cards (viewer_id);

-- Per-field grants are gone: facts need no grant, and notes are decided on the
-- client now. Keeping it would mean two mechanisms for who-sees-what, which is
-- the thing most likely to disagree with itself later.
DROP INDEX IF EXISTS profile_shares_viewer_idx;
DROP TABLE IF EXISTS profile_shares;

-- profiles.notes went with it. Notes were stored here as one blob for everyone
-- and filtered per viewer on read; they now live in published_cards, already
-- narrowed to their reader.
ALTER TABLE profiles DROP COLUMN IF EXISTS notes;

-- +goose Down
ALTER TABLE profiles ADD COLUMN notes JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE profile_shares (
    owner_id   TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    field_key  TEXT        NOT NULL,
    viewer_id  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id, field_key, viewer_id)
);
CREATE INDEX profile_shares_viewer_idx ON profile_shares (viewer_id, owner_id);

DROP TABLE published_cards;
