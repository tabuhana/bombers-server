package friends

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/types"
)

// Friendship state constants — mirror the CHECK constraint in the migration.
const (
	StatePending  = "pending"
	StateAccepted = "accepted"
	StateBlocked  = "blocked"
)

var (
	// ErrUserNotFound is returned by user lookups against the users table.
	// Generally collapses to 404/401 at the HTTP layer depending on context.
	ErrUserNotFound = errors.New("user not found")

	// ErrFriendshipNotFound is returned when no friendship row exists for
	// the requested pair. Not an error in most flows — callers branch on it.
	ErrFriendshipNotFound = errors.New("friendship not found")
)

// dbExecutor is the subset of pgx methods our queries need. Both
// *pgxpool.Pool and pgx.Tx satisfy it.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// userRecord is the friends-domain view of a user. Friends owns its own
// narrow queries against the users table instead of importing internal/users —
// see CLAUDE.md "domains should stay loosely coupled". The duplication is the
// price; the alternative is a bidirectional dep, which is worse.
type userRecord struct {
	ID         string
	Username   string
	FriendCode string
	CreatedAt  time.Time
}

func (u *userRecord) public() types.PublicUser {
	return types.PublicUser{
		ID:         u.ID,
		Username:   u.Username,
		FriendCode: u.FriendCode,
		CreatedAt:  u.CreatedAt,
	}
}

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

const getUserByFriendCodeSQL = `
SELECT id, username, friend_code, created_at
FROM users
WHERE friend_code = $1
`

func getUserByFriendCode(ctx context.Context, pool *pgxpool.Pool, code string) (*userRecord, error) {
	var u userRecord
	err := pool.QueryRow(ctx, getUserByFriendCodeSQL, code).Scan(
		&u.ID, &u.Username, &u.FriendCode, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by friend code: %w", err)
	}
	return &u, nil
}

const getUserByIDSQL = `
SELECT id, username, friend_code, created_at
FROM users
WHERE id = $1
`

