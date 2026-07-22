package rooms

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Rooms themselves are never persisted - the only database this package touches
// is for the two questions it must answer before letting someone in: are these
// two accepted friends, and what is this user called. Narrow queries owned here
// rather than importing the friends/users domains, the same loose-coupling
// tradeoff profiles and messaging already make.

const areFriendsSQL = `
SELECT EXISTS (
  SELECT 1 FROM friendships
  WHERE state = 'accepted'
    AND ((user_a_id = $1 AND user_b_id = $2) OR (user_a_id = $2 AND user_b_id = $1))
)
`

func areFriends(ctx context.Context, pool *pgxpool.Pool, a, b string) (bool, error) {
	var ok bool
	if err := pool.QueryRow(ctx, areFriendsSQL, a, b).Scan(&ok); err != nil {
		return false, fmt.Errorf("check friendship: %w", err)
	}
	return ok, nil
}

const usernameSQL = `SELECT username FROM users WHERE id = $1`

// usernameFor resolves a display handle for the presence list. A miss isn't
// fatal: the roster falls back to the id, because a name is a nicety and being
// in the room is the point.
func usernameFor(ctx context.Context, pool *pgxpool.Pool, userID string) string {
	var name string
	if err := pool.QueryRow(ctx, usernameSQL, userID).Scan(&name); err != nil {
		return ""
	}
	return name
}
