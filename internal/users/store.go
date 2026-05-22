package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUsernameTaken is returned when the canonical-username UNIQUE constraint
// rejects an insert. Callers translate this into a 409 at the HTTP layer.
var ErrUsernameTaken = errors.New("username already taken")

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
