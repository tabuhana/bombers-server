package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors returned by Service. Both collapse to a single 401 at the HTTP
// boundary — the distinction exists only for server-side logging.
var (
	// ErrInvalidRefresh covers: bad signature/parse, expired (either at JWT
	// or DB level), unknown jti, wrong token type.
	ErrInvalidRefresh = errors.New("invalid refresh token")

	// ErrRefreshReused fires when an already-rotated refresh token is presented
	// again. Service responds by invalidating every still-valid refresh token
	// for the user (presumed-theft posture). The caller MUST NOT reveal this
	// distinction to clients.
	ErrRefreshReused = errors.New("refresh token reuse detected")
)

// TokenPair is the payload Service hands back to login and refresh handlers.
// ExpiresIn is the access-token lifetime in seconds (the refresh-token
// expiry is intentionally NOT exposed — the client retries via /auth/refresh
// when it sees an access-token failure).
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// Service owns the persistent half of the auth model: issuing token pairs
// that are recorded for rotation tracking and rotating refresh tokens with
// reuse detection. The stateless half (signing/verifying JWTs, middleware)
// lives on Issuer.
type Service struct {
	issuer *Issuer
	pool   *pgxpool.Pool
}

func NewService(issuer *Issuer, pool *pgxpool.Pool) *Service {
	return &Service{issuer: issuer, pool: pool}
}

func (s *Service) Issuer() *Issuer { return s.issuer }

// IssueInitialPair mints a fresh access+refresh pair for userID and persists
// the refresh token. Used by /auth/login (and registration, if it ever issues
// tokens). The caller is responsible for having authenticated userID — this
// function does not verify credentials.
func (s *Service) IssueInitialPair(ctx context.Context, userID string) (TokenPair, error) {
	access, _, err := s.issuer.IssueAccessToken(userID)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, jti, refreshExp, err := s.issuer.IssueRefreshToken(userID)
	if err != nil {
		return TokenPair{}, err
	}
	if err := insertRefreshToken(ctx, s.pool, jti, userID, refreshExp); err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.issuer.AccessTTL().Seconds()),
	}, nil
}

// Rotate consumes one refresh token and issues a fresh pair atomically.
// The SELECT … FOR UPDATE row lock blocks a concurrent rotation on the same
// token until the first commits, at which point the loser sees used_at
// populated and triggers reuse detection.
//
// Tx management is inlined (not via a generic helper) because the
// reuse-detection branch MUST commit the cascade-kill UPDATE before
// returning an error — a wrapper that rolls back on returned error would
// silently undo the kill and degrade option B to option A.
//
// Returns ErrRefreshReused when the presented token was already rotated
// (and has, as a side effect, invalidated every other valid token for the
// user). Returns ErrInvalidRefresh for parse/signature/expiry/unknown-jti.
func (s *Service) Rotate(ctx context.Context, rawRefresh string) (TokenPair, error) {
	claims, err := s.issuer.ParseRefreshToken(rawRefresh)
	if err != nil {
		return TokenPair{}, ErrInvalidRefresh
	}
	if claims.JTI == "" {
		// Refresh tokens issued by this server always carry jti; a missing
		// jti means either a corrupt token or a refresh token from before
		// jti was introduced. Treat as invalid.
		return TokenPair{}, ErrInvalidRefresh
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenPair{}, fmt.Errorf("begin tx: %w", err)
	}
	// Safety net for any path that returns without an explicit commit.
	// Rollback is a no-op once Commit has succeeded.
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := getRefreshTokenForUpdate(ctx, tx, claims.JTI)
	if err != nil {
		return TokenPair{}, err
	}

	// Sanity: the JWT's subject and the stored user_id must agree. They
	// will, unless the token was forged with a valid signature (impossible
	// without the secret) or the DB was tampered with. Defense in depth.
	if row.UserID != claims.UserID {
		return TokenPair{}, ErrInvalidRefresh
	}

	if row.UsedAt != nil {
		// REUSE DETECTED. Kill every still-valid refresh token for this
		// user, then EXPLICITLY COMMIT so the kill persists — the deferred
		// Rollback above must not undo this.
		if err := invalidateAllUserRefreshTokens(ctx, tx, row.UserID); err != nil {
			log.Printf("auth: reuse-detection invalidate failed for user %s: %v", row.UserID, err)
			return TokenPair{}, ErrRefreshReused
		}
		if err := tx.Commit(ctx); err != nil {
			log.Printf("auth: reuse-detection commit failed for user %s: %v", row.UserID, err)
			return TokenPair{}, ErrRefreshReused
		}
		log.Printf("auth: refresh-token reuse detected for user %s; all tokens invalidated", row.UserID)
		return TokenPair{}, ErrRefreshReused
	}

	if time.Now().After(row.ExpiresAt) {
		return TokenPair{}, ErrInvalidRefresh
	}

	// Happy path: mark old used, issue+persist new pair, commit.
	if err := markRefreshTokenUsed(ctx, tx, claims.JTI); err != nil {
		return TokenPair{}, err
	}
	access, _, err := s.issuer.IssueAccessToken(row.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, newJTI, refreshExp, err := s.issuer.IssueRefreshToken(row.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	if err := insertRefreshToken(ctx, tx, newJTI, row.UserID, refreshExp); err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, fmt.Errorf("commit tx: %w", err)
	}

	return TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.issuer.AccessTTL().Seconds()),
	}, nil
}
