// Package nodes serves the OFFICIAL NODE STORE: this server's operator-
// published nodes in the SDK {manifest, files} format (the same opaque-bundle
// shape nodeshare's friend transfers carry). Any authenticated user browses
// the catalog and downloads bundles to install; PUBLISHING is operator-only
// via the server console (internal/console `publish`/`unpublish`) — there is
// deliberately no HTTP publish endpoint, and no admin role yet (deferred).
// Each server's store is its own island: a self-hosted server's operator
// curates their own catalog with the same commands.
package nodes

import (
	"errors"
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
