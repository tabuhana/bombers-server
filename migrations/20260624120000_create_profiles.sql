-- +goose Up
-- Self-card: a user's OWN published profile (distinct from the about-card a user
-- keeps about someone else, which stays client-local for now). One row per user,
-- keyed by user_id. Structured base fields + a freeform bio. Age is DERIVED from
-- birthday at read time and never stored (a stored age drifts the day after a
-- birthday). visibility gates who may fetch it: 'friends' (any accepted friend,
-- plus the owner) or 'private' (owner only). Specific-friend visibility is a
-- later refinement; the two-value enum covers v1.
CREATE TABLE profiles (
    user_id      TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name TEXT        NOT NULL DEFAULT '',
    birthday     DATE,
    country      TEXT        NOT NULL DEFAULT '',
    timezone     TEXT        NOT NULL DEFAULT '',
    bio          TEXT        NOT NULL DEFAULT '',
    visibility   TEXT        NOT NULL DEFAULT 'friends',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id),
    CONSTRAINT profiles_visibility_valid CHECK (visibility IN ('friends', 'private'))
);

-- +goose Down
DROP TABLE profiles;
