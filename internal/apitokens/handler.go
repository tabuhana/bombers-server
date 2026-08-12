package apitokens

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	errInvalidBody  = "invalid_body"
	errInvalidName  = "invalid_token_name"
	errInvalidScope = "invalid_scope"
	errTooManyScope = "no_scopes"
	errNotFound     = "token_not_found"
)

// maxTokens caps how many live tokens one account may hold. Not a resource
// limit — a list you can read at a glance is a list you'll actually audit.
const maxTokens = 20

// maxNameLen keeps a name to something a person wrote, not a payload.
const maxNameLen = 60

type Handler struct{ pool *pgxpool.Pool }

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// ResolveAPIToken implements auth.TokenResolver: it turns a presented secret
// into a user id, plus a function that stamps the request with the token's
// scopes so `RequireScope` can read them downstream.
func (h *Handler) ResolveAPIToken(
	ctx context.Context,
	secret string,
) (string, func(*http.Request) *http.Request, error) {
	holder, err := Resolve(ctx, h.pool, secret)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			logx.Error("apitokens: resolve: %v", err)
		}
		return "", nil, err
	}
	return holder.UserID, func(r *http.Request) *http.Request {
		return r.WithContext(WithHolder(r.Context(), holder))
	}, nil
}

// List → GET /me/tokens
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFromContext(r.Context())
	tokens, err := List(r.Context(), h.pool, userID)
	if err != nil {
		logx.Error("apitokens: list: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list tokens")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// Create → POST /me/tokens
//
// The ONE time the secret exists outside the caller: it's returned here and
// never again, because only its hash is stored. That's deliberate — a server
// that can show you a token is a server that can leak every token.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFromContext(r.Context())

	var body struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresIn *int     `json:"expires_in_days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > maxNameLen {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidName)
		return
	}

	// Every scope must be one the server knows. An unrecognised scope is a
	// caller who is WRONG about what their token can do — storing it and
	// ignoring it would let them find that out in production.
	scopes := make([]string, 0, len(body.Scopes))
	for _, s := range body.Scopes {
		s = strings.TrimSpace(s)
		if !Valid(s) {
			httpx.WriteError(w, http.StatusBadRequest, errInvalidScope)
			return
		}
		if s != string(ProfileRead) {
			scopes = append(scopes, s)
		}
	}
	// A token granting nothing can still read `/me`, which is almost certainly
	// not what someone meant to create.
	if len(scopes) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyScope)
		return
	}

	existing, err := List(r.Context(), h.pool, userID)
	if err != nil {
		logx.Error("apitokens: count: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not create token")
		return
	}
	if len(existing) >= maxTokens {
		httpx.WriteError(w, http.StatusBadRequest, "too_many_tokens")
		return
	}

	var expiresAt *time.Time
	if body.ExpiresIn != nil && *body.ExpiresIn > 0 {
		t := time.Now().AddDate(0, 0, *body.ExpiresIn)
		expiresAt = &t
	}

	token, secret, err := Create(r.Context(), h.pool, ulid.Make().String(), userID, name, scopes, expiresAt)
	if err != nil {
		logx.Error("apitokens: create: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not create token")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"token": token,
		// Named `secret` rather than `token` so nobody mistakes the object above
		// for the credential.
		"secret":  secret,
		"warning": "This is the only time this secret is shown. Store it now.",
	})
}

// Revoke → DELETE /me/tokens/{id}
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	removed, err := Revoke(r.Context(), h.pool, userID, id)
	if err != nil {
		logx.Error("apitokens: revoke: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not revoke token")
		return
	}
	if !removed {
		// Scoped to the caller, so this covers "no such token" and "not yours"
		// with one answer — the second must not be distinguishable.
		httpx.WriteError(w, http.StatusNotFound, errNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Scopes → GET /token-scopes : what a client can offer when minting one.
// Unauthenticated on purpose: it's a constant, and a UI needs it before it has
// anywhere to put a token.
func (h *Handler) Scopes(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, len(All))
	for _, s := range All {
		out = append(out, map[string]any{
			"scope":     string(s),
			"describes": describe(s),
			// So a client can hide (or mark) what this user could never use.
			// The list itself stays a constant — the endpoint is
			// unauthenticated and doesn't know who's asking — and the client
			// already knows whether its user is an admin.
			"requires_admin": RequiresAdmin(s),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"scopes": out})
}

// describe is the plain-English half of a scope, for a consent screen. A
// permission nobody can read is a permission everybody grants.
func describe(s Scope) string {
	switch s {
	case NotesRead:
		return "Read the notes you've published to this server"
	case NotesWrite:
		return "Add and change notes on this server"
	case PeopleRead:
		return "Read the cards you keep about people"
	case FriendsRead:
		return "See who you're friends with"
	case MessagesRead:
		return "Read your direct messages"
	case MessagesWrite:
		return "Send direct messages as you"
	case StoreRead:
		return "Browse and download this server's nodes, packs and games"
	case StoreWrite:
		return "Publish to this server's stores (still requires an admin account)"
	default:
		return "Know who you are"
	}
}
