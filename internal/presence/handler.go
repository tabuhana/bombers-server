package presence

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// Two endpoints, and only friends can see anything. There is no way to ask
// whether a stranger is online — that would be a presence directory, and this
// server has no directory of any kind on purpose.
//
// The endpoints answer; the SOCKET is what makes a dot move (see Moved). A
// client reads this once at sign-in and then on each nudge, and never on a
// timer.

const errInvalidStatus = "invalid_status"

type Handler struct {
	pool *pgxpool.Pool
	// Who is connected right now, answered for a whole list at once (the nudge
	// hub's ConnectedAmong). A func rather than an import: this domain does not
	// learn what a WebSocket is, the same seam every other fan-out uses.
	connected func([]string) map[string]bool
	// Tell these people to re-read. The frame says only whose presence moved.
	nudge func(userIDs []string, fromID string)
}

func NewHandler(pool *pgxpool.Pool, connected func([]string) map[string]bool, nudge func(userIDs []string, fromID string)) *Handler {
	return &Handler{pool: pool, connected: connected, nudge: nudge}
}

// Announce tells a user's friends that their presence moved, so each re-reads
// GET /presence. Called when their first socket arrives, when their last one
// goes (the hub's hook), and when they choose a status.
//
// An INVISIBLE user's socket is nobody's business: `{"from": X}` says X is at
// their computer, which is the one thing "appear offline" promises not to say.
// Their CHOOSING offline is announced — that is the change, and all it reveals
// is the grey dot they asked for — which is why `told` is a parameter rather
// than something read back out of the table here.
func (h *Handler) Announce(ctx context.Context, userID string, told Status) {
	if h.nudge == nil || told == Offline {
		return
	}
	friends, err := FriendIDs(ctx, h.pool, userID)
	if err != nil {
		logx.Error("presence: friends to tell: %v", err)
		return
	}
	if len(friends) == 0 {
		return
	}
	h.nudge(friends, userID)
}

// Moved is the nudge hub's hook: a user's first socket arrived, or their last one
// went. Which of the two it was is deliberately not a parameter — the frame says
// only whose presence moved and every friend answers it with the same re-read,
// so an arrival and a departure are the same announcement.
func (h *Handler) Moved(ctx context.Context, userID string) {
	chosen, err := Chosen(ctx, h.pool, userID)
	if err != nil {
		logx.Error("presence: chosen status: %v", err)
		return
	}
	h.Announce(ctx, userID, chosen)
}

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
	// A choice is the one presence change that is announced whatever it was: going
	// invisible has to reach your friends, or the dot they are looking at stays
	// green. Theirs is the only read — this says nothing but "look again".
	h.Announce(r.Context(), userID, rec.Status)
	// Echo the EFFECTIVE status, not the stored one. They differ in exactly one
	// interesting case — you just set yourself offline — and a client that
	// rendered the stored value would show you as online for a moment. Connected
	// is true by construction: this request came over a session of theirs.
	httpx.WriteJSON(w, http.StatusOK, entry{
		UserID: userID,
		Status: string(Effective(rec.Status, true)),
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

	records, err := Friends(r.Context(), h.pool, userID, h.connected)
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
