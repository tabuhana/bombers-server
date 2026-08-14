-- +goose Up
-- Who is around, and what they'd like you to think about that.
--
-- Two things are deliberately conflated in one row, because they are two halves
-- of the same question:
--
--   `status`     what you CHOSE — online, away, dnd, offline.
--   `updated_at` when you last said anything at all, which is the heartbeat.
--
-- Neither is the answer on its own. A stored `online` from a laptop that shut
-- three days ago is a lie, and a fresh heartbeat from someone who set themselves
-- offline is none of your business. The effective status is computed from both,
-- at read time, and never stored — the same rule the profile follows for age.
--
-- No history. This table only ever answers "now", so a row is overwritten rather
-- than appended to; keeping a log of when somebody was at their computer is a
-- surveillance feature wearing an analytics hat.
CREATE TABLE presence (
    user_id    TEXT        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    status     TEXT        NOT NULL DEFAULT 'online',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE presence;
