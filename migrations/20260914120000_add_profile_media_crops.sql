-- +goose Up
-- How the avatar and banner sit in their frames.
--
-- A picture is rarely the shape of the slot it's shown in, so its owner frames
-- it, and every friend's client has to draw that same framing. That makes it a
-- fact on the card like the others, saved with PUT /me/profile.
--
-- x and y are CSS object-position percentages (0–100) and scale is zoom (1–4).
-- The defaults are centred and unzoomed — CSS's own default — so existing cards
-- need no backfill.
ALTER TABLE profiles ADD COLUMN avatar_crop_x     REAL NOT NULL DEFAULT 50;
ALTER TABLE profiles ADD COLUMN avatar_crop_y     REAL NOT NULL DEFAULT 50;
ALTER TABLE profiles ADD COLUMN avatar_crop_scale REAL NOT NULL DEFAULT 1;
ALTER TABLE profiles ADD COLUMN banner_crop_x     REAL NOT NULL DEFAULT 50;
ALTER TABLE profiles ADD COLUMN banner_crop_y     REAL NOT NULL DEFAULT 50;
ALTER TABLE profiles ADD COLUMN banner_crop_scale REAL NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE profiles DROP COLUMN banner_crop_scale;
ALTER TABLE profiles DROP COLUMN banner_crop_y;
ALTER TABLE profiles DROP COLUMN banner_crop_x;
ALTER TABLE profiles DROP COLUMN avatar_crop_scale;
ALTER TABLE profiles DROP COLUMN avatar_crop_y;
ALTER TABLE profiles DROP COLUMN avatar_crop_x;
