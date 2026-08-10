package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// The queries behind Discord sign-in, and behind the allowlist that decides who
// may sign in at all. They live in `users` because they read and write the
// users table, and reaching into another domain's table is the one thing the
// package layout is meant to prevent.

// ErrDiscordAlreadyLinked is returned when a Discord account is already
// attached to a different Bombers account. Two Bombers accounts sharing one
// Discord identity would make "who is this" unanswerable.
var ErrDiscordAlreadyLinked = errors.New("that Discord account is already linked to another user")

const constraintDiscordIDKey = "users_discord_id_key"

// DiscordProfile is what a sign-in learns about someone, ready to be stored.
type DiscordProfile struct {
	ID       string
	Username string
	// Avatar is Discord's hash, not a URL and not the image — see the migration.
	Avatar string
	// Connections is their linked accounts as Discord returned them. Stored
	// opaquely: the client decides what to show, so Discord adding a service
	// needs no migration here.
	Connections []byte
}

// EncodeConnections turns whatever the Discord client returned into the JSONB
// payload. A nil or failed encode stores SQL NULL rather than blocking a login
// over decoration.
func EncodeConnections(conns any) []byte {
	if conns == nil {
		return nil
	}
	raw, err := json.Marshal(conns)
	if err != nil {
		return nil
	}
	return raw
}

const getUserByDiscordIDSQL = `
SELECT id, username, username_canonical, COALESCE(password_hash, ''), friend_code, created_at,
       is_admin, banned_at IS NOT NULL
FROM users
WHERE discord_id = $1
`

// GetUserByDiscordID finds the account a Discord identity owns, or
// ErrUserNotFound when that Discord account has never signed in here.
func GetUserByDiscordID(ctx context.Context, pool *pgxpool.Pool, discordID string) (*User, error) {
	var u User
	err := pool.QueryRow(ctx, getUserByDiscordIDSQL, discordID).Scan(
		&u.ID, &u.Username, &u.UsernameCanonical, &u.PasswordHash, &u.FriendCode, &u.CreatedAt,
		&u.IsAdmin, &u.Banned,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by discord id: %w", err)
	}
	return &u, nil
}

const insertDiscordUserSQL = `
INSERT INTO users (id, username, username_canonical, friend_code,
                   discord_id, discord_username, discord_avatar, discord_connections)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at
`

// insertDiscordUser creates an account with no password at all — there is
// nothing to reset, and nothing for a leaked database to crack.
func insertDiscordUser(ctx context.Context, pool *pgxpool.Pool, u *User, p DiscordProfile) error {
	var created time.Time
	err := pool.QueryRow(ctx, insertDiscordUserSQL,
		u.ID, u.Username, u.UsernameCanonical, u.FriendCode,
		p.ID, p.Username, nullable(p.Avatar), p.Connections,
	).Scan(&created)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			switch pgErr.ConstraintName {
			case constraintUsernameCanonicalKey:
				return ErrUsernameTaken
			case constraintFriendCodeKey:
				return errFriendCodeTaken
			case constraintDiscordIDKey:
				return ErrDiscordAlreadyLinked
			}
		}
		return fmt.Errorf("insert discord user: %w", err)
	}
	u.CreatedAt = created
	return nil
}

const refreshDiscordProfileSQL = `
UPDATE users
SET discord_username    = $2,
    discord_avatar      = $3,
    discord_connections = COALESCE($4, discord_connections)
WHERE id = $1
`

// RefreshDiscordProfile updates what we know about someone from their latest
// sign-in. Their handle, picture and linked accounts change on Discord's side
// and this is the only moment we're allowed to look — we hold no token between
// logins on purpose.
//
// Connections are only overwritten when we actually read some: a failure
// fetching them is not a reason to erase what we already had.
func RefreshDiscordProfile(ctx context.Context, pool *pgxpool.Pool, userID string, p DiscordProfile) error {
	_, err := pool.Exec(ctx, refreshDiscordProfileSQL, userID, p.Username, nullable(p.Avatar), p.Connections)
	if err != nil {
		return fmt.Errorf("refresh discord profile: %w", err)
	}
	return nil
}

const linkDiscordSQL = `
UPDATE users
SET discord_id          = $2,
    discord_username    = $3,
    discord_avatar      = $4,
    discord_connections = COALESCE($5, discord_connections)
WHERE id = $1
`

