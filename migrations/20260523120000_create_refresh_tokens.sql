-- +goose Up
CREATE TABLE refresh_tokens (
    id         TEXT        PRIMARY KEY,                                       -- jti embedded in the JWT
    user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    used_at    TIMESTAMPTZ                                                    -- NULL = still valid; non-NULL = rotated or revoked
);

CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens(user_id);

-- +goose Down
DROP TABLE refresh_tokens;
