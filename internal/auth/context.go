package auth

import "context"

// contextKey is an unexported type so values stored under it can't be
// retrieved (or overwritten) by anyone outside this package.
type contextKey struct{ name string }

var userIDContextKey = &contextKey{"user-id"}

func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDContextKey, id)
}

// UserIDFromContext returns the authenticated user id placed by RequireAuth.
// The boolean is false when the request did not pass through the middleware
// (e.g. a public endpoint or a test calling the handler directly).
func UserIDFromContext(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(userIDContextKey).(string)
	return s, ok && s != ""
}
