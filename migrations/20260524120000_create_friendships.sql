-- +goose Up
-- Canonical-pair model: each friendship is stored as exactly ONE row keyed
-- by the lexicographically-ordered pair (user_a_id, user_b_id). The CHECK
-- constraint enforces user_a_id < user_b_id, which makes duplicate inverse
-- rows ((A,B) AND (B,A)) impossible at the schema level. The requested_by
-- column preserves "who initiated" — essential for pending (only the
-- recipient can accept) and blocked (only the blocker can unblock).
CREATE TABLE friendships (
    user_a_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_b_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    state        TEXT        NOT NULL,
    requested_by TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_a_id, user_b_id),
    CONSTRAINT friendships_canonical_order CHECK (user_a_id < user_b_id),
    CONSTRAINT friendships_state_valid     CHECK (state IN ('pending', 'accepted', 'blocked')),
    CONSTRAINT friendships_requester_in_pair
        CHECK (requested_by = user_a_id OR requested_by = user_b_id)
);

-- PK already indexes (user_a_id, user_b_id); add the mirror so lookups by
-- the second column ("everything involving user X") are also indexed.
CREATE INDEX friendships_user_b_idx ON friendships(user_b_id);

-- +goose Down
DROP TABLE friendships;
