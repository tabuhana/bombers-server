package packs

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
)

// The HTTP surface. Browsing, downloading and fetching assets are read-only and
// open to any authenticated user; publishing is operator-only and ADMIN-gated
// at the route, the same two-ways-into-one-operation the node store has —
// either the console's `publish-pack` or the endpoints below, so an operator
// can publish from the client instead of copying a folder onto the server box.

const (
	errPackNotFound = "pack_not_found"

	// Publish-path error codes. Machine-readable and stable: the operator's
	// client branches on these to say what to fix.
	errInvalidBody         = "invalid_body"
	errInvalidPackID       = "invalid_pack_id"
	errInvalidPackJSON     = "invalid_pack_json"
	errInvalidPackManifest = "invalid_pack_manifest"
	errTooManyThemes       = "too_many_themes"
	errInvalidAssetPath    = "invalid_asset_path"
	errBundleTooLarge      = "bundle_too_large"
	errAssetTooLarge       = "asset_too_large"
	errNoMediaStore        = "media_store_unavailable"
)

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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	// Kind is "theme" or "sound". A theme pack is values only — nothing to
	// download but its JSON — while a sound pack exists because audio has to be
	// fetched. The client groups the library by this.
	Kind       string   `json:"kind,omitempty"`
	Assets     int      `json:"assets"`
	AssetBytes int64    `json:"asset_bytes"`
	Players    *players `json:"players,omitempty"`
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
		Kind        string   `json:"kind"`
		Players     *players `json:"players"`
		// Only their COUNT is read, to infer the kind for a pack that predates
		// the `kind` field — the themes themselves stay opaque like the rest.
		Themes []struct{}     `json:"themes"`
		Theme  map[string]any `json:"theme"`
	} `json:"manifest"`
}

// packKind reports what a pack is, inferring it when the manifest doesn't say.
// Mirrors the client's rule so both halves agree: values → theme, files → sound.
func packKind(fields manifestFields, assetCount int) string {
	switch fields.Manifest.Kind {
	case "theme", "sound":
		return fields.Manifest.Kind
	}
	if len(fields.Manifest.Themes) > 0 || len(fields.Manifest.Theme) > 0 {
		return "theme"
	}
	if assetCount > 0 {
		return "sound"
	}
	// Nothing to go on — a pack with neither values nor files is a sound pack
	// waiting for its clips, which is the friendlier of the two guesses.
	return "sound"
}

// List → GET /packs : the catalogue.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	records, err := List(r.Context(), h.pool)
	if err != nil {
		logx.Error("packs: list: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list packs")
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
		// The download size matters here: a pack with art is a real download,
		// and the library should say so before you commit to it.
		if assets, aerr := ListAssets(r.Context(), h.pool, rec.ID); aerr == nil {
			entry.Assets = len(assets)
			for _, a := range assets {
				entry.AssetBytes += a.Size
			}
		}
		// After the asset count, since the count is part of the inference for a
		// pack that predates the `kind` field.
		entry.Kind = packKind(fields, entry.Assets)
		out = append(out, entry)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"packs": out})
}

// Download → GET /packs/{id}/bundle : the {manifest, files} JSON, plus the
// asset manifest so an installer knows what else to fetch in one round trip.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, err := Get(r.Context(), h.pool, id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errPackNotFound)
		return
	}
	if err != nil {
		logx.Error("packs: get: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch pack")
		return
	}

	assets, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("packs: list assets: %v", err)
		assets = []Asset{} // the pack is still installable without its art
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

// Asset → GET /packs/{id}/assets/* : the bytes, streamed THROUGH the
// server. Never a bucket or presigned URL — the same rule profile media follows.
func (h *Handler) Asset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if !ValidAssetPath(path) {
		httpx.WriteError(w, http.StatusNotFound, errPackNotFound)
		return
	}

	meta, err := GetAsset(r.Context(), h.pool, id, path)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errPackNotFound)
		return
	}
	if err != nil {
		logx.Error("packs: get asset: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch asset")
		return
	}
	if h.storage == nil {
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	body, err := h.storage.GetObject(r.Context(), AssetKey(id, path))
	if err != nil {
		if errors.Is(err, media.ErrObjectNotFound) {
			httpx.WriteError(w, http.StatusNotFound, errPackNotFound)
			return
		}
		logx.Error("packs: read asset: %v", err)
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
		logx.Error("packs: stream asset: %v", err)
	}
}