func getUserByID(ctx context.Context, db dbExecutor, id string) (*userRecord, error) {
	var u userRecord
	err := db.QueryRow(ctx, getUserByIDSQL, id).Scan(
		&u.ID, &u.Username, &u.FriendCode, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return &u, nil
}

// friendshipRow is the in-memory view of a friendships row. Pair is stored
// canonically (UserA < UserB); RequestedBy is the original initiator.
type friendshipRow struct {
	UserA       string
	UserB       string
	State       string
	RequestedBy string
}

const getFriendshipForUpdateSQL = `
SELECT user_a_id, user_b_id, state, requested_by
FROM friendships
WHERE user_a_id = $1 AND user_b_id = $2
FOR UPDATE
`

// getFriendshipForUpdate looks up the row inside a transaction and takes a
// row lock so concurrent SendRequest calls for the same pair serialize.
// The pair MUST already be canonicalized (userA < userB).
func getFriendshipForUpdate(ctx context.Context, tx pgx.Tx, userA, userB string) (*friendshipRow, error) {
	var f friendshipRow
	err := tx.QueryRow(ctx, getFriendshipForUpdateSQL, userA, userB).Scan(
		&f.UserA, &f.UserB, &f.State, &f.RequestedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFriendshipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get friendship: %w", err)
	}
	return &f, nil
}

const insertPendingFriendshipSQL = `
INSERT INTO friendships (user_a_id, user_b_id, state, requested_by)
VALUES ($1, $2, 'pending', $3)
`

func insertPendingFriendship(ctx context.Context, db dbExecutor, userA, userB, requestedBy string) error {
	if _, err := db.Exec(ctx, insertPendingFriendshipSQL, userA, userB, requestedBy); err != nil {
		return fmt.Errorf("insert pending friendship: %w", err)
	}
	return nil
}

const updateFriendshipStateSQL = `
UPDATE friendships
SET state = $3, updated_at = NOW()
WHERE user_a_id = $1 AND user_b_id = $2
`

func updateFriendshipState(ctx context.Context, db dbExecutor, userA, userB, newState string) error {
	if _, err := db.Exec(ctx, updateFriendshipStateSQL, userA, userB, newState); err != nil {
		return fmt.Errorf("update friendship state: %w", err)
	}
	return nil
}

const deleteFriendshipSQL = `
DELETE FROM friendships
WHERE user_a_id = $1 AND user_b_id = $2
`

func deleteFriendship(ctx context.Context, db dbExecutor, userA, userB string) error {
	if _, err := db.Exec(ctx, deleteFriendshipSQL, userA, userB); err != nil {
		return fmt.Errorf("delete friendship: %w", err)
	}
	return nil
}

const insertBlockedFriendshipSQL = `
INSERT INTO friendships (user_a_id, user_b_id, state, requested_by)
VALUES ($1, $2, 'blocked', $3)
`

func insertBlockedFriendship(ctx context.Context, db dbExecutor, userA, userB, blocker string) error {
	if _, err := db.Exec(ctx, insertBlockedFriendshipSQL, userA, userB, blocker); err != nil {
		return fmt.Errorf("insert blocked friendship: %w", err)
	}
	return nil
}

// updateFriendshipBlock overrides any prior state (accepted, pending) with
// a fresh block by `blocker`. Used when we already know a row exists for
// the pair but it isn't in blocked state.
const updateFriendshipBlockSQL = `
UPDATE friendships
SET state = 'blocked', requested_by = $3, updated_at = NOW()
WHERE user_a_id = $1 AND user_b_id = $2
`

func updateFriendshipBlock(ctx context.Context, db dbExecutor, userA, userB, blocker string) error {
	if _, err := db.Exec(ctx, updateFriendshipBlockSQL, userA, userB, blocker); err != nil {
		return fmt.Errorf("update friendship block: %w", err)
	}
	return nil
}

// pendingRequest extends userRecord with the side-of-the-request flag so the
// HTTP layer can split a single result set into {incoming, outgoing}.
type pendingRequest struct {
	userRecord
	IsOutgoing bool
}

// CASE expression in the JOIN resolves "the other user in the pair" — works
// regardless of which side ($1) sits on. Without this we'd need two queries
// (one for each side) and stitch the results in Go.
const listAcceptedFriendsSQL = `
SELECT u.id, u.username, u.friend_code, u.created_at
FROM friendships f
JOIN users u
  ON u.id = (CASE WHEN f.user_a_id = $1 THEN f.user_b_id ELSE f.user_a_id END)
WHERE (f.user_a_id = $1 OR f.user_b_id = $1)
  AND f.state = 'accepted'
ORDER BY u.username ASC
`

func listAcceptedFriends(ctx context.Context, pool *pgxpool.Pool, userID string) ([]userRecord, error) {
	rows, err := pool.Query(ctx, listAcceptedFriendsSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("list accepted friends: %w", err)
	}
	defer rows.Close()

	// Initialize empty (not nil) so an empty result marshals to [] not null.
	out := []userRecord{}
	for rows.Next() {
		var u userRecord
		if err := rows.Scan(&u.ID, &u.Username, &u.FriendCode, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan friend row: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate friend rows: %w", err)
	}
	return out, nil
}

// is_outgoing is computed at the DB ("did the authed user initiate this row?")
// so one round trip produces both halves of the {incoming, outgoing} split.
const listPendingRequestsSQL = `
SELECT
  u.id, u.username, u.friend_code, u.created_at,
  (f.requested_by = $1) AS is_outgoing
FROM friendships f
JOIN users u
  ON u.id = (CASE WHEN f.user_a_id = $1 THEN f.user_b_id ELSE f.user_a_id END)
WHERE (f.user_a_id = $1 OR f.user_b_id = $1)
  AND f.state = 'pending'
ORDER BY f.created_at DESC
`

func listPendingRequests(ctx context.Context, pool *pgxpool.Pool, userID string) ([]pendingRequest, error) {
	rows, err := pool.Query(ctx, listPendingRequestsSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("list pending requests: %w", err)
	}
	defer rows.Close()

	out := []pendingRequest{}
	for rows.Next() {
		var r pendingRequest
		if err := rows.Scan(&r.ID, &r.Username, &r.FriendCode, &r.CreatedAt, &r.IsOutgoing); err != nil {
			return nil, fmt.Errorf("scan request row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate request rows: %w", err)
	}
	return out, nil
}

// canonicalPair returns the two user ids ordered for storage in the
// friendships table (matches the friendships_canonical_order CHECK).
func canonicalPair(a, b string) (string, string) {
	if a < b {
		return a, b
	}
	return b, a
}
