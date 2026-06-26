-- +goose Up
-- About-card: the private notes user A keeps ABOUT user B (relationship, nickname,
-- how they met, favorites, freeform notes…). Distinct from the self-card (a user's
-- OWN published profile). One row per (author, subject) pair. The rich, evolving
-- field set is stored as a JSONB `content` blob the server treats opaquely — the
-- client owns the shape, which lets the about-card UI iterate without migrations.
-- Age is never stored: if a birthday lives in `content`, the client derives age.
--
-- visibility gates who may read it beyond the author: 'private' (author only,
-- the default) or 'subject' (the author has chosen to let the person it's about
-- see it too). Sharing with arbitrary third friends is a later refinement.
CREATE TABLE about_cards (
    author_id  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject_id TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    visibility TEXT        NOT NULL DEFAULT 'private',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (author_id, subject_id),
    CONSTRAINT about_cards_no_self CHECK (author_id <> subject_id),
    CONSTRAINT about_cards_visibility_valid CHECK (visibility IN ('private', 'subject'))
);

-- Reading "what has anyone shared ABOUT me" is a lookup by subject, so index it.
CREATE INDEX about_cards_subject_idx ON about_cards (subject_id);

-- +goose Down
DROP TABLE about_cards;
