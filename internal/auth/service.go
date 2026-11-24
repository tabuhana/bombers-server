package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/logx"
)

// Errors returned by Service. Both collapse to a single 401 at the HTTP
// boundary — the distinction exists only for server-side logging.
var (
	// ErrInvalidRefresh covers: bad signature/parse, expired (either at JWT
	// or DB level), unknown jti, wrong token type.
	ErrInvalidRefresh = errors.New("invalid refresh token")

	// ErrRefreshReused fires when an already-rotated refresh token is presented
	// again OUTSIDE the lost-answer grace (see LostAnswerWindow and
	// decideRotation): later than the window, or after its replacement was
	// used or revoked. Service responds by invalidating every still-valid
	// refresh token for the user (presumed-theft posture). The caller MUST NOT
	// reveal this distinction to clients.
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
// populated and is judged by decideRotation.
//
// A used token presented again is theft EXCEPT inside a short grace: within
// LostAnswerWindow of its use, while its replacement has not been used or
// revoked, it is a lost reply. Rotate then answers with a fresh pair and
// revokes the replacement nobody received. Once the real app has used its
// replacement, or the window has passed, a retry is reuse again.
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

	// A banned account cannot renew. Access tokens are short-lived, so refusing
	// here is what makes a ban actually take hold — within one token lifetime,
	// without a database read on every authed request. A narrow query rather
	// than importing the users domain, the same tradeoff every domain makes.
	var banned bool
	if err := tx.QueryRow(ctx,
		`SELECT banned_at IS NOT NULL FROM users WHERE id = $1`, row.UserID).Scan(&banned); err == nil && banned {
		return TokenPair{}, ErrInvalidRefresh
	}

	// A used token may be a lost answer, which depends on whether its
	// replacement has been used, so lock that row too. Lock order is always
	// older -> newer along the replacement chain, so two rotations touching the
	// same chain take their locks in the same order and cannot deadlock.
	var replacement *refreshRow
	if row.UsedAt != nil && row.ReplacedBy != nil {
		rep, err := getRefreshTokenForUpdate(ctx, tx, *row.ReplacedBy)
		switch {
		case err == nil:
			replacement = rep
		case errors.Is(err, ErrInvalidRefresh):
			replacement = nil
		default:
			return TokenPair{}, err
		}
	}

	var repState *tokenState
	if replacement != nil {
		repState = &tokenState{
			ExpiresAt:  replacement.ExpiresAt,
			UsedAt:     replacement.UsedAt,
			RevokedAt:  replacement.RevokedAt,
			ReplacedBy: replacement.ReplacedBy,
		}
	}
	verdict := decideRotation(tokenState{
		ExpiresAt:  row.ExpiresAt,
		UsedAt:     row.UsedAt,
		RevokedAt:  row.RevokedAt,
		ReplacedBy: row.ReplacedBy,
	}, repState, time.Now())

	switch verdict {
	case verdictInvalid:
		return TokenPair{}, ErrInvalidRefresh

	case verdictReuse:
		// REUSE DETECTED. Kill every still-valid refresh token for this
		// user, then EXPLICITLY COMMIT so the kill persists — the deferred
		// Rollback above must not undo this.
		if err := invalidateAllUserRefreshTokens(ctx, tx, row.UserID); err != nil {
			logx.Error("auth: reuse-detection invalidate failed for user %s: %v", row.UserID, err)
			return TokenPair{}, ErrRefreshReused
		}
		if err := tx.Commit(ctx); err != nil {
			logx.Error("auth: reuse-detection commit failed for user %s: %v", row.UserID, err)
			return TokenPair{}, ErrRefreshReused
		}
		logx.Warn("auth: refresh-token reuse detected for user %s; all tokens invalidated", row.UserID)
		return TokenPair{}, ErrRefreshReused

	case verdictRotate:
		// Insert the new row FIRST: replaced_by is a foreign key to it.
		pair, newJTI, err := s.issueAndStore(ctx, tx, row.UserID)
		if err != nil {
			return TokenPair{}, err
		}
		if err := markRefreshTokenUsed(ctx, tx, claims.JTI, newJTI); err != nil {
			return TokenPair{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenPair{}, fmt.Errorf("commit tx: %w", err)
		}
		return pair, nil

	case verdictReissue:
		// LOST ANSWER. The pair we rotated to never arrived, so cancel it and
		// hand out a fresh one. used_at stays as it was: the window is measured
		// from the original use and a retry never extends it.
		pair, newJTI, err := s.issueAndStore(ctx, tx, row.UserID)
		if err != nil {
			return TokenPair{}, err
		}
		if err := revokeRefreshToken(ctx, tx, *row.ReplacedBy); err != nil {
			return TokenPair{}, err
		}
		if err := setReplacedBy(ctx, tx, claims.JTI, newJTI); err != nil {
			return TokenPair{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenPair{}, fmt.Errorf("commit tx: %w", err)
		}
		logx.Info("auth: lost answer for user %s; re-issued and cancelled the unused replacement", row.UserID)
		return pair, nil
	}

	return TokenPair{}, ErrInvalidRefresh
}

// issueAndStore mints an access+refresh pair for userID and inserts the new
// refresh row inside tx. It returns the new refresh token's jti so the caller
// can link the old token to it.
func (s *Service) issueAndStore(ctx context.Context, tx pgx.Tx, userID string) (TokenPair, string, error) {
	access, _, err := s.issuer.IssueAccessToken(userID)
	if err != nil {
		return TokenPair{}, "", err
	}
	refresh, newJTI, refreshExp, err := s.issuer.IssueRefreshToken(userID)
	if err != nil {
		return TokenPair{}, "", err
	}
	if err := insertRefreshToken(ctx, tx, newJTI, userID, refreshExp); err != nil {
		return TokenPair{}, "", err
	}
	return TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.issuer.AccessTTL().Seconds()),
	}, newJTI, nil
}
