package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUsernameTaken is returned when the canonical-username UNIQUE constraint
// rejects an insert. Callers translate this into a 409 at the HTTP layer.
var ErrUsernameTaken = errors.New("username already taken")

// ErrUserNotFound is returned by lookup queries when no row matches.
// The login handler MUST NOT propagate this distinction to clients —
// it collapses to a generic invalid_credentials response.
var ErrUserNotFound = errors.New("user not found")

// errFriendCodeTaken is the package-internal signal for "regenerate and try
// again." Never propagated past createUser.
var errFriendCodeTaken = errors.New("friend code collision")

const (
	// Constraint names follow Postgres' default <table>_<column>_key convention
	// for inline UNIQUE declarations — see migrations/20260522120000_create_users.sql.
	constraintUsernameCanonicalKey = "users_username_canonical_key"
	constraintFriendCodeKey        = "users_friend_code_key"

	pgUniqueViolation = "23505"
)

const insertUserSQL = `
INSERT INTO users (id, username, username_canonical, password_hash, friend_code)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at
`

func insertUser(ctx context.Context, pool *pgxpool.Pool, u *User) error {
	var created time.Time
	err := pool.QueryRow(ctx, insertUserSQL,
		u.ID, u.Username, u.UsernameCanonical, u.PasswordHash, u.FriendCode,
	).Scan(&created)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			switch pgErr.ConstraintName {
			case constraintUsernameCanonicalKey:
				return ErrUsernameTaken
			case constraintFriendCodeKey:
				return errFriendCodeTaken
			}
		}
		return fmt.Errorf("insert user: %w", err)
	}
	u.CreatedAt = created
	return nil
}

const getUserByCanonicalSQL = `
SELECT id, username, username_canonical, password_hash, friend_code, created_at,
       banned_at IS NOT NULL
FROM users
WHERE username_canonical = $1
`

// getUserByCanonical looks up a user by the canonical username form.
// Returns ErrUserNotFound when no row matches.
func getUserByCanonical(ctx context.Context, pool *pgxpool.Pool, canonical string) (*User, error) {
	var u User
	err := pool.QueryRow(ctx, getUserByCanonicalSQL, canonical).Scan(
		&u.ID, &u.Username, &u.UsernameCanonical, &u.PasswordHash, &u.FriendCode, &u.CreatedAt,
		&u.Banned,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by canonical: %w", err)
	}
	return &u, nil
}

const getUserByIDSQL = `
SELECT id, username, username_canonical, password_hash, friend_code, created_at
FROM users
WHERE id = $1
`

// getUserByID looks up a user by primary key. Returns ErrUserNotFound when
// no row matches — e.g. a still-valid access token for a since-deleted user.
func getUserByID(ctx context.Context, pool *pgxpool.Pool, id string) (*User, error) {
	var u User
	err := pool.QueryRow(ctx, getUserByIDSQL, id).Scan(
		&u.ID, &u.Username, &u.UsernameCanonical, &u.PasswordHash, &u.FriendCode, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return &u, nil
}
