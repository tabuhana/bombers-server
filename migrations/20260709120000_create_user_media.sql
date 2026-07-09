-- +goose Up
-- Profile media metadata: one row per (user, kind) for the avatar/banner a user
-- has uploaded. The image BYTES live in S3-compatible object storage (MinIO
-- locally, MinIO-on-VPS or R2 in prod) under the derived key
-- users/<user_id>/<kind> — this table holds only the metadata the API needs
-- without a round-trip to the object store: which media exist (for profile
-- avatar_url/banner_url fields), the sniffed content type to serve back, the
-- size, and updated_at (drives the ?v= cache-buster and ETag). Replace-on-
-- reupload overwrites both the object (same key) and this row.
CREATE TABLE user_media (
    user_id      TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL,
    content_type TEXT        NOT NULL,
    size_bytes   BIGINT      NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, kind),
    CONSTRAINT user_media_kind_valid CHECK (kind IN ('avatar', 'banner'))
);

-- +goose Down
DROP TABLE user_media;
