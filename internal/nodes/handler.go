// Package nodes serves the OFFICIAL NODE STORE: this server's operator-
// published nodes in the SDK {manifest, files} format (the same opaque-bundle
// shape nodeshare's friend transfers carry). Any authenticated user browses
// the catalog and downloads bundles to install; PUBLISHING is operator-only —
// either the console's `publish`/`unpublish` or the ADMIN-gated POST /nodes and
// DELETE /nodes/{id}, which are the same operation reached over HTTP so an
// operator can publish from the client.
// Each server's store is its own island: a self-hosted server's operator
// curates their own catalog with the same commands.
package nodes

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const errNodeNotFound = "node_not_found"

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// catalogResponse is the wire shape of one store listing — light manifest
// fields only, never the files.
type catalogResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Icon        string    `json:"icon"`
	Description string    `json:"description"`
	Tags        []string  `json:"tags"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// List → GET /nodes : the store catalog. An empty store is a 200 with an
// empty list.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	entries, err := Catalog(r.Context(), h.pool)
	if err != nil {
		logx.Error("nodes: catalog: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}
	out := make([]catalogResponse, len(entries))
	for i, e := range entries {
		out[i] = catalogResponse{
			ID:          e.ID,
			Name:        e.Name,
			Version:     e.Version,
			Icon:        e.Icon,
			Description: e.Description,
			Tags:        e.Tags,
			UpdatedAt:   e.UpdatedAt,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// Download → GET /nodes/{id}/bundle : the full {manifest, files} JSON for
// install. (The /bundle suffix keeps this route clear of nodeshare's static
// /nodes/received.)
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	bundle, err := Bundle(r.Context(), h.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, errNodeNotFound)
		return
	}
	if err != nil {
		logx.Error("nodes: bundle %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to read bundle")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bundle)
}

// Publish → POST /nodes : upsert a {manifest, files} bundle into the store.
// ADMIN-ONLY (gated by admin.RequireAdmin at the route). This is the same
// operation the console's `publish` performs, reached over HTTP so an operator
// can publish from the client instead of copying a bundle onto the server box
// and typing at its prompt.
//
// The body IS the bundle, byte for byte — it's stored opaquely, so re-encoding
// it here would only risk changing what the client meant to publish.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	// Cap before reading: an unbounded ReadAll on a public route is how a
	// server gets memory-exhausted by one request.
	raw, err := io.ReadAll(io.LimitReader(r.Body, BundleLimit+1))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	if len(raw) > BundleLimit {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "bundle_too_large")
		return
	}
	info, err := PublishBundle(r.Context(), h.pool, raw)
	if err != nil {
		// parseBundle's failures are all "your bundle is wrong" — report the
		// reason, since the operator is the one who can fix it.
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	logx.Info("nodes: published %s (%s) v%s", info.ID, info.Name, info.Version)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":      info.ID,
		"name":    info.Name,
		"version": info.Version,
	})
}

// Unpublish → DELETE /nodes/{id} : remove a node from the store. ADMIN-ONLY.
// Idempotent — removing what isn't there reports `removed: false` rather than
// 404, so a retry after a dropped response isn't an error.
func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	removed, err := Unpublish(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("nodes: unpublish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to unpublish")
		return
	}
	if removed {
		logx.Info("nodes: unpublished %s", id)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "removed": removed})
}
