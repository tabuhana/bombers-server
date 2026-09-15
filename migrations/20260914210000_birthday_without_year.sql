-- +goose Up
-- A birthday no longer needs a year. The client saves as much of the date as
-- the person knows — "03-14" when nobody knows the year — but a DATE only holds
-- a whole one, so a birthday like that couldn't be published at all. TEXT holds
-- every shape, and the server checks the shape on the way in. Dates already
-- saved carry over as the "YYYY-MM-DD" they were sent as.
ALTER TABLE profiles
    ALTER COLUMN birthday TYPE TEXT USING to_char(birthday, 'YYYY-MM-DD');

-- +goose Down
-- A DATE only holds a whole date, so every partial birthday becomes NULL.
ALTER TABLE profiles
    ALTER COLUMN birthday TYPE DATE
    USING CASE WHEN birthday ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' THEN birthday::date END;
