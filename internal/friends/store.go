package friends

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUserNotFound is returned when a lookup against the users table misses.
// Mapped to 401 at the HTTP boundary (a valid-signature token for a user
// that no longer exists is still "unauthorized" from the client's view).
var ErrUserNotFound = errors.New("user not found")

// Friends reads the columns it needs from the users table via its own narrow
// queries rather than importing internal/users. This keeps the domain
// coupling one-directional (no friends ↔ users dependency) and the query
// surface explicit per domain — the cost is a small amount of SQL duplication,
// which is acceptable while the schema is stable.
const getOwnFriendCodeSQL = `
SELECT friend_code
FROM users
WHERE id = $1
`

func getOwnFriendCode(ctx context.Context, pool *pgxpool.Pool, userID string) (string, error) {
	var code string
	err := pool.QueryRow(ctx, getOwnFriendCodeSQL, userID).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get own friend code: %w", err)
	}
	return code, nil
}
