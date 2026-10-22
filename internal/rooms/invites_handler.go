package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// KindInvite is the notify kind an invite travels under. rooms cannot import
// notify, so the string is repeated here and cmd/bombers pins the two together.
// One kind for every change of state, the way `dm` covers arrive/edit/delete: the
// frame carries the whole invite and its `state` says which change this is.
//
// It is a NUDGE to a live socket and nothing more. Whether it landed, or whether
// anyone was listening at all, is not knowable and is never asked: GET /invites is
// the truth, and that is why it exists.
const KindInvite = "invite"

const (
	inviteBodyLimit = 1 << 12 // 4 KiB - an invite request is one id.

	errInviteNotFound = "invite_not_found"
	errInviteExpired  = "invite_expired"
	errInviteAnswered = "invite_answered"
	errTooManyInvites = "too_many_invites"
)

type inviteRequest struct {
	UserID string `json:"user_id"`
}

// pushInvite tells one user about an invite as it now stands.
func (h *Handler) pushInvite(userID string, inv Invite) {
	if h.nudge == nil {
		return
	}
	h.nudge([]string{userID}, KindInvite, inv)
}

// mayInvite is the gate on asking someone to a room, run before the room is
// touched. The inviter and invitee must be accepted friends - the rule every
// cross-user surface uses - AND the invitee must be someone the room would let
// in: the host, or the host's friend (`allowed`, the join gate itself).
//
// The second half is the join gate's, not new policy. Without it a member could
// invite their own friend who is a stranger to the host, the invitee would accept,
// and the room socket would answer 404: an invitation that can only fail. Widening
// the join gate to fix that would be a security change this feature has no
// business making, so the invite refuses up front instead.
//
// A failed check counts as no, like `allowed`.
func (h *Handler) mayInvite(ctx context.Context, room *Room, byID, inviteeID string) bool {
	friends, err := h.friends(ctx, byID, inviteeID)
	if err != nil {
		logx.Error("rooms: friendship check: %v", err)
		return false
	}
	if !friends {
		return false
	}
	if byID == room.HostID || inviteeID == room.HostID {
		return true // the friendship just checked IS the host relationship
	}
	return h.allowed(ctx, room, inviteeID)
}

// CreateInvite is POST /rooms/{roomID}/invites {"user_id"}: ask a friend to the
// room. Any MEMBER may, not only the host - the button is on every member's
// toolbar - and every refusal is the same opaque 404 the join gate gives: no such
// room, an ended one, not in it, not your friend, or someone the room would turn
// away. An id cannot be probed through this.
//
// 201 with the invite when it is new; 200 with the existing one when the person
// already has a pending invite to this room (asking twice is one invitation, and
// it is not pushed twice) or has declined one (final for the room's life).
func (h *Handler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req inviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, inviteBodyLimit)).Decode(&req); err != nil || req.UserID == "" {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	room, err := h.hub.Get(chi.URLParam(r, "roomID"))
	// Membership and self are checked before any query: they cost nothing and
	// settle most refusals without touching the database.
	if err != nil || req.UserID == authedID || !room.HasMember(authedID) ||
		!h.mayInvite(r.Context(), room, authedID, req.UserID) {
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}

	inv, created, err := room.Invite(authedID, req.UserID, h.username(r.Context(), req.UserID), h.now())
	switch {
	case errors.Is(err, ErrTooManyInvites):
		httpx.WriteError(w, http.StatusBadRequest, errTooManyInvites)
		return
	case err != nil:
		// The room ended, or the inviter left, between the checks above and the
		// lock. Same answer as if it had happened a moment earlier.
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}

	if !created {
		httpx.WriteJSON(w, http.StatusOK, inv)
		return
	}
	h.pushInvite(inv.To, inv)
	httpx.WriteJSON(w, http.StatusCreated, inv)
}

// ListInvites is GET /invites: what is waiting on the caller, across every room,
// newest first. Pending only - an answered, lapsed or ended invite is not waiting
// on anyone. This is the truth; the notify frame is only a nudge to look.
func (h *Handler) ListInvites(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"invites": h.hub.InvitesFor(authedID, h.now()),
	})
}

// AcceptInvite is POST /invites/{inviteID}/accept. It marks the invite and hands
// back the room id - and joins NOTHING. The client then opens the room socket as
// it always has, through the join gate, which is why the gate stays the only way
// into a room.
func (h *Handler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	inv, ok := h.answer(w, r, true)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"room": inv.Room})
}

// DeclineInvite is POST /invites/{inviteID}/decline. Final for the life of the
// room: the inviter is told, and asking again returns this same declined invite.
func (h *Handler) DeclineInvite(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.answer(w, r, false); !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// answer is the one path both answers share, so they cannot disagree on who may
// answer or what happens next. On success the inviter is told; on any refusal it
// has already written the response and returns false.
func (h *Handler) answer(w http.ResponseWriter, r *http.Request, accept bool) (Invite, bool) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return Invite{}, false
	}
	inv, err := h.hub.AnswerInvite(authedID, chi.URLParam(r, "inviteID"), accept, h.now())
	switch {
	case err == nil:
		h.pushInvite(inv.From, inv)
		return inv, true
	case errors.Is(err, ErrInviteExpired):
		httpx.WriteError(w, http.StatusGone, errInviteExpired)
	case errors.Is(err, ErrInviteAnswered):
		httpx.WriteError(w, http.StatusConflict, errInviteAnswered)
	default:
		// Unknown, or addressed to someone else: the same opaque answer.
		httpx.WriteError(w, http.StatusNotFound, errInviteNotFound)
	}
	return Invite{}, false
}
