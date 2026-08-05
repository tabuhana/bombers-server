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

// The HTTP surface: browse the catalogue, download a game, fetch its assets —
// open to any authed user — plus publish, upload an asset, and unpublish, which
// are ADMIN-ONLY (gated at the router, not here).
//
// Publishing over HTTP is the same act the console's `publish-game` performs,
// reachable from a client so the operator doesn't need a shell on the box to put
// a game in the store. It takes TWO steps, exactly like the pack store, because
// a game carries binary assets: the bundle first, then one raw-bytes PUT per
// file. One multipart request would have been a third upload convention in a
// codebase that already has this one.

const (
	errActivityNotFound = "activity_not_found"
	errInvalidBody      = "invalid_body"
	errInvalidManifest  = "invalid_manifest"
	errInvalidID        = "invalid_activity_id"
	errInvalidAssetPath = "invalid_asset_path"
	errBundleTooLarge   = "bundle_too_large"
	errAssetTooLarge    = "asset_too_large"
	errNoMediaStore     = "no_media_store"
)

// AssetLimit caps one uploaded file. Matches what the desktop client refuses to
// read out of a folder, so a game that installs locally is a game that publishes.
const AssetLimit = 8 << 20 // 8 MiB

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
	// Where a game's art lives, by the asset path it ships under. Not the art
	// itself — the paths, so a client can ask for those two files and skip the
	// rest. Without them the only way to show a cover before installing would be
	// to download the whole game, which is precisely what looking at a store
	// page is meant to help you avoid.
	Icon  string `json:"icon,omitempty"`
	Cover string `json:"cover,omitempty"`
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
		Icon        string   `json:"icon"`
		Cover       string   `json:"cover"`
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

		// The download size matters here: a game with art is a real download,
		// and the library should say so before you commit to it. The same query
		// answers whether the art a manifest NAMES was actually shipped.
		shipped := map[string]bool{}
		if assets, aerr := ListAssets(r.Context(), h.pool, rec.ID); aerr == nil {
			entry.Assets = len(assets)
			for _, a := range assets {
				entry.AssetBytes += a.Size
				shipped[a.Path] = true
			}
		}

		var fields manifestFields
		if err := json.Unmarshal(rec.Bundle, &fields); err == nil {
			entry.Description = fields.Manifest.Description
			entry.Category = fields.Manifest.Category
			entry.Players = fields.Manifest.Players
			// Only advertise art that's really there. A manifest naming a file
			// it forgot to ship would otherwise have every client fetch a 404
			// for every listing, forever.
			if shipped[fields.Manifest.Icon] {
				entry.Icon = fields.Manifest.Icon
			}
			if shipped[fields.Manifest.Cover] {
				entry.Cover = fields.Manifest.Cover
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

// Publish → POST /activities : put a game in the store. Admin-only.
//
// The body is the whole {manifest, files} bundle the client compiles — opaque
// here, exactly as the console leaves it. Only three keys are read, and only
// because they become the listing's columns.
//
// Republishing REPLACES: same id, new bundle. That's what makes `publish-game`
// idempotent and what lets a version bump land without a delete first.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	// Bounded before it is read: an unbounded ReadAll is how one request
	// exhausts a server's memory.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBundleBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	var body struct {
		Manifest struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"manifest"`
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidManifest)
		return
	}

	id := strings.TrimSpace(body.Manifest.ID)
	name := strings.TrimSpace(body.Manifest.Name)
	version := strings.TrimSpace(body.Manifest.Version)
	// A version is required, and this is the last place it can be caught before
	// two players end up on builds that disagree about what a message means.
	if id == "" || name == "" || version == "" {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidManifest)
		return
	}
	if !ValidActivityID(id) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidID)
		return
	}
	if len(body.Files) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidManifest)
		return
	}

	if err := Upsert(r.Context(), h.pool, Record{ID: id, Name: name, Version: version, Bundle: raw}); err != nil {
		logx.Error("activities: publish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not publish activity")
		return
	}

	// Republishing starts the asset folder over, so a file removed from the game
	// stops being served instead of lingering forever. Rows first: an asset row
	// pointing at bytes that are gone is worse than bytes nobody references.
	if err := ReplaceAssets(r.Context(), h.pool, id, nil); err != nil {
		logx.Error("activities: clear asset rows %s: %v", id, err)
	}
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(id, "")); err != nil {
			logx.Error("activities: clear assets %s: %v", id, err)
		}
	}

	logx.Info("activities: published %s (%s) %s", name, id, version)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "version": version, "files": len(body.Files)})
}

// PublishAsset → PUT /activities/{id}/assets/* : one file's raw bytes. Admin-only.
//
// Called once per file after the bundle lands. The Content-Type header is taken
// at its word — the operator is the only one who can reach this.
func (h *Handler) PublishAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	// Both halves of the object key are validated BEFORE anything touches
	// storage: between them they are the whole of what could escape activities/.
	if !ValidActivityID(id) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidID)
		return
	}
	if !ValidAssetPath(path) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidAssetPath)
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, AssetLimit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errAssetTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	if len(data) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	if h.storage == nil {
		// A game that ships assets needs somewhere to put them. Say so plainly
		// rather than recording a row pointing at nothing.
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	// An asset belongs to a published game: POST /activities comes first.
	published, err := Exists(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("activities: check %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	if !published {
		httpx.WriteError(w, http.StatusNotFound, errActivityNotFound)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := h.storage.PutObject(r.Context(), AssetKey(id, path), data, contentType); err != nil {
		logx.Error("activities: upload %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}

	// Record it alongside whatever is already there — each PUT adds one file, so
	// the rows accumulate across the calls that follow a publish.
	existing, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("activities: list assets %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	next := make([]Asset, 0, len(existing)+1)
	for _, a := range existing {
		if a.Path != path {
			next = append(next, a)
		}
	}
	next = append(next, Asset{Path: path, ContentType: contentType, Size: int64(len(data))})
	if err := ReplaceAssets(r.Context(), h.pool, id, next); err != nil {
		logx.Error("activities: record asset %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"path": path, "size_bytes": len(data)})
}

// Unpublish → DELETE /activities/{id} : take a game out of the store. Admin-only.
//
// Idempotent: an id that isn't published reports removed:false rather than 404,
// so a cleanup script doesn't have to care whether it ran before.
func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !ValidActivityID(id) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidID)
		return
	}

	removed, err := Delete(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("activities: unpublish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not unpublish activity")
		return
	}
	// The asset ROWS cascade with the game; the bytes are ours to remove.
	if removed && h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(id, "")); err != nil {
			logx.Error("activities: remove assets %s: %v", id, err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"removed": removed})
}
