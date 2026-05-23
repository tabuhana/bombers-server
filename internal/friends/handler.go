// Package friends owns the friend graph: friend codes, friend requests,
// the friendship relation. The first endpoint is the warm-up — returning
// the caller's own friend code so the share flow has something to read.
package friends

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/types"
)

const (
	sendRequestBodyLimit = 1 << 10

	// Client-branching error codes for POST /friends/requests. See the
	// handler for which case emits which code.
	errFriendCodeRequired    = "friend_code_required"
	errCannotFriendSelf      = "cannot_friend_self"
	errUnknownFriendCode     = "unknown_friend_code"
	errAlreadyFriends        = "already_friends"
	errRequestAlreadyPending = "request_already_pending"
	errUserBlocked           = "user_blocked"

	// Accept/reject error codes.
	errUnknownRequest        = "unknown_request"
	errCannotActOnOwnRequest = "cannot_act_on_own_request"

	// responseStateRejected is the HTTP-response state for a successful
	// reject. It is NOT a DB state — rejection deletes the row.
	responseStateRejected = "rejected"
)

// requestAction discriminates the two transition paths inside actOnRequest.
type requestAction int

const (
	requestActionAccept requestAction = iota
	requestActionReject
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

type sendRequestRequest struct {
	FriendCode string `json:"friend_code"`
}

// sendRequestResponse is the success body for POST /friends/requests. The
// target user's PublicUser is included so the client can update its UI
// without a follow-up lookup.
type sendRequestResponse struct {
	User  types.PublicUser `json:"user"`
	State string           `json:"state"`
}

// SendRequest creates a pending friendship from the authed user to the user
// holding the supplied friend code. See the switch in this function for the
// edge-case behavior table — every branch is documented inline so future
// readers (and the test suite) can reason about it without external context.
func (h *Handler) SendRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendRequestBodyLimit)

	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req sendRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	code := strings.TrimSpace(req.FriendCode)
	if code == "" {
		httpx.WriteError(w, http.StatusBadRequest, errFriendCodeRequired)
		return
	}

	target, err := getUserByFriendCode(r.Context(), h.pool, code)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			httpx.WriteError(w, http.StatusNotFound, errUnknownFriendCode)
			return
		}
		log.Printf("friends: resolve friend code: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send request")
		return
	}

	if target.ID == authedID {
		httpx.WriteError(w, http.StatusBadRequest, errCannotFriendSelf)
		return
	}

	state, status, sendErr := h.upsertOutgoingRequest(r.Context(), authedID, target.ID)
	if sendErr != nil {
		writeSendRequestError(w, sendErr)
		return
	}

	httpx.WriteJSON(w, status, sendRequestResponse{
		User:  target.public(),
		State: state,
	})
}

// sendRequestError carries the (httpStatus, errorCode) pair that the handler
// needs to render. Kept internal so upsertOutgoingRequest can encode both
// success-shaped errors (409 conflicts) and opaque ones (404 for block-leak
// prevention) without leaking pgx error types up to the HTTP layer.
type sendRequestError struct {
	status int
	code   string
}

func (e *sendRequestError) Error() string { return e.code }

func sendErr(status int, code string) *sendRequestError {
	return &sendRequestError{status: status, code: code}
}

func writeSendRequestError(w http.ResponseWriter, err error) {
	var se *sendRequestError
	if errors.As(err, &se) {
		httpx.WriteError(w, se.status, se.code)
		return
	}
	log.Printf("friends: send request: %v", err)
	httpx.WriteError(w, http.StatusInternalServerError, "could not send request")
}

// upsertOutgoingRequest is the core decision tree. It runs in a transaction
// with SELECT … FOR UPDATE so two concurrent requests for the same pair
// (e.g. both users sending to each other at the same moment) serialize.
//
// Returns (newState, httpStatus, nil) on success or (_, _, *sendRequestError)
// on a client-visible error. Raw DB errors return (_, _, error) and become 500s.
func (h *Handler) upsertOutgoingRequest(ctx context.Context, authedID, targetID string) (string, int, error) {
	userA, userB := canonicalPair(authedID, targetID)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	existing, err := getFriendshipForUpdate(ctx, tx, userA, userB)
	if err != nil && !errors.Is(err, ErrFriendshipNotFound) {
		return "", 0, err
	}

	var newState string
	var status int

	switch {
	case existing == nil:
		// No prior friendship — create a fresh pending row.
		if err := insertPendingFriendship(ctx, tx, userA, userB, authedID); err != nil {
			return "", 0, err
		}
		newState = StatePending
		status = http.StatusCreated

	case existing.State == StateAccepted:
		// Already friends.
		return "", 0, sendErr(http.StatusConflict, errAlreadyFriends)

	case existing.State == StatePending && existing.RequestedBy == authedID:
		// I already have a pending request out to this user.
		return "", 0, sendErr(http.StatusConflict, errRequestAlreadyPending)

	case existing.State == StatePending && existing.RequestedBy == targetID:
		// They had a pending request to me; me asking them back is mutual
		// consent — promote to accepted. (Preserve original requested_by
		// as history; only the state and updated_at change.)
		if err := updateFriendshipState(ctx, tx, userA, userB, StateAccepted); err != nil {
			return "", 0, err
		}
		newState = StateAccepted
		status = http.StatusOK

	case existing.State == StateBlocked && existing.RequestedBy == authedID:
		// I blocked them — they're not gone, I just don't want this. Tell me.
		return "", 0, sendErr(http.StatusConflict, errUserBlocked)

	case existing.State == StateBlocked && existing.RequestedBy == targetID:
		// They blocked me. MUST NOT reveal this — collapse to the same 404
		// as a nonexistent friend code so the blocker stays opaque.
		return "", 0, sendErr(http.StatusNotFound, errUnknownFriendCode)

	default:
		// Unreachable unless the state CHECK constraint is bypassed or
		// requested_by is somehow neither user — fail closed.
		return "", 0, errors.New("unexpected friendship row state")
	}

	if err := tx.Commit(ctx); err != nil {
		return "", 0, err
	}
	return newState, status, nil
}

