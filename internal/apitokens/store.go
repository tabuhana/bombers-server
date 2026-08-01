package apitokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Prefix marks a secret as an API token rather than a session JWT. The auth
// middleware branches on it, and it also makes a leaked token recognisable in a
// log or a paste — the thing secret scanners look for.
const Prefix = "bmb_"

// secretBytes is the entropy behind the random half. 32 bytes is well past
// brute force and keeps the printed token a manageable length.
const secretBytes = 32

// dbExecutor is the subset of pgx both *pgxpool.Pool and pgx.Tx satisfy,
// matching the other domains.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Token is one credential as its owner sees it. The secret is NOT here: it
// exists exactly once, in the response to the call that created it.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Holder is who a presented token belongs to and what it may do.
type Holder struct {
	TokenID string
	UserID  string
	Scopes  Set
}

// ErrNotFound is a token that doesn't exist, is revoked, or has expired — one
// error for all three, because the caller's next move is identical and the
// difference is not the presenter's business.
var ErrNotFound = errors.New("api token not found")

// generate mints a secret and its hash. The secret is returned once and never
// stored; only the hash goes to the database, so a copy of the table cannot be
// turned back into working credentials.
func generate() (secret string, hash string, err error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	secret = Prefix + base64.RawURLEncoding.EncodeToString(raw)
	return secret, hashSecret(secret), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Create mints a token for a user. Returns the row and the plaintext secret,
// which the caller must hand back immediately and then forget.
func Create(ctx context.Context, db dbExecutor, id, userID, name string, scopes []string, expiresAt *time.Time) (Token, string, error) {
	secret, hash, err := generate()
	if err != nil {
		return Token{}, "", err
	}

	const q = `
        INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6)
        RETURNING created_at`
	var createdAt time.Time
	if err := db.QueryRow(ctx, q, id, userID, name, hash, scopes, expiresAt).Scan(&createdAt); err != nil {
		return Token{}, "", err
	}
	return Token{ID: id, Name: name, Scopes: scopes, CreatedAt: createdAt, ExpiresAt: expiresAt}, secret, nil
}

// List returns a user's live tokens, newest first. Revoked ones are gone from
// this view — the row survives for the record, but a list of credentials should
// only show credentials that work.
func List(ctx context.Context, db dbExecutor, userID string) ([]Token, error) {
	const q = `
        SELECT id, name, scopes, created_at, expires_at, last_used_at
        FROM api_tokens
        WHERE user_id = $1 AND revoked_at IS NULL
        ORDER BY created_at DESC`
	rows, err := db.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke marks a token dead. Scoped to the owner, so one user cannot revoke
// another's by guessing an id. Reports whether anything changed, so a repeat
// call is a no-op rather than an error.
func Revoke(ctx context.Context, db dbExecutor, userID, id string) (bool, error) {
	const q = `UPDATE api_tokens SET revoked_at = now()
               WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`
	tag, err := db.Exec(ctx, q, id, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Resolve turns a presented secret into its holder, or ErrNotFound.
//
// Expiry and revocation are checked in the QUERY rather than after it: a token
// that has expired must never be resolvable, and a condition applied in Go is a
// condition somebody can forget to apply. `last_used_at` is stamped in the same
// statement so a lookup is one round trip.
func Resolve(ctx context.Context, db dbExecutor, secret string) (Holder, error) {
	if !strings.HasPrefix(secret, Prefix) {
		return Holder{}, ErrNotFound
	}
	const q = `
        UPDATE api_tokens SET last_used_at = now()
        WHERE token_hash = $1
          AND revoked_at IS NULL
          AND (expires_at IS NULL OR expires_at > now())
        RETURNING id, user_id, scopes`
	var h Holder
	var scopes []string
	err := db.QueryRow(ctx, q, hashSecret(secret)).Scan(&h.TokenID, &h.UserID, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return Holder{}, ErrNotFound
	}
	if err != nil {
		return Holder{}, err
	}
	h.Scopes = NewSet(scopes)
	return h, nil
}