// LinkDiscord attaches a Discord identity to an account that already exists.
//
// Two jobs, and the second is why it isn't just part of signing in: it's how an
// account that predates Discord login keeps everything it owns, and it's the
// escape hatch for somebody whose Discord account is gone — the operator points
// their Bombers account at a new one from the console.
func LinkDiscord(ctx context.Context, pool *pgxpool.Pool, userID string, p DiscordProfile) error {
	tag, err := pool.Exec(ctx, linkDiscordSQL, userID, p.ID, p.Username, nullable(p.Avatar), p.Connections)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == constraintDiscordIDKey {
			return ErrDiscordAlreadyLinked
		}
		return fmt.Errorf("link discord: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

const unlinkDiscordSQL = `
UPDATE users SET discord_id = NULL WHERE id = $1
`

// UnlinkDiscord detaches a Discord identity, leaving the account otherwise
// intact. Without a password that account can no longer be signed into, which
// is the point when it's being handed to a different Discord.
func UnlinkDiscord(ctx context.Context, pool *pgxpool.Pool, userID string) error {
	tag, err := pool.Exec(ctx, unlinkDiscordSQL, userID)
	if err != nil {
		return fmt.Errorf("unlink discord: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// CreateDiscordUser makes an account for a verified Discord identity, under a
// username the person chose.
//
// The name is theirs to pick rather than derived from their Discord handle,
// because a derived one collides — and the only ways out of a collision are
// appending a number to somebody's name or asking them anyway. Since signing up
// happens on the website, there's already a form to ask on.
//
// Same friend-code retry as the password path: a code collision is a dice roll
// worth re-rolling, while a taken username is the person's to resolve.
func CreateDiscordUser(ctx context.Context, pool *pgxpool.Pool, username string, p DiscordProfile) (*User, error) {
	name, err := validateUsername(username)
	if err != nil {
		return nil, err
	}

	id := ulid.Make().String()
	canonical := CanonicalUsername(name)

	for attempt := 0; attempt < friendCodeMaxAttempts; attempt++ {
		code, cerr := GenerateFriendCode()
		if cerr != nil {
			return nil, cerr
		}
		u := &User{
			ID:                id,
			Username:          name,
			UsernameCanonical: canonical,
			FriendCode:        code,
		}
		err = insertDiscordUser(ctx, pool, u, p)
		if err == nil {
			return u, nil
		}
		if errors.Is(err, errFriendCodeTaken) {
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("could not generate unique friend code after %d attempts", friendCodeMaxAttempts)
}

// --- the allowlist -------------------------------------------------------

// AllowlistEntry is one person cleared to create an account.
type AllowlistEntry struct {
	DiscordID string
	Note      string
	AddedAt   time.Time
}

const isAllowedSQL = `SELECT EXISTS (SELECT 1 FROM signup_allowlist WHERE discord_id = $1)`

// IsAllowed reports whether a Discord id may create an account.
//
// Only consulted when SIGNUP_MODE is "list". A query failure must be treated by
// the caller as "no" — the failure mode of a broken allowlist should be nobody
// getting in, not everybody.
func IsAllowed(ctx context.Context, pool *pgxpool.Pool, discordID string) (bool, error) {
	var ok bool
	if err := pool.QueryRow(ctx, isAllowedSQL, discordID).Scan(&ok); err != nil {
		return false, fmt.Errorf("check allowlist: %w", err)
	}
	return ok, nil
}

const addToAllowlistSQL = `
INSERT INTO signup_allowlist (discord_id, note)
VALUES ($1, $2)
ON CONFLICT (discord_id) DO UPDATE SET note = EXCLUDED.note
RETURNING (xmax = 0)
`

// AddToAllowlist clears a Discord id to sign up, reporting whether this added
// somebody new rather than re-noting somebody already there. Re-adding is not
// an error: "make sure this person can sign up" should be safe to repeat.
func AddToAllowlist(ctx context.Context, pool *pgxpool.Pool, discordID, note string) (bool, error) {
	var inserted bool
	// xmax = 0 is true only for a freshly inserted row, so one round trip
	// distinguishes "added" from "updated" without a prior SELECT.
	if err := pool.QueryRow(ctx, addToAllowlistSQL, discordID, note).Scan(&inserted); err != nil {
		return false, fmt.Errorf("add to allowlist: %w", err)
	}
	return inserted, nil
}

const removeFromAllowlistSQL = `DELETE FROM signup_allowlist WHERE discord_id = $1`

// RemoveFromAllowlist withdraws permission to sign UP. It does not touch an
// account that already exists — for that there's `ban`, which is a different
// decision and reversible in a different way.
func RemoveFromAllowlist(ctx context.Context, pool *pgxpool.Pool, discordID string) (bool, error) {
	tag, err := pool.Exec(ctx, removeFromAllowlistSQL, discordID)
	if err != nil {
		return false, fmt.Errorf("remove from allowlist: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

const listAllowlistSQL = `
SELECT a.discord_id, a.note, a.added_at
FROM signup_allowlist a
ORDER BY a.added_at
`

// ListAllowlist returns everyone cleared to sign up, oldest first.
func ListAllowlist(ctx context.Context, pool *pgxpool.Pool) ([]AllowlistEntry, error) {
	rows, err := pool.Query(ctx, listAllowlistSQL)
	if err != nil {
		return nil, fmt.Errorf("list allowlist: %w", err)
	}
	defer rows.Close()

	var out []AllowlistEntry
	for rows.Next() {
		var e AllowlistEntry
		if err := rows.Scan(&e.DiscordID, &e.Note, &e.AddedAt); err != nil {
			return nil, fmt.Errorf("scan allowlist: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// nullable stores SQL NULL for an empty string, so "they have no avatar" and
// "they have an avatar called empty string" don't become the same row.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
