package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Refused sign-ins, and the identities refused permanently.
//
// The only route to the installer runs through the website terminal, so an
// identity arriving at a login with no account behind it didn't take it. The
// OAuth exchange tells us exactly who they are; this is where that gets kept.

// SigninAttempt is one Discord identity that tried and had no account.
type SigninAttempt struct {
	DiscordID       string
	DiscordUsername string
	Attempts        int
	FirstAt         time.Time
	LastAt          time.Time
	// BlockedAt is set when this identity is refused outright. Zero means not
	// blocked.
	BlockedAt   time.Time
	BlockReason string
}

// Blocked reports whether this identity is refused regardless of the allowlist.
func (a SigninAttempt) Blocked() bool { return !a.BlockedAt.IsZero() }

const recordAttemptSQL = `
INSERT INTO signin_attempts (discord_id, discord_username)
VALUES ($1, $2)
ON CONFLICT (discord_id) DO UPDATE
SET attempts         = signin_attempts.attempts + 1,
    last_at          = now(),
    discord_username = EXCLUDED.discord_username
RETURNING attempts
`

// RecordSigninAttempt notes that an identity with no account tried to sign in,
// returning how many times it has now done so.
func RecordSigninAttempt(ctx context.Context, pool *pgxpool.Pool, discordID, discordUsername string) (int, error) {
	var attempts int
	if err := pool.QueryRow(ctx, recordAttemptSQL, discordID, discordUsername).Scan(&attempts); err != nil {
		return 0, fmt.Errorf("record signin attempt: %w", err)
	}
	return attempts, nil
}

const isBlockedSQL = `
SELECT EXISTS (SELECT 1 FROM signin_attempts WHERE discord_id = $1 AND blocked_at IS NOT NULL)
`

// IsBlocked reports whether an identity is refused outright.
//
// Checked BEFORE the allowlist, so an identity that ends up on both stays
// refused rather than depending on which list is consulted first. A query
// failure must be treated by the caller as blocked.
func IsBlocked(ctx context.Context, pool *pgxpool.Pool, discordID string) (bool, error) {
	var blocked bool
	if err := pool.QueryRow(ctx, isBlockedSQL, discordID).Scan(&blocked); err != nil {
		return false, fmt.Errorf("check block: %w", err)
	}
	return blocked, nil
}

const blockIdentitySQL = `
INSERT INTO signin_attempts (discord_id, discord_username, attempts, blocked_at, block_reason)
VALUES ($1, '', 0, now(), $2)
ON CONFLICT (discord_id) DO UPDATE
SET blocked_at   = now(),
    block_reason = EXCLUDED.block_reason
`

// BlockIdentity refuses a Discord identity permanently. It works on one that
// has never tried, so somebody can be shut out in advance.
func BlockIdentity(ctx context.Context, pool *pgxpool.Pool, discordID, reason string) error {
	if _, err := pool.Exec(ctx, blockIdentitySQL, discordID, nullable(reason)); err != nil {
		return fmt.Errorf("block identity: %w", err)
	}
	return nil
}

const unblockIdentitySQL = `
UPDATE signin_attempts SET blocked_at = NULL, block_reason = NULL
WHERE discord_id = $1 AND blocked_at IS NOT NULL
`

// UnblockIdentity lifts a block, reporting whether one was there.
func UnblockIdentity(ctx context.Context, pool *pgxpool.Pool, discordID string) (bool, error) {
	tag, err := pool.Exec(ctx, unblockIdentitySQL, discordID)
	if err != nil {
		return false, fmt.Errorf("unblock identity: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

const listAttemptsSQL = `
SELECT discord_id, discord_username, attempts, first_at, last_at,
       COALESCE(blocked_at, 'epoch'::timestamptz), COALESCE(block_reason, '')
FROM signin_attempts
ORDER BY last_at DESC
LIMIT $1
`

// ListSigninAttempts returns refused sign-ins, most recent first.
func ListSigninAttempts(ctx context.Context, pool *pgxpool.Pool, limit int) ([]SigninAttempt, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := pool.Query(ctx, listAttemptsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("list signin attempts: %w", err)
	}
	defer rows.Close()

	var out []SigninAttempt
	for rows.Next() {
		var a SigninAttempt
		var blockedAt time.Time
		if err := rows.Scan(&a.DiscordID, &a.DiscordUsername, &a.Attempts,
			&a.FirstAt, &a.LastAt, &blockedAt, &a.BlockReason); err != nil {
			return nil, fmt.Errorf("scan signin attempt: %w", err)
		}
		// COALESCE needs a concrete timestamp, so "never blocked" arrives as the
		// epoch and is turned back into a zero time here.
		if blockedAt.Year() > 1970 {
			a.BlockedAt = blockedAt
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const clearAttemptSQL = `
DELETE FROM signin_attempts WHERE discord_id = $1 AND blocked_at IS NULL
`

// ClearSigninAttempt forgets an identity's attempts. It refuses to delete a
// blocked one — that would silently lift the block.
func ClearSigninAttempt(ctx context.Context, pool *pgxpool.Pool, discordID string) (bool, error) {
	tag, err := pool.Exec(ctx, clearAttemptSQL, discordID)
	if err != nil {
		return false, fmt.Errorf("clear signin attempt: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

const getAttemptSQL = `
SELECT discord_id, discord_username, attempts, first_at, last_at,
       COALESCE(blocked_at, 'epoch'::timestamptz), COALESCE(block_reason, '')
FROM signin_attempts
WHERE discord_id = $1
`

// GetSigninAttempt reads one identity's record, or ErrUserNotFound when there
// isn't one.
func GetSigninAttempt(ctx context.Context, pool *pgxpool.Pool, discordID string) (*SigninAttempt, error) {
	var a SigninAttempt
	var blockedAt time.Time
	err := pool.QueryRow(ctx, getAttemptSQL, discordID).Scan(
		&a.DiscordID, &a.DiscordUsername, &a.Attempts, &a.FirstAt, &a.LastAt,
		&blockedAt, &a.BlockReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get signin attempt: %w", err)
	}
	if blockedAt.Year() > 1970 {
		a.BlockedAt = blockedAt
	}
	return &a, nil
}
