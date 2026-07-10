// Package sync is the publish/pull half of the client↔server contract: the
// server's versioned key-value store of items a client has published, keyed by
// the client's ULID and scoped to the account. Sync is TRIGGERED, not streamed —
// these are plain request/response endpoints the client initiates:
//
//	POST /sync/push    upsert a batch of items (last-write-wins by client updated_at)
//	GET  /sync/pull    fetch my items (optionally only those changed since ?since=)
//	GET  /sync/status  when the account last synced + live item count
//
// The server never edits content — it stores what clients push (an opaque JSON
// blob per item) and serves it back. Conflict handling is last-write-wins for
// v1: a push reports each item as "applied" (server took it) or "stale" (server
// kept a newer copy; client should pull). Real-time nudges that make pull "feel
// live" are a later WebSocket layer (internal/realtime). Sharing (pulling items
// others shared with me) is a later domain (internal/sharing); pull returns only
// the caller's own items today.
package sync

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	pushBodyLimit   = 8 << 20 // 8 MiB per push batch
	maxContentBytes = 1 << 20 // 1 MiB of JSON content per item
	maxItemsPerPush = 500     // items per push batch
	maxTypeLen      = 64

	// Client-branching error codes.
	errTooManyItems    = "too_many_items"
	errMissingID       = "missing_item_id"
	errMissingType     = "missing_item_type"
	errTypeTooLong     = "type_too_long"
	errInvalidContent  = "invalid_content"
	errContentTooLarge = "content_too_large"
	errMissingUpdated  = "missing_updated_at"
	errInvalidUpdated  = "invalid_updated_at"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// pushItem is one item in a push batch. updated_at is the client's own timestamp
// (authoritative for last-write-wins). content is an opaque JSON object.
type pushItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	UpdatedAt string          `json:"updated_at"`
	Deleted   bool            `json:"deleted"`
}

type pushRequest struct {
	Items []pushItem `json:"items"`
}

type pushResultDTO struct {
	ID     string `json:"id"`
	Status string `json:"status"` // "applied" | "stale"
}

// Push upserts a batch of published items. The whole batch is one transaction,
// so a single malformed item rejects the batch (the client resends) rather than
// applying a partial, ambiguous result.
func (h *Handler) Push(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, pushBodyLimit)

	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Items) > maxItemsPerPush {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyItems)
		return
	}

	records := make([]itemRecord, len(req.Items))
	for i := range req.Items {
		rec, code := req.Items[i].toRecord()
		if code != "" {
			httpx.WriteError(w, http.StatusBadRequest, code)
			return
		}
		records[i] = rec
	}

	// An empty batch is a valid no-op "touch" — it still refreshes last-synced.
	results, syncedAt, err := pushItems(r.Context(), h.pool, ownerID, records)
	if err != nil {
		logx.Error("sync: push: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not push items")
		return
	}

	out := make([]pushResultDTO, len(results))
	for i, res := range results {
		out[i] = pushResultDTO{ID: res.ID, Status: res.Status}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"server_time": syncedAt,
		"results":     out,
	})
}

// toRecord validates and normalizes a pushed item into a storable record,
// returning a non-empty error code on invalid input.
func (it *pushItem) toRecord() (itemRecord, string) {
	if it.ID == "" {
		return itemRecord{}, errMissingID
	}
	if it.Type == "" {
		return itemRecord{}, errMissingType
	}
	if len(it.Type) > maxTypeLen {
		return itemRecord{}, errTypeTooLong
	}
	if it.UpdatedAt == "" {
		return itemRecord{}, errMissingUpdated
	}
	updated, err := time.Parse(time.RFC3339, it.UpdatedAt)
	if err != nil {
		return itemRecord{}, errInvalidUpdated
	}

	content := it.Content
	if len(content) == 0 || string(content) == "null" {
		content = json.RawMessage(`{}`)
	} else {
		if len(content) > maxContentBytes {
			return itemRecord{}, errContentTooLarge
		}
		// Must be a JSON object so the client's keyed shape holds.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(content, &obj); err != nil {
			return itemRecord{}, errInvalidContent
		}
	}

	return itemRecord{
		ID:        it.ID,
		Type:      it.Type,
		Content:   content,
		UpdatedAt: updated,
		Deleted:   it.Deleted,
	}, ""
}

// itemDTO is the JSON-safe view of a published item on pull.
type itemDTO struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Content         json.RawMessage `json:"content"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Deleted         bool            `json:"deleted"`
	ServerUpdatedAt time.Time       `json:"server_updated_at"`
}

// Pull returns the caller's published items. With ?since=<RFC3339> it returns
// only items the server has written since then (incremental refresh); without
// it, the full set (new-device restore). Tombstones are included so the client
// can apply deletes.
func (h *Handler) Pull(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var since *time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, errInvalidUpdated)
			return
		}
		since = &t
	}

	rows, err := pullItems(r.Context(), h.pool, ownerID, since)
	if err != nil {
		logx.Error("sync: pull: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not pull items")
		return
	}

	out := make([]itemDTO, len(rows))
	for i := range rows {
		out[i] = itemDTO{
			ID:              rows[i].ID,
			Type:            rows[i].Type,
			Content:         rows[i].Content,
			UpdatedAt:       rows[i].UpdatedAt,
			Deleted:         rows[i].Deleted,
			ServerUpdatedAt: rows[i].ServerUpdatedAt,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"server_time": time.Now().UTC(),
		"items":       out,
	})
}

// Status reports when the account last synced and how many live items it has,
// for the client's "last synced X ago" indicator.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	s, err := getSyncStatus(r.Context(), h.pool, ownerID)
	if err != nil {
		logx.Error("sync: status: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch sync status")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"last_synced_at": s.LastSyncedAt,
		"item_count":     s.ItemCount,
	})
}
