package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/tabuhana/bombers-server/internal/httpx"
)

// errUnauthorized is the single client-visible error code for every
// auth-middleware rejection: no header, wrong scheme, bad signature,
// wrong algorithm, expired, wrong token type. Discriminating leaks
// information about why the token failed.
const errUnauthorized = "unauthorized"

// TokenResolver turns an API-token secret into its owner and scopes. The
// apitokens package implements it; auth takes it as a seam so the dependency
// points one way (apitokens → auth, never back).
type TokenResolver interface {
	ResolveAPIToken(ctx context.Context, secret string) (userID string, apply func(*http.Request) *http.Request, err error)
}

// SetTokenResolver teaches RequireAuth to accept API tokens as well as session
// JWTs. Called once at wiring time. Left nil, only sessions are accepted — which
// is the correct behaviour for a server built without the tokens domain.
func (i *Issuer) SetTokenResolver(r TokenResolver) { i.tokens = r }

// RequireAuth is a chi-compatible middleware (func(http.Handler) http.Handler).
// On success it injects the authenticated user id into the request context;
// on any failure it writes 401 with the shared error envelope and does NOT
// call next.
//
// It accepts TWO kinds of credential on the same header, because a route should
// not care which one arrived:
//
//   - a session JWT — a person at a client, unscoped, 15 minutes long;
//   - an API token (`bmb_…`) — a script, a mini-client, an agent, carrying
//     exactly the scopes it was granted.
//
// The prefix decides, so a JWT is never hashed against the token table and a
// token is never handed to the JWT parser. Whichever it is, downstream sees the
// same authenticated user id; only `apitokens.RequireScope` can tell them apart,
// and only where the difference matters.
func (i *Issuer) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}

		if i.tokens != nil && strings.HasPrefix(tokenStr, APITokenPrefix) {
			userID, apply, err := i.tokens.ResolveAPIToken(r.Context(), tokenStr)
			if err != nil {
				httpx.WriteError(w, http.StatusUnauthorized, errUnauthorized)
				return
			}
			r = apply(r)
			next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), userID)))
			return
		}

		claims, err := i.ParseAccessToken(tokenStr)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		ctx := WithUserID(r.Context(), claims.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// APITokenPrefix mirrors apitokens.Prefix. Duplicated as a constant rather than
// imported so the dependency doesn't reverse (apitokens → auth, never back).
// Exported only so the apitokens tests can assert the two never drift: if they
// did, every API token would be handed to the JWT parser and every agent would
// be silently locked out.
const APITokenPrefix = "bmb_"

// bearerToken extracts the token portion of an `Authorization: Bearer <token>`
// header. The scheme match is case-insensitive per RFC 7235.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) {
		return "", false
	}
	if !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
