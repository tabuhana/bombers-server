package auth

import (
	"net/http"
	"strings"

	"github.com/tabuhana/bombers-server/internal/httpx"
)

// errUnauthorized is the single client-visible error code for every
// auth-middleware rejection: no header, wrong scheme, bad signature,
// wrong algorithm, expired, wrong token type. Discriminating leaks
// information about why the token failed.
const errUnauthorized = "unauthorized"

// RequireAuth is a chi-compatible middleware (func(http.Handler) http.Handler).
// On success it injects the authenticated user id into the request context;
// on any failure it writes 401 with the shared error envelope and does NOT
// call next.
func (i *Issuer) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, errUnauthorized)
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
