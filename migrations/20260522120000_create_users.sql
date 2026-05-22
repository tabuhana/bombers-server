-- +goose Up
CREATE TABLE users (
    id                 TEXT        PRIMARY KEY,
    username           TEXT        NOT NULL,
    username_canonical TEXT        NOT NULL UNIQUE,
    password_hash      TEXT        NOT NULL,
    friend_code        TEXT        NOT NULL UNIQUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT users_username_canonical_matches
        CHECK (username_canonical = lower(btrim(username, E' \t\n\r\v\f')))
);

-- +goose Down
DROP TABLE users;
