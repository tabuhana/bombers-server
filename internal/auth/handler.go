package auth

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/tabuhana/bombers-server/internal/httpx"
)

const (
	refreshBodyLimit = 4 << 10

	// errUnauthorized (shared with middleware.go) — same generic envelope
	// for every refresh failure: expired, unknown, reused, parse error.
	// Reuse detection is logged server-side but never surfaced to the client.
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse is the success body for POST /auth/refresh. Fields match
// the token-bearing portion of users.loginResponse — clients can decode both
// responses with the same struct (the User field on login is the only delta).
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, refreshBodyLimit)

	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.RefreshToken == "" {
		httpx.WriteError(w, http.StatusUnauthorized, errUnauthorized)
		return
	}

	pair, err := h.svc.Rotate(r.Context(), req.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidRefresh), errors.Is(err, ErrRefreshReused):
			// Both collapse to the same 401. Reuse is already logged in
			// Service.Rotate; nothing more to say here.
			httpx.WriteError(w, http.StatusUnauthorized, errUnauthorized)
		default:
			log.Printf("auth: rotate refresh token: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not refresh tokens")
		}
		return
	}

	httpx.WriteJSON(w, http.StatusOK, refreshResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresIn:    pair.ExpiresIn,
	})
}
