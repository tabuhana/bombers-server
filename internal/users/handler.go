package users

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	// Bodies here are a code or a ticket and a name — 4 KiB is generous.
	loginBodyLimit        = 4 << 10
	friendCodeMaxAttempts = 5
)

type Handler struct {
	pool *pgxpool.Pool
	auth *auth.Service
}

func NewHandler(pool *pgxpool.Pool, authSvc *auth.Service) *Handler {
	return &Handler{pool: pool, auth: authSvc}
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := getUserByID(r.Context(), h.pool, id)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// Valid-signature token for a deleted user — still 401.
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		logx.Error("users: get by id: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch user")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u.Public())
}
