package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrMediaNotFound is returned when no user_media row exists for (user, kind).
var ErrMediaNotFound = errors.New("media not found")

// dbExecutor is the subset of pgx methods the queries need; both *pgxpool.Pool
// and pgx.Tx satisfy it (same shape the other domains declare).
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// mediaRecord is the in-memory view of a user_media row — the metadata the
// API serves without touching the object store.
type mediaRecord struct {
	UserID      string
	Kind        string
	ContentType string
	SizeBytes   int64
	UpdatedAt   time.Time
}

const getMediaSQL = `
SELECT user_id, kind, content_type, size_bytes, updated_at
FROM user_media
WHERE user_id = $1 AND kind = $2
`

func getMedia(ctx context.Context, db dbExecutor, userID, kind string) (*mediaRecord, error) {
	var m mediaRecord
	err := db.QueryRow(ctx, getMediaSQL, userID, kind).Scan(
		&m.UserID, &m.Kind, &m.ContentType, &m.SizeBytes, &m.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMediaNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get media: %w", err)
	}
	return &m, nil
}

// upsertMedia records an upload. One row per (user, kind); a reupload
// overwrites the metadata and bumps updated_at (which rotates the ?v=
// cache-buster in the URLs the profile exposes).
const upsertMediaSQL = `
INSERT INTO user_media (user_id, kind, content_type, size_bytes, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (user_id, kind) DO UPDATE SET
  content_type = EXCLUDED.content_type,
  size_bytes   = EXCLUDED.size_bytes,
  updated_at   = NOW()
RETURNING user_id, kind, content_type, size_bytes, updated_at
`

func upsertMedia(ctx context.Context, db dbExecutor, userID, kind, contentType string, sizeBytes int64) (*mediaRecord, error) {
	var m mediaRecord
	err := db.QueryRow(ctx, upsertMediaSQL, userID, kind, contentType, sizeBytes).Scan(
		&m.UserID, &m.Kind, &m.ContentType, &m.SizeBytes, &m.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert media: %w", err)
	}
	return &m, nil
}

const deleteMediaSQL = `DELETE FROM user_media WHERE user_id = $1 AND kind = $2`

func deleteMedia(ctx context.Context, db dbExecutor, userID, kind string) error {
	if _, err := db.Exec(ctx, deleteMediaSQL, userID, kind); err != nil {
		return fmt.Errorf("delete media: %w", err)
	}
	return nil
}

// areFriends reports whether two users have an accepted friendship. Media owns
// this narrow query against the friendships table rather than importing
// internal/friends — the same loose-coupling tradeoff profiles, messaging, and
// nodeshare already make.
const areFriendsSQL = `
SELECT EXISTS (
  SELECT 1 FROM friendships
  WHERE state = 'accepted'
    AND ((user_a_id = $1 AND user_b_id = $2) OR (user_a_id = $2 AND user_b_id = $1))
)
`

func areFriends(ctx context.Context, db dbExecutor, a, b string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, areFriendsSQL, a, b).Scan(&ok); err != nil {
		return false, fmt.Errorf("check friendship: %w", err)
	}
	return ok, nil
}

// profileVisibility reads a user's self-card visibility — the same setting
// that gates GET /profiles/{id} gates their media. A user with no saved
// profile row defaults to 'friends' (matching the profiles domain's default),
// so an avatar is viewable by friends even before the text card is first
// saved. Narrow read against the profiles table, same tradeoff as areFriends
// above.
const profileVisibilitySQL = `SELECT visibility FROM profiles WHERE user_id = $1`

func profileVisibility(ctx context.Context, db dbExecutor, userID string) (string, error) {
	var v string
	err := db.QueryRow(ctx, profileVisibilitySQL, userID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "friends", nil
	}
	if err != nil {
		return "", fmt.Errorf("get profile visibility: %w", err)
	}
	return v, nil
}
