package presence

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// Two endpoints, and only friends can see anything. There is no way to ask
// whether a stranger is online — that would be a presence directory, and this
// server has no directory of any kind on purpose.

const errInvalidStatus = "invalid_status"

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

type entry struct {
	UserID string `json:"user_id"`
	Status string `json:"status"`
}

// SetMine → PUT /me/presence : say what you are, and prove you're still here.
//
// One endpoint for both because they can't be allowed to disagree. A client on a
// timer sends this whether or not anything changed; the status is what you chose
// and the write itself is the heartbeat.
func (h *Handler) SetMine(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	status := strings.TrimSpace(req.Status)
	if !Valid(status) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidStatus)
		return
	}

	rec, err := Set(r.Context(), h.pool, userID, Status(status))
	if err != nil {
		logx.Error("presence: set: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not set presence")
		return
	}
	// Echo the EFFECTIVE status, not the stored one. They differ in exactly one
	// interesting case — you just set yourself offline — and a client that
	// rendered the stored value would show you as online for a moment.
	httpx.WriteJSON(w, http.StatusOK, entry{
		UserID: userID,
		Status: string(Effective(rec.Status, rec.UpdatedAt, time.Now())),
	})
}

// ListFriends → GET /presence : every accepted friend and what they are.
//
// One request for everyone, because the alternative is a client that makes N
// requests on a timer. Friends who have never been seen come back as offline
// rather than being absent — "all my friends and what they are" is the question.
func (h *Handler) ListFriends(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	records, err := Friends(r.Context(), h.pool, userID, time.Now())
	if err != nil {
		logx.Error("presence: list friends: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not read presence")
		return
	}

	out := make([]entry, 0, len(records))
	for _, rec := range records {
		out = append(out, entry{UserID: rec.UserID, Status: string(rec.Status)})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"presence": out})
}
