// Package nodeshare owns friend node-sharing: a lightweight inbox of node
// bundles sent friend-to-friend. A transfer is a ONE-WAY copy — the recipient
// clones the bundle into a project they own and dismisses the row; there is no
// live link or ownership. This is NOT the public node store (internal/nodes) —
// that is the curated catalog; this is person-to-person. The bundle is the
// node's {manifest, files} source, stored as an opaque JSON blob the server
// never inspects (same dumb-blob rule as sync). You may only send to an
// accepted friend; every not-allowed case (non-friend, nonexistent, self)
// collapses to the same opaque 404 so you can't probe.
package nodeshare

import (
	"context"
	"encoding/json"
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
	// Cap on a sent bundle — same ceiling as the node store's publish cap
	// (node sources are small text files; 4 MiB is generous).
	sendBodyLimit = 4 << 20

	// Client-branching error codes. errRecipient is deliberately opaque (it
	// covers non-friend, nonexistent, and self-target alike); errTransfer
	// likewise covers "no such transfer" and "not yours" on dismiss.
	errInvalidBundle = "invalid_bundle"
	errRecipient     = "recipient_not_found"
	errTransfer      = "transfer_not_found"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// transferResponse is the JSON-safe view of a received transfer. Bundle is
// re-emitted as raw JSON (never re-encoded into a Go shape — the server stays
// content-agnostic).
type transferResponse struct {
	ID             string          `json:"id"`
	SenderID       string          `json:"sender_id"`
	SenderUsername string          `json:"sender_username"`
	Bundle         json.RawMessage `json:"bundle"`
	CreatedAt      time.Time       `json:"created_at"`
}

type sendRequest struct {
	RecipientID string          `json:"recipient_id"`
	Bundle      json.RawMessage `json:"bundle"`
}

// Send stores a node bundle addressed to an accepted friend. The recipient
// must be an accepted friend; otherwise the request collapses to an opaque 404
// so the sender can't distinguish "not a friend" from "no such user".
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendBodyLimit)

	senderID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if !isJSONObject(req.Bundle) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBundle)
		return
	}
	recipientID := strings.TrimSpace(req.RecipientID)
	if recipientID == "" || recipientID == senderID {
		// Self-send and missing target both collapse to the opaque code.
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}

	if code := h.guardRecipient(r.Context(), senderID, recipientID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	t := &transferRecord{
		ID:          ulid.Make().String(),
		SenderID:    senderID,
		RecipientID: recipientID,
		Bundle:      req.Bundle,
	}
	if err := insertTransfer(r.Context(), h.pool, t); err != nil {
		logx.Error("nodeshare: insert: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send node")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": t.ID, "created_at": t.CreatedAt})
}

// ListReceived returns every transfer addressed to the authed user, newest
// first. An empty inbox is a 200 with an empty list.
func (h *Handler) ListReceived(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	rows, err := listReceived(r.Context(), h.pool, authedID)
	if err != nil {
		logx.Error("nodeshare: list received: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list received nodes")
		return
	}

	out := make([]transferResponse, len(rows))
	for i, t := range rows {
		out[i] = transferResponse{
			ID:             t.ID,
			SenderID:       t.SenderID,
			SenderUsername: t.SenderUsername,
			Bundle:         t.Bundle,
			CreatedAt:      t.CreatedAt,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"transfers": out})
}

// Dismiss deletes one received transfer after the recipient has handled it.
// Only the recipient may dismiss; someone else's transfer (or an unknown id)
// is the same opaque 404.
func (h *Handler) Dismiss(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		httpx.WriteError(w, http.StatusNotFound, errTransfer)
		return
	}

	deleted, err := deleteReceived(r.Context(), h.pool, id, authedID)
	if err != nil {
		logx.Error("nodeshare: dismiss: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not dismiss transfer")
		return
	}
	if !deleted {
		httpx.WriteError(w, http.StatusNotFound, errTransfer)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guardRecipient returns "" if the authed user may send to recipient, else an
// opaque error code. A DB failure is logged and collapsed to the opaque code so
// the handler never leaks whether a user exists or is a friend.
func (h *Handler) guardRecipient(ctx context.Context, senderID, recipientID string) string {
	exists, err := userExists(ctx, h.pool, recipientID)
	if err != nil {
		logx.Error("nodeshare: user exists: %v", err)
		return errRecipient
	}
	if !exists {
		return errRecipient
	}
	friends, err := areFriends(ctx, h.pool, senderID, recipientID)
	if err != nil {
		logx.Error("nodeshare: are friends: %v", err)
		return errRecipient
	}
	if !friends {
		return errRecipient
	}
	return ""
}

// isJSONObject reports whether raw is a JSON object ({...}). The decoder has
// already validated it as JSON; this just rejects arrays/strings/null so the
// stored bundle is always the {manifest, files} object shape.
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
