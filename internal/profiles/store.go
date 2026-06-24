package profiles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Visibility values — mirror the CHECK constraint in the migration.
const (
	VisibilityFriends = "friends"
	VisibilityPrivate = "private"
)

// ErrProfileNotFound is returned when no profile row exists for a user. Callers
// branch on it (e.g. GetMine returns a default empty card instead of erroring).
var ErrProfileNotFound = errors.New("profile not found")

// dbExecutor is the subset of pgx methods the queries need; both *pgxpool.Pool
// and pgx.Tx satisfy it.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// profileRecord is the in-memory view of a profiles row. Birthday is nullable
// (a user may not set one), hence the pointer.
type profileRecord struct {
	UserID      string
	DisplayName string
	Birthday    *time.Time
	Country     string
	Timezone    string
	Bio         string
	Visibility  string
	UpdatedAt   time.Time
}

const getProfileSQL = `
SELECT user_id, display_name, birthday, country, timezone, bio, visibility, updated_at
FROM profiles
WHERE user_id = $1
`

func getProfile(ctx context.Context, db dbExecutor, userID string) (*profileRecord, error) {
	var p profileRecord
	err := db.QueryRow(ctx, getProfileSQL, userID).Scan(
		&p.UserID, &p.DisplayName, &p.Birthday, &p.Country, &p.Timezone, &p.Bio, &p.Visibility, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get profile: %w", err)
	}
	return &p, nil
}

// upsertProfile inserts or replaces the caller's self-card. One row per user, so
// ON CONFLICT (user_id) overwrites the editable fields and bumps updated_at;
// created_at is preserved by leaving it out of the SET list.
const upsertProfileSQL = `
INSERT INTO profiles (user_id, display_name, birthday, country, timezone, bio, visibility, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  birthday     = EXCLUDED.birthday,
  country      = EXCLUDED.country,
  timezone     = EXCLUDED.timezone,
  bio          = EXCLUDED.bio,
  visibility   = EXCLUDED.visibility,
  updated_at   = NOW()
RETURNING user_id, display_name, birthday, country, timezone, bio, visibility, updated_at
`

func upsertProfile(ctx context.Context, pool *pgxpool.Pool, p *profileRecord) (*profileRecord, error) {
	var out profileRecord
	err := pool.QueryRow(ctx, upsertProfileSQL,
		p.UserID, p.DisplayName, p.Birthday, p.Country, p.Timezone, p.Bio, p.Visibility,
	).Scan(
		&out.UserID, &out.DisplayName, &out.Birthday, &out.Country, &out.Timezone, &out.Bio, &out.Visibility, &out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert profile: %w", err)
	}
	return &out, nil
}

// areFriends reports whether two users have an accepted friendship. Profiles
// owns this narrow query against the friendships table rather than importing
// internal/friends — same loose-coupling tradeoff friends makes against the
// users table (see internal/friends/store.go). The pair is matched in either
// order so the caller needn't canonicalize.
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

// userExists guards GetForUser so a request for a nonexistent user collapses to
// the same opaque not-found as a non-visible profile.
const userExistsSQL = `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`

func userExists(ctx context.Context, db dbExecutor, id string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, userExistsSQL, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check user exists: %w", err)
	}
	return ok, nil
}