// Publish → POST /packs : publish (or republish) a pack. ADMIN-ONLY, gated by
// admin.RequireAdmin at the route. The same operation as the console's
// `publish-pack`, reached over HTTP.
//
// The body IS pack.json — the theme's variables live inside it — and it is
// stored verbatim, so re-encoding it here would only risk changing what the
// operator meant to publish. A pack carries BINARY assets, which is why this
// can't be one JSON request like the node store's publish: sounds and the
// wallpaper follow one raw-bytes PUT at a time on PublishAsset.
//
// Publishing CLEARS the previous asset set, rows and stored bytes both, so a
// republish REPLACES rather than accumulates — a sound dropped from the pack
// stops being served. That is exactly what `publish-pack` does before it
// uploads, and the reason the client must re-PUT every asset it still ships.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	// Bounded before it is read: an unbounded ReadAll is how one request
	// exhausts a server's memory.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, BundleLimit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	// The only keys the server reads: they become the denormalized listing
	// columns. Everything else in pack.json stays opaque.
	var manifest struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
		// Counted, not interpreted: a theme pack ships a FAMILY, and the cap is
		// what keeps one download from flooding the user's theme list.
		Themes []struct{} `json:"themes"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidPackJSON)
		return
	}
	id := strings.TrimSpace(manifest.ID)
	name := strings.TrimSpace(manifest.Name)
	version := strings.TrimSpace(manifest.Version)
	if id == "" || name == "" {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidPackManifest)
		return
	}
	if !ValidPackID(id) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidPackID)
		return
	}
	if len(manifest.Themes) > MaxThemesPerPack {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyThemes)
		return
	}

	if err := Upsert(r.Context(), h.pool, Record{ID: id, Name: name, Version: version, Bundle: raw}); err != nil {
		logx.Error("packs: publish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not publish pack")
		return
	}

	// Rows first: once they are gone nothing serves the old bytes, so a failure
	// to reach object storage below leaves invisible litter rather than a pack
	// that still serves files it no longer ships.
	if err := ReplaceAssets(r.Context(), h.pool, id, nil); err != nil {
		logx.Error("packs: clear assets %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not publish pack")
		return
	}
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(id, "")); err != nil {
			logx.Error("packs: clear stored assets %s: %v", id, err)
		}
	}

	logx.Info("packs: published %s (%s) %s", id, name, version)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "version": version})
}

// PublishAsset → PUT /packs/{id}/assets/* : the raw bytes of ONE asset — a
// sound or the wallpaper — as step two of an HTTP publish. ADMIN-ONLY.
//
// One asset per request, body-is-the-bytes: no multipart envelope, so a client
// needs nothing beyond the raw PUT it already uses for profile media, and a
// large pack uploads incrementally instead of as one enormous request.
func (h *Handler) PublishAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	// Both halves of the object key are validated BEFORE anything touches
	// storage: between them they are the whole of what could escape packs/.
	if !ValidPackID(id) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidPackID)
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
		// A pack that ships assets needs somewhere to put them. Say so plainly
		// rather than recording a row pointing at nothing — the console refuses
		// the same case for the same reason.
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	// An asset belongs to a published pack: POST /packs comes first. Checking is
	// also what keeps a typo'd id from leaving orphaned bytes in the bucket.
	published, err := Exists(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("packs: check %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	if !published {
		httpx.WriteError(w, http.StatusNotFound, errPackNotFound)
		return
	}

	asset := Asset{Path: path, ContentType: assetContentType(r.Header.Get("Content-Type"), data), Size: int64(len(data))}
	if err := h.storage.PutObject(r.Context(), AssetKey(id, path), data, asset.ContentType); err != nil {
		logx.Error("packs: put asset %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	if err := UpsertAsset(r.Context(), h.pool, id, asset); err != nil {
		logx.Error("packs: record asset %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, asset)
}

// Unpublish → DELETE /packs/{id} : remove a pack, its asset rows, and its
// stored bytes. ADMIN-ONLY. Idempotent — removing what isn't there reports
// `removed: false` rather than 404, so a retry after a dropped response isn't
// an error (the same contract the node store's unpublish gives).
func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !ValidPackID(id) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidPackID)
		return
	}

	removed, err := Delete(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("packs: unpublish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not unpublish pack")
		return
	}
	// The asset ROWS cascade with the pack; the bytes are ours to remove. Run it
	// even when no row was deleted — a publish interrupted between the pack and
	// its assets leaves objects behind, and this is the only thing that ever
	// clears them.
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(id, "")); err != nil {
			logx.Error("packs: remove stored assets %s: %v", id, err)
		}
	}
	if removed {
		logx.Info("packs: unpublished %s", id)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "removed": removed})
}

// assetContentType prefers what the uploader declared and falls back to
// sniffing the bytes. Unlike profile media there is no accept-list to enforce
// here — a pack ships whatever formats its author chose — so the declared type
// is metadata for serving, not a gate anything could be smuggled past.
func assetContentType(declared string, data []byte) string {
	if ct, _, err := mime.ParseMediaType(declared); err == nil && ct != "" && ct != "application/octet-stream" && len(ct) <= 128 {
		return ct
	}
	return http.DetectContentType(data)
}
