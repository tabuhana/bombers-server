package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
	"golang.org/x/crypto/bcrypt"

	"github.com/tabuhana/bombers-server/internal/httpx"
)

const (
	registerBodyLimit     = 4 << 10 // 4 KiB is plenty for {username, password}
	friendCodeMaxAttempts = 5
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, registerBodyLimit)

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	username, err := validateUsername(req.Username)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePassword(req.Password); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("users: bcrypt hash: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not hash password")
		return
	}

	user, err := h.createUser(r.Context(), username, string(hash))
	if err != nil {
		switch {
		case errors.Is(err, ErrUsernameTaken):
			httpx.WriteError(w, http.StatusConflict, "username already taken")
		default:
			log.Printf("users: create user: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not create user")
		}
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, user.Public())
}

// createUser assembles the row and inserts it, regenerating the friend code
// on UNIQUE collision. The username canonical collision is surfaced directly
// (no retry — the user picked a taken name).
func (h *Handler) createUser(ctx context.Context, username, passwordHash string) (*User, error) {
	id := ulid.Make().String()
	canonical := CanonicalUsername(username)

	for attempt := 0; attempt < friendCodeMaxAttempts; attempt++ {
		code, err := GenerateFriendCode()
		if err != nil {
			return nil, err
		}
		u := &User{
			ID:                id,
			Username:          username,
			UsernameCanonical: canonical,
			PasswordHash:      passwordHash,
			FriendCode:        code,
		}
		err = insertUser(ctx, h.pool, u)
		if err == nil {
			return u, nil
		}
		if errors.Is(err, errFriendCodeTaken) {
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("could not generate unique friend code after %d attempts", friendCodeMaxAttempts)
}
