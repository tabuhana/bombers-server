// Package messaging owns direct messages: text DMs between two users, persisted
// indefinitely in Postgres (DMs are not ephemeral; rooms are). v1 is text only —
// image/file attachments wait for the later S3 phase. Live delivery carries the
// message: once a write is stored, the Notify func below (wired to
// internal/notify in main) pushes it to both people in the conversation, so a
// client applies it directly instead of going back to ask. This domain stays the
// durable REST source of truth — history for a fresh reader, and a seq-ordered
// catch-up feed for one that missed frames. You may only message an accepted
// friend, and every not-allowed case (non-friend, blocked, nonexistent
// recipient) collapses to the same opaque 404 so you can't probe.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	sendBodyLimit = 1 << 16 // 64 KiB — DMs are text, not blobs.
	maxBody       = 4000    // max characters per message
	maxNonce      = 64      // max characters of a client's nonce

	defaultHistoryLimit = 100
	maxHistoryLimit     = 200

	defaultConversationsLimit = 50
	maxConversationsLimit     = 200

	defaultUpdatesLimit = 200
	maxUpdatesLimit     = 500

	// Client-branching error codes. errRecipient is deliberately opaque: it
	// covers non-friend, blocked, nonexistent, and self-target alike.
	// errMessageNotFound is the same idea for a message id: it covers missing,
	// already deleted and not yours, so an id can't be probed for.
	errEmptyBody       = "empty_body"
	errBodyTooLong     = "body_too_long"
	errNonceTooLong    = "nonce_too_long"
	errRecipient       = "recipient_not_found"
	errMessageNotFound = "message_not_found"
	// errBadCursor is a ?before= naming no message the caller may page from. It
	// is an error and not an empty page: an empty page reads as "there is nothing
	// older", which is a different statement.
	errBadCursor = "bad_cursor"

	// The three things a dm frame can say happened to a message.
	opNew    = "new"
	opEdit   = "edit"
	opDelete = "delete"
)

// KindDM is the notify kind messaging pushes under. It repeats the value of
// notify.KindDM because messaging may not import that package; a test in
// cmd/bombers pins the two together.
const KindDM = "dm"

// KindTyping and KindRead repeat notify.KindTyping and notify.KindRead for the
// same reason, and the same test pins them.
const (
	KindTyping = "typing"
	KindRead   = "read"
)

// Notify delivers one frame of the given kind to every listed user, if anything
// is wired up to hear it. It is how a stored message reaches the people in the
// conversation while they have the app open. A function rather than an import of
// the notify package: messaging has no business knowing what a WebSocket is, and
// a domain reaching into another domain is the thing this codebase does not do.
//
// Nil is a valid value and means nobody is listening — the console and the tests
// both build handlers that way.
type Notify func(userIDs []string, kind string, data any)

type Handler struct {
	db     dbExecutor
	begin  beginFunc
	notify Notify
}

func NewHandler(pool *pgxpool.Pool, notify Notify) *Handler {
	return newHandler(pool, pool.Begin, notify)
}

// newHandler is the seam: a test builds a Handler over a fake database.
func newHandler(db dbExecutor, begin beginFunc, notify Notify) *Handler {
	return &Handler{db: db, begin: begin, notify: notify}
}

// push delivers a frame to the listed users, if anything is wired up to hear it.
func (h *Handler) push(userIDs []string, kind string, data any) {
	if h.notify != nil {
		h.notify(userIDs, kind, data)
	}
}

// dmFrame is the `d` of a dm frame: what happened, who did it, and the message
// itself. `from` is the message's sender, which is also who edited or deleted it
// (only the sender may). Nonce rides only on a new message, and only when the
// sender supplied one.
type dmFrame struct {
	Op      string          `json:"op"`
	From    string          `json:"from"`
	Message messageResponse `json:"message"`
	Nonce   string          `json:"nonce,omitempty"`
}

// pushDM tells BOTH people in the conversation what happened to a message. The
// sender is included on purpose: it is how their other windows and devices stay
// in step with the one that acted. The ids are the message's own two parties, so
// there is no list to get wrong.
func (h *Handler) pushDM(op string, m *messageRecord, nonce string) {
	h.push([]string{m.RecipientID, m.SenderID}, KindDM, dmFrame{
		Op:      op,
		From:    m.SenderID,
		Message: toResponse(m),
		Nonce:   nonce,
	})
}

