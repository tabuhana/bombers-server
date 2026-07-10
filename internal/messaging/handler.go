// Package messaging owns direct messages: text DMs between two users, persisted
// indefinitely in Postgres (DMs are not ephemeral; rooms are). v1 is text only —
// image/file attachments wait for the later S3 phase. Real-time delivery over
// WebSocket is a separate, later layer (internal/realtime); this domain is the
// durable REST source of truth that any device pulls history from. You may only
// message an accepted friend, and every not-allowed case (non-friend, blocked,
// nonexistent recipient) collapses to the same opaque 404 so you can't probe.
package messaging

import (
	"context"
	"encoding/json"
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

	defaultHistoryLimit = 100
	maxHistoryLimit     = 200

	// Client-branching error codes. errRecipient is deliberately opaque: it
	// covers non-friend, blocked, nonexistent, and self-target alike.
	errEmptyBody   = "empty_body"
	errBodyTooLong = "body_too_long"
	errRecipient   = "recipient_not_found"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// messageResponse is the JSON-safe view of a message.
type messageResponse struct {
	ID          string    `json:"id"`
	SenderID    string    `json:"sender_id"`
	RecipientID string    `json:"recipient_id"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
}

func toResponse(m *messageRecord) messageResponse {
	return messageResponse{
		ID:          m.ID,
		SenderID:    m.SenderID,
		RecipientID: m.RecipientID,
		Body:        m.Body,
		CreatedAt:   m.CreatedAt,
	}
}

type sendMessageRequest struct {
	RecipientID string `json:"recipient_id"`
	Body        string `json:"body"`
}

// Send persists a DM from the authed user to a recipient. The recipient must be
// an accepted friend; otherwise the request collapses to an opaque 404 so the
// sender can't distinguish "not a friend" from "no such user".
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
	body := strings.TrimSpace(req.Body)
	if body == "" {
		httpx.WriteError(w, http.StatusBadRequest, errEmptyBody)
		return
	}
	if len([]rune(body)) > maxBody {
		httpx.WriteError(w, http.StatusBadRequest, errBodyTooLong)
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

	saved, err := insertMessage(r.Context(), h.pool, &messageRecord{
		ID:          ulid.Make().String(),
		SenderID:    senderID,
		RecipientID: recipientID,
		Body:        body,
	})
	if err != nil {
		logx.Error("messaging: insert: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send message")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(saved))
}

// History returns the conversation between the authed user and {userID}, ordered
// oldest→newest, capped at the most recent `limit` messages (?limit=, default
// 100, max 200). The other party must be an accepted friend; otherwise opaque
// 404. An empty conversation is a 200 with an empty list.
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
	rows, err := listConversation(r.Context(), h.pool, authedID, otherID, limit)
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
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"messages": out})
}

// unreadConversation is one peer's entry in the unread summary.
type unreadConversation struct {
	UserID string `json:"user_id"`
	Count  int64  `json:"count"`
}

// Unread returns the authed user's unread DM summary in one request: a per-peer
// count of messages received but not yet read, plus the grand total. Read state
// is server-authoritative (a message_reads marker per peer), so this reflects
// reads made on any device. Nothing unread → empty conversations list, total 0.
func (h *Handler) Unread(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	entries, err := unreadCounts(r.Context(), h.pool, authedID)
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

// MarkRead marks the authed user's conversation with {userID} read up to now
// (server time), advancing the server-authoritative read marker so the unread
// count clears on every device. A missing or self-target path param is a client
// mistake (there's no such conversation to mark) → 400. Success is 204.
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

	if err := markRead(r.Context(), h.pool, authedID, peerID, time.Now()); err != nil {
		logx.Error("messaging: mark read: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not mark read")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guardRecipient returns "" if the authed user may converse with other, else an
// opaque error code. A DB failure is logged and collapsed to the opaque code so
// the handler never leaks whether a user exists or is a friend.
func (h *Handler) guardRecipient(ctx context.Context, authedID, otherID string) string {
	exists, err := userExists(ctx, h.pool, otherID)
	if err != nil {
		logx.Error("messaging: user exists: %v", err)
		return errRecipient
	}
	if !exists {
		return errRecipient
	}
	friends, err := areFriends(ctx, h.pool, authedID, otherID)
	if err != nil {
		logx.Error("messaging: are friends: %v", err)
		return errRecipient
	}
	if !friends {
		return errRecipient
	}
	return ""
}

// parseLimit clamps the ?limit= query param into [1, maxHistoryLimit], falling
// back to the default for missing or unparseable values.
func parseLimit(raw string) int {
	if raw == "" {
		return defaultHistoryLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultHistoryLimit
	}
	if n > maxHistoryLimit {
		return maxHistoryLimit
	}
	return n
}
