// Package admin holds the operator ROLE: who is an admin, and the HTTP gate
// that admin-only routes hang off.
//
// Until this existed, "operator" meant "whoever holds the server console" —
// privilege by physical access. That works for a box you sit at, but it makes
// every operator action unreachable from the client, which is why publishing a
// node meant copying a bundle to the server and typing at its prompt. The role
// is what lets those same actions be driven over the API.
//
// The root of trust stays the console: `admin <user>` grants it, and the
// ADMIN_USERNAME env var promotes an account at boot for a hosted deploy where
// attaching to the console is awkward. There is deliberately NO endpoint that
// grants admin — an HTTP path to self-promotion is exactly the thing you don't
// want, so the only ways in are at the machine or in its config.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// errForbidden is the client-visible code for "authenticated, but not an
// admin". Distinct from `unauthorized` (401) so the client can tell "log in"
// from "you're logged in, but this isn't yours to do".
const errForbidden = "forbidden"

// dbExecutor is the subset of pgx methods the queries need; both *pgxpool.Pool
// and pgx.Tx satisfy it (matching the other domains).
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IsAdmin reports whether a user id carries the admin flag. A missing user is
// not an error — it's simply not an admin.
func IsAdmin(ctx context.Context, db dbExecutor, userID string) (bool, error) {
	var isAdmin bool
	err := db.QueryRow(ctx, `SELECT is_admin FROM users WHERE id = $1`, userID).Scan(&isAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up admin: %w", err)
	}
	return isAdmin, nil
}

// Set grants or revokes admin by username OR id, and reports the resolved
// username. Used by the console commands and the boot-time env bootstrap.
func Set(ctx context.Context, db dbExecutor, needle string, isAdmin bool) (string, error) {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return "", errors.New("which user? give a username or id")
	}
	var id, username string
	err := db.QueryRow(ctx,
		`SELECT id, username FROM users WHERE username = $1 OR id = $1`, needle).Scan(&id, &username)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("no user %q", needle)
	}
	if err != nil {
		return "", fmt.Errorf("look up user: %w", err)
	}
	if _, err := db.Exec(ctx, `UPDATE users SET is_admin = $2 WHERE id = $1`, id, isAdmin); err != nil {
		return "", fmt.Errorf("set admin: %w", err)
	}
	return username, nil
}

// Bootstrap promotes the ADMIN_USERNAME account at boot. Absent config is a
// no-op, and an unknown username is a WARNING rather than a fatal: a server
// must still start when the named account hasn't registered yet (the usual
// order on a fresh deploy is "server up, then sign up").
func Bootstrap(ctx context.Context, db dbExecutor, username string) {
	if strings.TrimSpace(username) == "" {
		return
	}
	resolved, err := Set(ctx, db, username, true)
	if err != nil {
		logx.Warn("admin bootstrap: %v (set ADMIN_USERNAME to an account that exists, or use the console's `admin` command)", err)
		return
	}
	logx.Info("admin bootstrap: %s is an admin", resolved)
}

// RequireAdmin gates a route on the admin flag. It runs AFTER RequireAuth —
// it reads the user id that middleware put in the context, so mounting it
// without RequireAdmin's partner is a 401, not an accidental open door.
func RequireAdmin(db dbExecutor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := auth.UserIDFromContext(r.Context())
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			isAdmin, err := IsAdmin(r.Context(), db, userID)
			if err != nil {
				logx.Error("admin check: %v", err)
				httpx.WriteError(w, http.StatusInternalServerError, "internal_error")
				return
			}
			if !isAdmin {
				httpx.WriteError(w, http.StatusForbidden, errForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(r.Context()))
		})
	}
}