// typingFrame is the `d` of a typing frame: who is composing.
type typingFrame struct {
	From string `json:"from"`
}

// readFrame is the `d` of a read frame: who read, and how far. `at` and `id` are
// the reader's new marker, the (created_at, id) of the newest message of the
// recipient's they have read, so the person who sent the messages can mark
// everything up to that position as seen.
type readFrame struct {
	From string    `json:"from"`
	At   time.Time `json:"at"`
	ID   string    `json:"id"`
}

// messageResponse is the JSON-safe view of a message. edited_at and deleted_at
// are always present, null when unset, so a client never has to tell "absent"
// from "null". A deleted message has deleted_at set and an empty body.
type messageResponse struct {
	ID          string     `json:"id"`
	Seq         int64      `json:"seq"`
	SenderID    string     `json:"sender_id"`
	RecipientID string     `json:"recipient_id"`
	Body        string     `json:"body"`
	CreatedAt   time.Time  `json:"created_at"`
	EditedAt    *time.Time `json:"edited_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

func toResponse(m *messageRecord) messageResponse {
	return messageResponse{
		ID:          m.ID,
		Seq:         m.Seq,
		SenderID:    m.SenderID,
		RecipientID: m.RecipientID,
		Body:        m.Body,
		CreatedAt:   m.CreatedAt,
		EditedAt:    m.EditedAt,
		DeletedAt:   m.DeletedAt,
	}
}

type sendMessageRequest struct {
	RecipientID string `json:"recipient_id"`
	Body        string `json:"body"`
	Nonce       string `json:"nonce"`
}

// sendResponse is the message plus the nonce the sender supplied, echoed so their
// client can swap the row it drew optimistically for this one. The nonce is also
// the sender's idempotency key: see Send.
type sendResponse struct {
	messageResponse
	Nonce string `json:"nonce,omitempty"`
}

type editMessageRequest struct {
	Body string `json:"body"`
}

// cleanBody applies the one rule for what a message may say, shared by send and
// edit so the two can't drift: trimmed, not empty, at most maxBody characters.
// It returns the trimmed body, or the error code to answer with.
func cleanBody(raw string) (body, code string) {
	body = strings.TrimSpace(raw)
	if body == "" {
		return "", errEmptyBody
	}
	if len([]rune(body)) > maxBody {
		return "", errBodyTooLong
	}
	return body, ""
}

// Send persists a DM from the authed user to a recipient. The recipient must be
// an accepted friend; otherwise the request collapses to an opaque 404 so the
// sender can't distinguish "not a friend" from "no such user".
//
// A nonce makes the send idempotent on (sender, recipient, nonce). The first send
// answers 201 and pushes the message; a repeat — a retry after a response was
// lost — answers 200 with the message already stored and pushes nothing, because
// the recipient already has it and a second frame would show it twice. The
// recipient is part of the key so the same nonce sent to someone else is a new
// message that gets delivered. No nonce, no idempotency: two identical sends are
// two messages.
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendBodyLimit)

	senderID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	recipientID := strings.TrimSpace(req.RecipientID)
	body, code := cleanBody(req.Body)
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}
	nonce := strings.TrimSpace(req.Nonce)
	if len([]rune(nonce)) > maxNonce {
		httpx.WriteError(w, http.StatusBadRequest, errNonceTooLong)
		return
	}
	if recipientID == "" || recipientID == senderID {
		// Self-DM and missing target both collapse to the opaque recipient code.
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}

	if code := h.guardRecipient(r.Context(), senderID, recipientID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	var created bool
	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		m, wasCreated, err := insertOnce(r.Context(), tx, &messageRecord{
			ID:          ulid.Make().String(),
			SenderID:    senderID,
			RecipientID: recipientID,
			Body:        body,
			Nonce:       nonce,
		})
		created = wasCreated
		return m, err
	})
	if err != nil {
		logx.Error("messaging: insert: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send message")
		return
	}
	if !created {
		httpx.WriteJSON(w, http.StatusOK, sendResponse{messageResponse: toResponse(saved), Nonce: nonce})
		return
	}
	// Stored, so it's safe to say so. After the write and not before: a frame for
	// a message that failed to save would show the recipient something that isn't
	// there.
	h.pushDM(opNew, saved, nonce)
	httpx.WriteJSON(w, http.StatusCreated, sendResponse{messageResponse: toResponse(saved), Nonce: nonce})
}

// Edit rewrites the body of a message the authed user sent. An edit ADDS words to
// a thread, so it needs the friendship that sending needed: after an unfriend or a
// block the sender can no longer put new text in front of the other person, and no
// dm frame is pushed to them.
//
// Every refusal — someone else's message, a message that does not exist, one
// already deleted, and a recipient who is no longer a friend — is the same opaque
// 404 message_not_found, so a message id can't be probed for. The order is what
// keeps it so: the message is looked up scoped to the caller as its sender, so a
// non-sender is turned away on the lookup alone and never reaches the friendship
// check; only the sender can get as far as learning that the recipient is no longer
// a friend, and they already know both. There is no edit window.
func (h *Handler) Edit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendBodyLimit)

	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req editMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body, code := cleanBody(req.Body)
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "messageID"))
	recipientID, err := ownMessageRecipient(r.Context(), h.db, id, authedID)
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if err != nil {
		logx.Error("messaging: edit: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not edit message")
		return
	}
	if h.guardRecipient(r.Context(), authedID, recipientID) != "" {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}

	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		return editMessage(r.Context(), tx, id, authedID, body)
	})
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if err != nil {
		logx.Error("messaging: edit: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not edit message")
		return
	}
	h.pushDM(opEdit, saved, "")
	httpx.WriteJSON(w, http.StatusOK, toResponse(saved))
}

// Delete turns a message the authed user sent into a tombstone: the body is
// cleared, deleted_at is set, and the row stays so a client that cached the
// message learns it is gone. Same opaque 404 as Edit for anything that is not a
// live message of the caller's.
//
// Delete is NOT friend-gated, and must not be: it is the one write on a thread
// that only takes words away. An edit adds words, so it needs the friendship that
// sending needed; a delete removes the sender's own words, and its dm frame carries
// no body at all (a delete clears it). Someone who has been unfriended or blocked
// keeps the right to take their own words back, and refusing it would protect
// nobody, because the result is strictly less text in front of the other person.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "messageID"))
	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		return deleteMessage(r.Context(), tx, id, authedID)
	})
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if err != nil {
		logx.Error("messaging: delete: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete message")
		return
	}
	h.pushDM(opDelete, saved, "")
	w.WriteHeader(http.StatusNoContent)
}

// Updates is the catch-up feed: every message the authed user sent or received
// with someone they are still accepted friends with that changed after ?since=,
// ordered by seq ascending, tombstones included —
// that is how a client that was away learns something was deleted. seq moves on
// every edit and delete, so this is the ONLY thing seq orders; a conversation is
// ordered by (created_at, id). `cursor` is the highest seq in the page (the
// requested since when the page is empty), and `has_more` is true when the page
// filled to the limit, so a client that was offline for a week pages forward
// until it is false. The feed is friends-only like history and the conversation
// list: a thread with someone who is no longer a friend is not in it.
func (h *Handler) Updates(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	since := parseSince(r.URL.Query().Get("since"))
	limit := clampLimit(r.URL.Query().Get("limit"), defaultUpdatesLimit, maxUpdatesLimit)
	rows, err := listUpdates(r.Context(), h.db, authedID, since, limit)
	if err != nil {
		logx.Error("messaging: updates: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}

	out := make([]messageResponse, len(rows))
	cursor := since
	for i := range rows {
		out[i] = toResponse(&rows[i])
		cursor = rows[i].Seq
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"messages": out,
		"cursor":   cursor,
		"has_more": len(rows) == limit,
	})
}

// History returns one page of the conversation between the authed user and
// {userID}, ordered oldest→newest. With no ?before= it is the newest page; with
// one it is the page strictly older than that message id, keyset on
// (created_at, id) so it never skips or repeats a message as new ones arrive.
// ?limit= defaults to 100, max 200, and `has_more` is true when the page filled
// to the limit, so a client scrolling back stops when it comes back false.
// Tombstones are left out. The other party must be an accepted friend; otherwise
// opaque 404. A ?before= that names no message of this conversation is a 400
// bad_cursor. An empty conversation is a 200 with an empty list.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	otherID := chi.URLParam(r, "userID")
	if otherID == "" || otherID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}

	if code := h.guardRecipient(r.Context(), authedID, otherID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"))
	var before *cursor
	if id := strings.TrimSpace(r.URL.Query().Get("before")); id != "" {
		c, err := historyCursor(r.Context(), h.db, authedID, otherID, id)
		if errors.Is(err, errNoCursor) {
			httpx.WriteError(w, http.StatusBadRequest, errBadCursor)
			return
		}
		if err != nil {
			logx.Error("messaging: history cursor: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
			return
		}
		before = c
	}

	rows, err := listConversation(r.Context(), h.db, authedID, otherID, limit, before)
	if err != nil {
		logx.Error("messaging: history: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}

	// listConversation returns newest-first; reverse to ascending for rendering.
	out := make([]messageResponse, len(rows))
	for i, m := range rows {
		out[len(rows)-1-i] = toResponse(&m)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"messages": out,
		"has_more": len(rows) == limit,
	})
}

// conversationResponse is one line of the conversation list. Each read marker is
// a position, so it is two fields, `*_read_at` and `*_read_id`; all four are
// present either way and null until that person has read anything. A marker from
// before positions existed has its time and a null id, and means everything up to
// that time is read.
type conversationResponse struct {
	UserID      string          `json:"user_id"`
	LastMessage messageResponse `json:"last_message"`
	Unread      int64           `json:"unread"`
	MyReadAt    *time.Time      `json:"my_read_at"`
	MyReadID    *string         `json:"my_read_id"`
	PeerReadAt  *time.Time      `json:"peer_read_at"`
	PeerReadID  *string         `json:"peer_read_id"`
}

// Conversations is the conversation list: one entry per accepted friend the
// authed user has any live message with, in either direction, ordered by the last
// message's created_at descending. `last_message` is the newest message that has
// not been deleted, so a pair whose every message was deleted is absent. With
// ?before=<last_message.id of the previous page's final entry> it is the next
// page, keyset on that message's (created_at, id); ?limit= defaults to 50, max
// 200, and `has_more` is true when the page filled to the limit. A ?before= that
// names no message of the caller's is a 400 bad_cursor.
func (h *Handler) Conversations(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	limit := clampLimit(r.URL.Query().Get("limit"), defaultConversationsLimit, maxConversationsLimit)
	var before *cursor
	if id := strings.TrimSpace(r.URL.Query().Get("before")); id != "" {
		c, err := inboxCursor(r.Context(), h.db, authedID, id)
		if errors.Is(err, errNoCursor) {
			httpx.WriteError(w, http.StatusBadRequest, errBadCursor)
			return
		}
		if err != nil {
			logx.Error("messaging: conversations cursor: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not load conversations")
			return
		}
		before = c
	}

	rows, err := listSummaries(r.Context(), h.db, authedID, limit, before)
	if err != nil {
		logx.Error("messaging: conversations: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load conversations")
		return
	}

	out := make([]conversationResponse, len(rows))
	for i := range rows {
		out[i] = conversationResponse{
			UserID:      rows[i].PeerID,
			LastMessage: toResponse(&rows[i].Last),
			Unread:      rows[i].Unread,
			MyReadAt:    rows[i].MyReadAt,
			MyReadID:    rows[i].MyReadID,
			PeerReadAt:  rows[i].PeerReadAt,
			PeerReadID:  rows[i].PeerReadID,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"conversations": out,
		"has_more":      len(rows) == limit,
	})
}

// unreadConversation is one peer's entry in the unread summary.
type unreadConversation struct {
	UserID string `json:"user_id"`
	Count  int64  `json:"count"`
}

// Unread returns the authed user's unread DM summary in one request: a per-peer
// count of messages received but not yet read, plus the grand total. Read state
// is server-authoritative (a message_reads position per peer), so this reflects
// reads made on any device. Nothing unread → empty conversations list, total 0.
func (h *Handler) Unread(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	entries, err := unreadCounts(r.Context(), h.db, authedID)
	if err != nil {
		logx.Error("messaging: unread: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load unread counts")
		return
	}

	// make (not nil) so an empty summary serializes as [] rather than null.
	conversations := make([]unreadConversation, len(entries))
	var total int64
	for i, e := range entries {
		conversations[i] = unreadConversation{UserID: e.PeerID, Count: e.Count}
		total += e.Count
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"conversations": conversations,
		"total":         total,
	})
}

// MarkRead marks the authed user's conversation with {userID} read up to the
// newest message {userID} has sent them that is not deleted, advancing the
// server-authoritative read marker so the unread count clears on every device. The
// marker is that message's (created_at, id), read out of the database by the
// statement itself: the request carries no time and no id, and no clock is
// consulted. A missing or self-target path param is a client mistake (there's no
// such conversation to mark) → 400. Success is 204, in every case below too.
//
// The marker itself is a personal bookmark and is not friend-gated. Telling the
// peer is: when the marker moved AND the two are accepted friends, the peer gets
// a read frame carrying exactly the position that was stored, so whoever sent the
// messages learns they were seen. A call that moved nothing tells nobody — there
// was nothing from the peer to read, or it was all read already — and neither does
// one aimed at somebody who is not a friend, otherwise this route would be a way
// to push a frame to any user id.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	peerID := strings.TrimSpace(chi.URLParam(r, "userID"))
	if peerID == "" || peerID == authedID {
		httpx.WriteError(w, http.StatusBadRequest, "invalid conversation")
		return
	}

	position, advanced, err := markRead(r.Context(), h.db, authedID, peerID)
	if err != nil {
		logx.Error("messaging: mark read: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not mark read")
		return
	}
	if advanced {
		// The marker is saved whatever happens next, so a failure here costs the
		// peer a receipt and the reader nothing: log it and still answer 204.
		friends, err := areFriends(r.Context(), h.db, authedID, peerID)
		if err != nil {
			logx.Error("messaging: are friends: %v", err)
		} else if friends {
			h.push([]string{peerID}, KindRead, readFrame{From: authedID, At: position.At.UTC(), ID: position.ID})
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Typing tells {userID} that the authed user is composing a message to them. It
// stores nothing and answers 204. The peer must be an accepted friend, with the
// same opaque 404 as sending to one. The server does not throttle it: the client
// sends at most one every few seconds while a person is typing.
func (h *Handler) Typing(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	peerID := strings.TrimSpace(chi.URLParam(r, "userID"))
	if peerID == "" || peerID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}
	if code := h.guardRecipient(r.Context(), authedID, peerID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}
	h.push([]string{peerID}, KindTyping, typingFrame{From: authedID})
	w.WriteHeader(http.StatusNoContent)
}

// guardRecipient returns "" if the authed user may converse with other, else an
// opaque error code. A DB failure is logged and collapsed to the opaque code so
// the handler never leaks whether a user exists or is a friend.
func (h *Handler) guardRecipient(ctx context.Context, authedID, otherID string) string {
	exists, err := userExists(ctx, h.db, otherID)
	if err != nil {
		logx.Error("messaging: user exists: %v", err)
		return errRecipient
	}
	if !exists {
		return errRecipient
	}
	friends, err := areFriends(ctx, h.db, authedID, otherID)
	if err != nil {
		logx.Error("messaging: are friends: %v", err)
		return errRecipient
	}
	if !friends {
		return errRecipient
	}
	return ""
}

// parseLimit clamps the history ?limit= query param.
func parseLimit(raw string) int {
	return clampLimit(raw, defaultHistoryLimit, maxHistoryLimit)
}

// clampLimit clamps a ?limit= query param into [1, max], falling back to def for
// missing or unparseable values.
func clampLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// parseSince reads the catch-up cursor. Missing, negative or unparseable all mean
// "from the beginning" — a client that lost its cursor should get everything, not
// an error it can do nothing with.
func parseSince(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
