package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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

	// inviteMintTimeout bounds the write of a new invite's message.
	inviteMintTimeout = 5 * time.Second

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
	h.pushInviteTo([]string{userID}, inv)
}

// pushInviteTo tells several users the same invite, as one frame value.
func (h *Handler) pushInviteTo(userIDs []string, inv Invite) {
	if h.nudge == nil {
		return
	}
	h.nudge(userIDs, KindInvite, inv)
}

// mayInvite is the gate on asking someone to a room, run before the room is
// touched. The inviter and invitee must be accepted friends - the rule every
// cross-user surface uses - AND the room must be willing to take that invitee:
//
//   - the host's member-invites switch is OFF (the default): the invitee must be
//     someone the room admits on the host's own relationships - the host, or a
//     friend of the host (`hostRelation`). Without that a member could invite
//     their own friend who is a stranger to the host, and the invite could only
//     ever fail. This is deliberately NOT `allowed`: an accepted invite is also a
//     way in, and counting it here would let a member re-invite a stranger a
//     previous accepted invite let in, extending their stay past the hour with the
//     switch off. Off means off.
//   - the switch is ON: any member's friend will do, strangers to the host
//     included. The join gate admits them on the strength of the invite itself.
//
// `needsSwitch` reports that the answer rested on the switch being on, so the room
// can re-check it under its lock (see Room.Invite).
//
// A failed check counts as no, like `allowed`.
func (h *Handler) mayInvite(ctx context.Context, room *Room, byID, inviteeID string) (ok, needsSwitch bool) {
	friends, err := h.friends(ctx, byID, inviteeID)
	if err != nil {
		logx.Error("rooms: friendship check: %v", err)
		return false, false
	}
	if !friends {
		return false, false
	}
	if byID == room.HostID || inviteeID == room.HostID {
		return true, false // the friendship just checked IS the host relationship
	}
	if room.MemberInvites() {
		return true, true
	}
	return h.hostRelation(ctx, room, inviteeID), false
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
	if err != nil || req.UserID == authedID || !room.HasMember(authedID) {
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}
	mayInvite, needsSwitch := h.mayInvite(r.Context(), room, authedID, req.UserID)
	if !mayInvite {
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}

	inv, created, err := room.Invite(authedID, req.UserID, h.username(r.Context(), req.UserID), h.now(), needsSwitch)
	switch {
	case errors.Is(err, ErrTooManyInvites):
		httpx.WriteError(w, http.StatusBadRequest, errTooManyInvites)
		return
	case err != nil:
		// The room ended, the inviter left, or the host switched member invites off,
		// between the checks above and the lock. Same answer as if it had happened a
		// moment earlier.
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}

	if !created {
		// The existing invite already has its message; a second would be a second
		// card for one invitation.
		httpx.WriteJSON(w, http.StatusOK, inv)
		return
	}

	// A NEW invite gets its message in the thread, and the two are made together or
	// not at all. The invite lives in memory and the message in the database, so
	// this is not a transaction - it is the invite first, then the message, and if
	// the message cannot be written the invite is VOIDED: an invite with no card is
	// one its inviter cannot see and its invitee is told about by nothing. The other
	// direction (a message whose invite is gone, after a restart or the room
	// ending) is allowed and correct: the card still says what was said, and
	// GET /invites/{id} says the invite is no longer live.
	//
	// The write is not tied to the request: a client hanging up between the invite
	// and its message must not leave one without the other.
	if h.mintInvite != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), inviteMintTimeout)
		err := h.mintInvite(ctx, inv.ID, inv.Room, inv.RoomName, authedID, inv.To)
		cancel()
		if err != nil {
			logx.Error("rooms: invite message: %v", err)
			room.VoidInvite(inv.ID)
			httpx.WriteError(w, http.StatusInternalServerError, "could not create invite")
			return
		}
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

// GetInvite is GET /invites/{inviteID}: the invite as its inviter OR its invitee
// reads it, in whatever state it is in. It is what a card in the DM thread asks, so
// the sender's can say "they declined" and the recipient's "you joined" instead of
// going blank once the invite stops being pending (GET /invites lists only what is
// waiting, addressed to you).
//
// A 404 is the whole of the other answer, and the card should read it as EXPIRED:
// an invite that is not live cannot be joined, whether it was answered long ago,
// was pruned by the reaper's sweep, or went with the process. Unknown and not-yours
// are the same 404, so an id cannot be probed for.
func (h *Handler) GetInvite(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	inv, err := h.hub.InviteFor(authedID, chi.URLParam(r, "inviteID"), h.now())
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, errInviteNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
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
// answer or what happens next. On success the inviter and the answerer are told; on any refusal it
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
		// ONE frame, built once from the settled invite, to both ends: the inviter
		// learns the answer, and every other window the invitee owns settles on it
		// instead of showing a pending invite that would now 409. Two pushes built
		// separately could disagree about what the answer was.
		h.pushInviteTo([]string{inv.From, inv.To}, inv)
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
