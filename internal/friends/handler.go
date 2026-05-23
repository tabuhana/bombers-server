// Package friends owns the friend graph: friend codes, friend requests,
// the friendship relation. The first endpoint is the warm-up — returning
// the caller's own friend code so the share flow has something to read.
package friends

import (
	"errors"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// myCodeResponse is the success body for GET /friends/code.
type myCodeResponse struct {
	FriendCode string `json:"friend_code"`
}

// MyCode returns the authenticated user's own friend code. Mounted behind
// auth.RequireAuth, so the user id is always present in context; absence is
// a programming error (wrong mount) — we fail closed with 401.
func (h *Handler) MyCode(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	code, err := getOwnFriendCode(r.Context(), h.pool, id)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// Valid-signature token for a deleted user — still 401.
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		log.Printf("friends: get own friend code: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch friend code")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, myCodeResponse{FriendCode: code})
}
