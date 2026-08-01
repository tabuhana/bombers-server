// Package auth issues and validates the JWTs used by HTTP and (later)
// WebSocket endpoints. The Issuer is the single owner of signing keys
// and token lifetimes — other domains depend on it but do not duplicate
// claim shapes or signing logic.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oklog/ulid/v2"
)

// Per SERVER.md "Auth model": short access token + long rotating refresh
// token. Rotation/persistence lives in Service (see service.go); this file
// owns token shape, signing, and verification.
const (
	DefaultAccessTTL  = 15 * time.Minute
	DefaultRefreshTTL = 30 * 24 * time.Hour

	tokenTypeAccess  = "access"
	tokenTypeRefresh = "refresh"
)

var (
	// ErrInvalidToken covers all "this token cannot be trusted" cases:
	// bad signature, wrong algorithm, malformed, expired. Callers MUST
	// collapse these to a single 401 — discriminating leaks information.
	ErrInvalidToken = errors.New("invalid token")

	// ErrWrongTokenType is returned when an access-token check sees a
	// refresh token (or vice versa). Treated as invalid by callers.
	ErrWrongTokenType = errors.New("wrong token type")
)

// Claims is the minimal validated claim set we expose to callers.
// Keep this small — anything added here becomes a contract for every
// consumer of ParseAccessToken / ParseRefreshToken.
type Claims struct {
	UserID    string
	TokenType string
	JTI       string // populated for refresh tokens; empty for access tokens
	ExpiresAt time.Time
}

type Issuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	// Set at wiring time so RequireAuth can also accept API tokens. Nil on a
	// server built without that domain, which then accepts sessions only.
	tokens TokenResolver
}

// NewIssuer constructs an Issuer that signs with the given secret (raw
// bytes — the secret is passed through unmodified). Uses HS256.
func NewIssuer(secret string) *Issuer {
	return &Issuer{
		secret:     []byte(secret),
		accessTTL:  DefaultAccessTTL,
		refreshTTL: DefaultRefreshTTL,
	}
}

func (i *Issuer) AccessTTL() time.Duration  { return i.accessTTL }
func (i *Issuer) RefreshTTL() time.Duration { return i.refreshTTL }

// IssueAccessToken returns a signed access JWT for userID along with its
// absolute expiry time. Access tokens are stateless — no jti, no DB row.
func (i *Issuer) IssueAccessToken(userID string) (string, time.Time, error) {
	signed, exp, err := i.signClaims(userID, tokenTypeAccess, i.accessTTL, "")
	return signed, exp, err
}

// IssueRefreshToken returns a signed refresh JWT for userID, the freshly
// generated jti embedded in it, and the token's expiry. The jti is what
// Service stores in the refresh_tokens table for rotation tracking.
func (i *Issuer) IssueRefreshToken(userID string) (token string, jti string, exp time.Time, err error) {
	jti = ulid.Make().String()
	token, exp, err = i.signClaims(userID, tokenTypeRefresh, i.refreshTTL, jti)
	return token, jti, exp, err
}

func (i *Issuer) signClaims(userID, typ string, ttl time.Duration, jti string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	claims := jwt.MapClaims{
		"sub": userID,
		"typ": typ,
		"iat": now.Unix(),
		"exp": exp.Unix(),
	}
	if jti != "" {
		claims["jti"] = jti
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := t.SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign %s token: %w", typ, err)
	}
	return signed, exp, nil
}

func (i *Issuer) ParseAccessToken(s string) (*Claims, error) {
	return i.parse(s, tokenTypeAccess)
}

func (i *Issuer) ParseRefreshToken(s string) (*Claims, error) {
	return i.parse(s, tokenTypeRefresh)
}

func (i *Issuer) parse(raw, expectedType string) (*Claims, error) {
	parsed, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
		}
		return i.secret, nil
	})
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalidToken
	}
	sub, _ := claims["sub"].(string)
	typ, _ := claims["typ"].(string)
	if sub == "" {
		return nil, ErrInvalidToken
	}
	if typ != expectedType {
		return nil, ErrWrongTokenType
	}
	expFloat, _ := claims["exp"].(float64)
	jti, _ := claims["jti"].(string)
	return &Claims{
		UserID:    sub,
		TokenType: typ,
		JTI:       jti,
		ExpiresAt: time.Unix(int64(expFloat), 0),
	}, nil
}
