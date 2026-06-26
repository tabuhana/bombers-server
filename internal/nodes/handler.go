// Package nodes serves the node store: catalog metadata, bundle downloads, and
// publishing. Nodes are the installable unit — a client lists the catalog,
// downloads a bundle, verifies its hash, and runs it. Curated in v1: publishing
// requires auth (owner-only gating is a follow-up), browsing/downloading are for
// any authenticated user.
package nodes

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
)

// Cap on a published node bundle. Node bundles are small JS files; 4 MiB is
// generous and keeps a bad upload from exhausting memory.
const publishBodyLimit = 4 << 20

type Handler struct {
	store *Store
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{store: NewStore(pool)}
}

// List → GET /nodes : catalog metadata for every published node.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.List(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}
	if items == nil {
		items = []Node{}
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

// Download → GET /nodes/{id}/bundle : the node.js ESM bytes (hash in the ETag).
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	bundle, hash, err := h.store.Bundle(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "unknown node")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "failed to read bundle")
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("ETag", `"`+hash+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bundle)
}

// publishRequest is the JSON body for POST /nodes: the manifest fields plus the
// base64-encoded node.js bundle.
type publishRequest struct {
	Node
	Bundle string `json:"bundle"`
}

// Publish → POST /nodes : publish (or replace) a node + bundle. Computes the
// bundle's sha256 and rejects a mismatched `hash` if one was supplied, then stores
// the computed hash so downloads can be integrity-checked.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, publishBodyLimit)

	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid body")
		return
	}

	bundle, err := base64.StdEncoding.DecodeString(req.Bundle)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bundle is not valid base64")
		return
	}
	if req.ID == "" || req.Name == "" || req.Version == "" || len(bundle) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "id, name, version and bundle are required")
		return
	}

	sum := sha256.Sum256(bundle)
	computed := "sha256-" + hex.EncodeToString(sum[:])
	if req.Hash != "" && req.Hash != computed {
		httpx.WriteError(w, http.StatusBadRequest, "hash does not match bundle")
		return
	}
	req.Hash = computed
	if req.Permissions == nil {
		req.Permissions = []string{}
	}

	if err := h.store.Upsert(r.Context(), req.Node, bundle); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "failed to publish node")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": req.ID, "hash": computed})
}
