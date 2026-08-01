package apitokens

import (
	"context"
	"net/http"

	"github.com/tabuhana/bombers-server/internal/httpx"
)

// The scope gate.
//
// A request arrives carrying either a session (a person at a client) or an API
// token (a script, a mini-client, an agent). A SESSION is unscoped: it can do
// whatever its owner can, because it IS its owner sitting there. A TOKEN carries
// exactly the scopes it was granted.
//
// So `RequireScope` asks one question — may this request do this? — and a
// request with no token in its context answers yes, because it got past
// RequireAuth as a session. That default is safe only because the ONLY way to
// put scopes in the context is to present a token, which the auth middleware
// does before any route runs.

type contextKey struct{ name string }

var holderKey = &contextKey{"api-token-holder"}

const (
	errForbiddenScope = "insufficient_scope"
	errSessionOnly    = "session_required"
)

// WithHolder marks a request as authenticated BY A TOKEN, and with what.
func WithHolder(ctx context.Context, h Holder) context.Context {
	return context.WithValue(ctx, holderKey, h)
}

// HolderFromContext returns the token behind this request. False means a
// session, not an absence of authorization.
func HolderFromContext(ctx context.Context) (Holder, bool) {
	h, ok := ctx.Value(holderKey).(Holder)
	return h, ok
}

// SessionOnly refuses API tokens outright, whatever they hold.
//
// For the routes that MANAGE tokens. A token that could mint another token
// would make every scope advisory: hand something a narrow credential and it
// issues itself a wide one. A token that could revoke would let a leaked
// credential lock its owner out of the only lever they have. Both are the same
// mistake — privilege that can extend itself — so there is deliberately no
// scope for this. You need the password.
func SessionOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, viaToken := HolderFromContext(r.Context()); viaToken {
			httpx.WriteError(w, http.StatusForbidden, errSessionOnly)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireScope gates a route on one scope. Sessions pass; tokens must hold it.
//
// 403 with `insufficient_scope`, deliberately NOT 401: the credential is valid
// and the caller should not go and get a new one — they should mint a token with
// a broader grant, or accept the answer.
func RequireScope(scope Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			holder, viaToken := HolderFromContext(r.Context())
			if viaToken && !holder.Scopes.Has(scope) {
				httpx.WriteError(w, http.StatusForbidden, errForbiddenScope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
