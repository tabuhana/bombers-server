-- +goose Up
-- Per-FIELD, per-VIEWER sharing of your self-card.
--
-- The client groups people with user-made "relationships" (Partner, Family, and
-- anything else the user invents or deletes). The server deliberately knows
-- NOTHING about those groups — it would be wrong to hardcode a set of them here,
-- and pointless to sync names that only mean something in one notebook. The
-- client resolves its own groups down to the friend user-ids they contain and
-- publishes the RESULT: "these people may see my birthday".
--
-- So one row = one grant: owner shares one field with one viewer. Revoking is a
-- delete; re-grouping on the client is just a different set of rows next publish.
-- Both sides cascade on user delete, matching every other cross-user table.
CREATE TABLE profile_shares (
    owner_id   TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    field_key  TEXT        NOT NULL,
    viewer_id  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id, field_key, viewer_id)
);

-- The read path is always "what has this owner shared with me?", so index the
-- viewer side too (the PK already covers owner-side lookups).
CREATE INDEX profile_shares_viewer_idx ON profile_shares (viewer_id, owner_id);

-- The Me card carries a few facts the self-card never had a column for. They are
-- exactly the fields the sharing UI can hand out, so they land here rather than
-- in a blob: nickname, the city half of a location (country/timezone already
-- exist), and the categorized jots. `notes` is opaque JSON — the server stores
-- and returns it without ever looking inside, same contract as sync blobs.
ALTER TABLE profiles ADD COLUMN nickname TEXT  NOT NULL DEFAULT '';
ALTER TABLE profiles ADD COLUMN city     TEXT  NOT NULL DEFAULT '';
ALTER TABLE profiles ADD COLUMN notes    JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down
ALTER TABLE profiles DROP COLUMN notes;
ALTER TABLE profiles DROP COLUMN city;
ALTER TABLE profiles DROP COLUMN nickname;
DROP TABLE profile_shares;
