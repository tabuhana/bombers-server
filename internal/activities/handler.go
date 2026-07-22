package activities

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
)

// The HTTP surface: browse the catalogue, download a game, fetch its assets.
// All three are read-only and auth-gated — publishing is console-only, the same
// rule the node store follows.

const errActivityNotFound = "activity_not_found"

type Handler struct {
	pool    *pgxpool.Pool
	storage media.Store
}

func NewHandler(pool *pgxpool.Pool, storage media.Store) *Handler {
	return &Handler{pool: pool, storage: storage}
}

// catalogEntry is the listing shape: enough to render a library row and decide
// whether to install, without shipping a single line of source or a byte of art.
type catalogEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Assets      int      `json:"assets"`
	AssetBytes  int64    `json:"asset_bytes"`
	Players     *players `json:"players,omitempty"`
}

type players struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// manifestFields is the slice of a bundle's manifest the catalogue surfaces. The
// server still doesn't interpret the bundle — it reads a few descriptive keys
// for the listing and passes everything else through untouched.
type manifestFields struct {
	Manifest struct {
		Description string   `json:"description"`
		Category    string   `json:"category"`
		Players     *players `json:"players"`
	} `json:"manifest"`
}

// List → GET /activities : the catalogue.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	records, err := List(r.Context(), h.pool)
	if err != nil {
		logx.Error("activities: list: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list activities")
		return
	}

	out := make([]catalogEntry, 0, len(records))
	for _, rec := range records {
		entry := catalogEntry{ID: rec.ID, Name: rec.Name, Version: rec.Version}
		var fields manifestFields
		if err := json.Unmarshal(rec.Bundle, &fields); err == nil {
			entry.Description = fields.Manifest.Description
			entry.Category = fields.Manifest.Category
			entry.Players = fields.Manifest.Players
		}
		// The download size matters here: a game with art is a real download,
		// and the library should say so before you commit to it.
		if assets, aerr := ListAssets(r.Context(), h.pool, rec.ID); aerr == nil {
			entry.Assets = len(assets)
			for _, a := range assets {
				entry.AssetBytes += a.Size
			}
		}
		out = append(out, entry)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"activities": out})
}

// Download → GET /activities/{id}/bundle : the {manifest, files} JSON, plus the
// asset manifest so an installer knows what else to fetch in one round trip.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, err := Get(r.Context(), h.pool, id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errActivityNotFound)
		return
	}
	if err != nil {
		logx.Error("activities: get: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch activity")
		return
	}

	assets, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("activities: list assets: %v", err)
		assets = []Asset{} // the game is still installable without its art
	}

	// The bundle is opaque JSON: splice it in rather than re-encoding, so what
	// the operator published is byte-for-byte what the client compiles.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	payload, _ := json.Marshal(map[string]any{"assets": assets})
	_, _ = w.Write([]byte(`{"bundle":`))
	_, _ = w.Write(rec.Bundle)
	_, _ = w.Write([]byte(`,`))
	_, _ = w.Write(payload[1:]) // drop the opening brace, keep "assets":[…]}
}

// Asset → GET /activities/{id}/assets/* : the bytes, streamed THROUGH the
// server. Never a bucket or presigned URL — the same rule profile media follows.
func (h *Handler) Asset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if !ValidAssetPath(path) {
		httpx.WriteError(w, http.StatusNotFound, errActivityNotFound)
		return
	}

	meta, err := GetAsset(r.Context(), h.pool, id, path)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errActivityNotFound)
		return
	}
	if err != nil {
		logx.Error("activities: get asset: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch asset")
		return
	}
	if h.storage == nil {
		httpx.WriteError(w, http.StatusInternalServerError, "media store unavailable")
		return
	}

	body, err := h.storage.GetObject(r.Context(), AssetKey(id, path))
	if err != nil {
		if errors.Is(err, media.ErrObjectNotFound) {
			httpx.WriteError(w, http.StatusNotFound, errActivityNotFound)
			return
		}
		logx.Error("activities: read asset: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch asset")
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	// Assets are immutable for a given published version, and the client caches
	// them on disk at install anyway; this just spares a re-download when it
	// doesn't.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if _, err := io.Copy(w, body); err != nil {
		logx.Error("activities: stream asset: %v", err)
	}
}
