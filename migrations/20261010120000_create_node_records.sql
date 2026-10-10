-- +goose Up
-- The people axis: a node putting something in front of named friends, and
-- collecting what they say back.
--
-- TWO SIDES, because nobody writes anyone else's data (the client's hard rule):
-- the OWNER publishes a record and names who may see it, and each RECIPIENT
-- attaches their own response. That one split is what makes it general — a
-- record plus responses is an invite with RSVPs, a poll with votes, a shared
-- list with additions, a challenge with everyone's results, or a "here, have
-- this" that expects no answer at all.
--
-- `body` is an opaque JSONB blob on both sides: the server stores it and hands
-- it back without ever looking inside (the same dumb-blob rule as sync and
-- node_transfers). It does not know what an invite is, and nothing here has to
-- change when a node invents a new kind of thing to send.
--
-- This is the DURABLE sibling of `rooms`. A room is live, in-memory, and dies
-- when the last person leaves — right for a game lobby, wrong for an invite,
-- which has to survive both people being offline and sit there for days.
CREATE TABLE node_records (
    id         TEXT        PRIMARY KEY,
    -- Which node's record this is, so two nodes' records never collide. NOT a
    -- permission boundary: a user's own token can read all of their own rows.
    -- What keeps one node out of another's is that a node never holds the
    -- token — it reaches this through the toolbox, which passes its own id.
    node_id    TEXT        NOT NULL,
    owner_id   TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body       JSONB       NOT NULL,
    -- Who may see it: user ids, checked against the owner's accepted friends
    -- when the record is written. An array rather than a join table (the
    -- profile_shares shape) because a record is addressed once, as a whole;
    -- the cost is that a deleted user leaves behind an id matching nobody,
    -- which reads as absence and is what it means.
    audience   TEXT[]      NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "What has this node of mine sent", newest first.
CREATE INDEX node_records_owner_idx ON node_records (owner_id, node_id, created_at DESC);
-- "What is in front of me" is a containment test, which needs GIN to use an index.
CREATE INDEX node_records_audience_idx ON node_records USING GIN (audience);

-- One response per person per record, and writing REPLACES. A response is your
-- answer; changing your mind is the same act again (Going, then Can't), not a
-- second answer beside the first. So the pair is the key, a write is an upsert,
-- and nobody can respond twice to stuff a vote.
CREATE TABLE node_responses (
    record_id  TEXT        NOT NULL REFERENCES node_records(id) ON DELETE CASCADE,
    author_id  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body       JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (record_id, author_id)
);

-- +goose Down
DROP TABLE node_responses;
DROP TABLE node_records;