// AcceptRequest flips a pending request to accepted. Only the recipient can
// accept (i.e. the authed user must NOT be requested_by).
func (h *Handler) AcceptRequest(w http.ResponseWriter, r *http.Request) {
	h.actOnRequest(w, r, requestActionAccept)
}

// RejectRequest deletes a pending request so a fresh request can be sent
// later. Only the recipient can reject.
func (h *Handler) RejectRequest(w http.ResponseWriter, r *http.Request) {
	h.actOnRequest(w, r, requestActionReject)
}

// actOnRequest is the shared accept/reject pipeline: auth → parse requester
// id from URL → apply transition inside a tx → render {user, state}.
func (h *Handler) actOnRequest(w http.ResponseWriter, r *http.Request, action requestAction) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	requesterID := chi.URLParam(r, "requesterID")
	if requesterID == "" {
		// chi guarantees a non-empty path segment, but belt-and-braces.
		httpx.WriteError(w, http.StatusNotFound, errUnknownRequest)
		return
	}

	user, newState, actErr := h.applyRequestAction(r.Context(), authedID, requesterID, action)
	if actErr != nil {
		writeSendRequestError(w, actErr)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, sendRequestResponse{
		User:  user.public(),
		State: newState,
	})
}

// applyRequestAction runs the validate-and-mutate flow inside a transaction.
// Validation rules (every non-happy path returns a *sendRequestError):
//   - friendship row must exist (404 unknown_request if not)
//   - state must be pending (409 already_friends if accepted; 404 unknown_request
//     for any other state — blocked etc. — to avoid leaking the existence of
//     a block via a distinct error code)
//   - authed user must be the RECIPIENT, i.e. requested_by == requesterID
//     (409 cannot_act_on_own_request if the authed user is themselves the
//     initiator)
func (h *Handler) applyRequestAction(ctx context.Context, authedID, requesterID string, action requestAction) (*userRecord, string, error) {
	userA, userB := canonicalPair(authedID, requesterID)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	existing, err := getFriendshipForUpdate(ctx, tx, userA, userB)
	if err != nil {
		if errors.Is(err, ErrFriendshipNotFound) {
			return nil, "", sendErr(http.StatusNotFound, errUnknownRequest)
		}
		return nil, "", err
	}

	switch existing.State {
	case StatePending:
		// Continue below.
	case StateAccepted:
		return nil, "", sendErr(http.StatusConflict, errAlreadyFriends)
	default:
		// Blocked or any future state — opaque 404 so block existence isn't
		// distinguishable from "no such request".
		return nil, "", sendErr(http.StatusNotFound, errUnknownRequest)
	}

	// Only the recipient can act. requested_by == authedID means the authed
	// user is themselves the initiator → forbidden.
	if existing.RequestedBy != requesterID {
		if existing.RequestedBy == authedID {
			return nil, "", sendErr(http.StatusConflict, errCannotActOnOwnRequest)
		}
		// Defensive: requested_by isn't one of the pair (impossible given the
		// requester_in_pair CHECK constraint, but fail closed).
		return nil, "", sendErr(http.StatusNotFound, errUnknownRequest)
	}

	// Pre-load the requester's PublicUser inside the tx for the response.
	user, err := getUserByID(ctx, tx, requesterID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// FK CASCADE should make this impossible, but if it happens treat
			// as if the request never existed.
			return nil, "", sendErr(http.StatusNotFound, errUnknownRequest)
		}
		return nil, "", err
	}

	var newState string
	switch action {
	case requestActionAccept:
		if err := updateFriendshipState(ctx, tx, userA, userB, StateAccepted); err != nil {
			return nil, "", err
		}
		newState = StateAccepted
	case requestActionReject:
		if err := deleteFriendship(ctx, tx, userA, userB); err != nil {
			return nil, "", err
		}
		newState = responseStateRejected
	default:
		return nil, "", errors.New("unknown request action")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return user, newState, nil
}
