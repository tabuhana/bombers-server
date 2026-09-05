// Package friends owns the friend graph: friend codes, friend requests,
// the friendship relation. The first endpoint is the warm-up — returning
// the caller's own friend code so the share flow has something to read.
package friends

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
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

	// Remove/block/unblock error codes.
	errNotFriends      = "not_friends"
	errCannotBlockSelf = "cannot_block_self"
	errNotBlocked      = "not_blocked"

	// HTTP-response state strings that aren't DB states.
	responseStateRejected  = "rejected"
	responseStateRemoved   = "removed"
	responseStateUnblocked = "unblocked"
)

// requestAction discriminates the two transition paths inside actOnRequest.
type requestAction int

const (
	requestActionAccept requestAction = iota
	requestActionReject
)

// Notify is called when the other party should look at their friends list
// again — a request arrived, or one they sent was accepted. A function rather
// than an import of the notify package: a domain reaching into another domain is
// the thing this codebase does not do. Nil means nobody is listening.
type Notify func(userID string)

type Handler struct {
	pool   *pgxpool.Pool
	notify Notify
}

func NewHandler(pool *pgxpool.Pool, notify Notify) *Handler {
	return &Handler{pool: pool, notify: notify}
}

func (h *Handler) nudge(userID string) {
	if h.notify != nil && userID != "" {
		h.notify(userID)
	}
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
		logx.Error("friends: get own friend code: %v", err)
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
		logx.Error("friends: resolve friend code: %v", err)
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

	// A request that landed is worth seeing now — it shows up as a pill in their
	// dock, and waiting for a refresh to find out somebody added you is the kind
	// of lateness that makes an app feel dead.
	h.nudge(target.ID)
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
	logx.Error("friends: send request: %v", err)
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

	// Tell the person who asked. Accepting concerns them far more than it
	// concerns you — you already know what you just clicked.
	h.nudge(requesterID)
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

// listFriendsResponse wraps the result list in an object instead of returning
// a bare array. The wrapper lets us add metadata (count, pagination cursor,
// "last sync at") later without breaking clients that have already adopted
// the endpoint.
type listFriendsResponse struct {
	Friends []types.PublicUser `json:"friends"`
}

// List returns the authed user's accepted friends. Blocked rows are excluded
// at the SQL layer.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	records, err := listAcceptedFriends(r.Context(), h.pool, authedID)
	if err != nil {
		logx.Error("friends: list: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list friends")
		return
	}

	friends := make([]types.PublicUser, 0, len(records))
	for i := range records {
		friends = append(friends, records[i].public())
	}
	httpx.WriteJSON(w, http.StatusOK, listFriendsResponse{Friends: friends})
}

// listRequestsResponse splits pending requests by side so the client can
// render the inbox (incoming) and the sent-folder (outgoing) differently.
type listRequestsResponse struct {
	Incoming []types.PublicUser `json:"incoming"`
	Outgoing []types.PublicUser `json:"outgoing"`
}

