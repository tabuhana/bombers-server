-- +goose Up
-- Note sharing was removed from the client. The notes you keep are about other
-- people and stay with whoever wrote them, and nobody keeps notes about
-- themselves — so there is nothing left to publish per viewer. This table only
-- ever carried those notes; it goes with them.
--
-- FACTS are untouched: they were never here. They live on `profiles` and any
-- accepted friend reads them.
DROP TABLE IF EXISTS published_cards;

-- +goose Down
CREATE TABLE published_cards (
    owner_id   TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    viewer_id  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content    JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_id, viewer_id)
);
CREATE INDEX published_cards_viewer_idx ON published_cards (viewer_id);
