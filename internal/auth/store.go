package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dbExecutor is the subset of pgx methods the refresh-token queries need.
// Both *pgxpool.Pool and pgx.Tx satisfy it, so a single helper works inside
// and outside a transaction.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// refreshRow is the in-memory view of a refresh_tokens row used by rotation.
type refreshRow struct {
	UserID    string
	ExpiresAt time.Time
	UsedAt    *time.Time // nil = still valid
}

const insertRefreshTokenSQL = `
INSERT INTO refresh_tokens (id, user_id, expires_at)
VALUES ($1, $2, $3)
`

func insertRefreshToken(ctx context.Context, db dbExecutor, id, userID string, expiresAt time.Time) error {
	if _, err := db.Exec(ctx, insertRefreshTokenSQL, id, userID, expiresAt); err != nil {
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

// getRefreshTokenForUpdateSQL takes a row lock so two concurrent /auth/refresh
// calls with the same token can't both succeed — the second one blocks until
// the first commits and then sees used_at populated.
const getRefreshTokenForUpdateSQL = `
SELECT user_id, expires_at, used_at
FROM refresh_tokens
WHERE id = $1
FOR UPDATE
`

func getRefreshTokenForUpdate(ctx context.Context, tx pgx.Tx, id string) (*refreshRow, error) {
	var r refreshRow
	err := tx.QueryRow(ctx, getRefreshTokenForUpdateSQL, id).Scan(&r.UserID, &r.ExpiresAt, &r.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidRefresh
	}
	if err != nil {
		return nil, fmt.Errorf("select refresh token: %w", err)
	}
	return &r, nil
}

const markRefreshTokenUsedSQL = `UPDATE refresh_tokens SET used_at = NOW() WHERE id = $1`

func markRefreshTokenUsed(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, markRefreshTokenUsedSQL, id); err != nil {
		return fmt.Errorf("mark refresh token used: %w", err)
	}
	return nil
}

// invalidateAllUserRefreshTokensSQL is the reuse-detection blast radius —
// when a rotated token is presented again, every still-valid token belonging
// to that user is killed so an attacker (and possibly the legitimate user)
// is forced to re-authenticate.
const invalidateAllUserRefreshTokensSQL = `
UPDATE refresh_tokens
SET used_at = NOW()
WHERE user_id = $1 AND used_at IS NULL
`

func invalidateAllUserRefreshTokens(ctx context.Context, db dbExecutor, userID string) error {
	if _, err := db.Exec(ctx, invalidateAllUserRefreshTokensSQL, userID); err != nil {
		return fmt.Errorf("invalidate user refresh tokens: %w", err)
	}
	return nil
}