// ListRequests returns pending friend requests involving the authed user,
// split into incoming (someone else asked) and outgoing (I asked).
func (h *Handler) ListRequests(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	records, err := listPendingRequests(r.Context(), h.pool, authedID)
	if err != nil {
		logx.Error("friends: list requests: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list requests")
		return
	}

	// Init to empty slices (NOT nil) so the JSON encoder writes [] rather
	// than null for empty halves.
	incoming := []types.PublicUser{}
	outgoing := []types.PublicUser{}
	for i := range records {
		u := records[i].public()
		if records[i].IsOutgoing {
			outgoing = append(outgoing, u)
		} else {
			incoming = append(incoming, u)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, listRequestsResponse{
		Incoming: incoming,
		Outgoing: outgoing,
	})
}

// RemoveFriend deletes an accepted friendship. Either party can unfriend.
// Pending/blocked rows are NOT a valid target — they get the same opaque
// 404 as a missing row.
func (h *Handler) RemoveFriend(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	targetID := chi.URLParam(r, "userID")
	if targetID == "" {
		httpx.WriteError(w, http.StatusNotFound, errNotFriends)
		return
	}

	user, actErr := h.unfriend(r.Context(), authedID, targetID)
	if actErr != nil {
		writeSendRequestError(w, actErr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sendRequestResponse{
		User:  user.public(),
		State: responseStateRemoved,
	})
}

func (h *Handler) unfriend(ctx context.Context, authedID, targetID string) (*userRecord, error) {
	userA, userB := canonicalPair(authedID, targetID)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	existing, err := getFriendshipForUpdate(ctx, tx, userA, userB)
	if err != nil {
		if errors.Is(err, ErrFriendshipNotFound) {
			return nil, sendErr(http.StatusNotFound, errNotFriends)
		}
		return nil, err
	}
	if existing.State != StateAccepted {
		// Pending or blocked rows are not "friendships" to remove. Opaque
		// 404 — don't reveal whether the row is pending or blocked.
		return nil, sendErr(http.StatusNotFound, errNotFriends)
	}

	user, err := getUserByID(ctx, tx, targetID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, sendErr(http.StatusNotFound, errNotFriends)
		}
		return nil, err
	}

	if err := deleteFriendship(ctx, tx, userA, userB); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return user, nil
}

// Block blocks the target user. Behavior is idempotent — calling block on
// someone already blocked is a 200 success (no error), and calling block
// when the OTHER party has already blocked you is also a silent 200 (the
// existing row is preserved so no-leak holds; the unblock authority stays
// with the original blocker).
//
// Self-block is the one explicit failure (400 cannot_block_self) — without
// the check, the canonical-pair (X, X) row would fail the schema CHECK and
// surface as a confusing 500.
func (h *Handler) Block(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	targetID := chi.URLParam(r, "userID")
	if targetID == "" || targetID == authedID {
		httpx.WriteError(w, http.StatusBadRequest, errCannotBlockSelf)
		return
	}

	user, actErr := h.blockUser(r.Context(), authedID, targetID)
	if actErr != nil {
		writeSendRequestError(w, actErr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sendRequestResponse{
		User:  user.public(),
		State: StateBlocked,
	})
}

func (h *Handler) blockUser(ctx context.Context, authedID, targetID string) (*userRecord, error) {
	userA, userB := canonicalPair(authedID, targetID)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve target user first. If they don't exist, fail with the same
	// not_friends code as the unfriend path — block target validity is
	// effectively opaque (low leak: user IDs are unguessable ULIDs).
	user, err := getUserByID(ctx, tx, targetID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, sendErr(http.StatusNotFound, errNotFriends)
		}
		return nil, err
	}

	existing, err := getFriendshipForUpdate(ctx, tx, userA, userB)
	if err != nil && !errors.Is(err, ErrFriendshipNotFound) {
		return nil, err
	}

	switch {
	case existing == nil:
		// No prior relationship — create a fresh blocked row.
		if err := insertBlockedFriendship(ctx, tx, userA, userB, authedID); err != nil {
			return nil, err
		}
	case existing.State == StateBlocked && existing.RequestedBy == targetID:
		// THEY blocked ME. Leave the row alone — preserves no-leak (we
		// don't reveal their block existed) and the unblock authority
		// stays with the original blocker. Net effect for the caller is
		// the same: no interaction with target. Known limitation: if the
		// other party later unblocks, the block is effectively gone for
		// both parties; the single-row-per-pair model has no way to
		// represent two simultaneous blocks. Acceptable at v1 scale.
	case existing.State == StateBlocked && existing.RequestedBy == authedID:
		// I already blocked them — idempotent success, no-op.
	default:
		// accepted or pending (either direction) → convert to blocked.
		if err := updateFriendshipBlock(ctx, tx, userA, userB, authedID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return user, nil
}

// Unblock removes a block — but only if the authed user was the original
// blocker. Any other case (no row, wrong state, blocked by the other party)
// collapses to a single opaque 404 not_blocked so the existence of a block
// placed by the OTHER party is never revealed.
func (h *Handler) Unblock(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	targetID := chi.URLParam(r, "userID")
	if targetID == "" {
		httpx.WriteError(w, http.StatusNotFound, errNotBlocked)
		return
	}

	user, actErr := h.unblockUser(r.Context(), authedID, targetID)
	if actErr != nil {
		writeSendRequestError(w, actErr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sendRequestResponse{
		User:  user.public(),
		State: responseStateUnblocked,
	})
}

func (h *Handler) unblockUser(ctx context.Context, authedID, targetID string) (*userRecord, error) {
	userA, userB := canonicalPair(authedID, targetID)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := getFriendshipForUpdate(ctx, tx, userA, userB)
	if err != nil {
		if errors.Is(err, ErrFriendshipNotFound) {
			return nil, sendErr(http.StatusNotFound, errNotBlocked)
		}
		return nil, err
	}
	// Wrong state OR I'm not the blocker → same 404. Don't distinguish:
	// the two cases together hide whether a block exists at all.
	if existing.State != StateBlocked || existing.RequestedBy != authedID {
		return nil, sendErr(http.StatusNotFound, errNotBlocked)
	}

	user, err := getUserByID(ctx, tx, targetID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, sendErr(http.StatusNotFound, errNotBlocked)
		}
		return nil, err
	}

	if err := deleteFriendship(ctx, tx, userA, userB); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return user, nil
}
