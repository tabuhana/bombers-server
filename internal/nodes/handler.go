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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
)

const (
	errNodeNotFound     = "node_not_found"
	errInvalidBody      = "invalid_body"
	errInvalidID        = "invalid_node_id"
	errInvalidAssetPath = "invalid_asset_path"
	errAssetTooLarge    = "asset_too_large"
	errNoMediaStore     = "no_media_store"
)

type Handler struct {
	pool *pgxpool.Pool
	// Where an asset's BYTES live. The same object store profile media and a
	// game's art use, through the same narrow interface — a node store that
	// could not ship a file is what this field ended.
	storage media.Store
}

func NewHandler(pool *pgxpool.Pool, storage media.Store) *Handler {
	return &Handler{pool: pool, storage: storage}
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
	// RequiresApp is the OLDEST app version this works on, from the manifest's
	// `requiresApp`. The client refuses to install or update onto anything
	// older and says so. Empty means no floor — everything published before
	// the field existed.
	RequiresApp string `json:"requires_app,omitempty"`
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
			RequiresApp: e.RequiresApp,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// Download → GET /nodes/{id}/bundle : the full {manifest, files} JSON for
// install, plus the ASSET MANIFEST so an installer knows what else to fetch in
// one round trip. (The /bundle suffix keeps this route clear of nodeshare's
// static /nodes/received.)
//
// `assets` IS SPLICED IN BESIDE THE BUNDLE'S OWN KEYS, and that shape was chosen
// over the game store's `{bundle, assets}` envelope for one reason: this route's
// body has always BEEN the bundle, so wrapping it would break every client
// already installed — they read `manifest` and `files` off the top level. An
// extra key is additive: an old client ignores it, a new one reads it. (The game
// store wraps because its body was an envelope from the first line it served.)
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := nodeID(r)
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

	assets, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("nodes: list assets %s: %v", id, err)
		assets = []Asset{} // the node is still installable without its art
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(withAssets(bundle, assets))
}

// withAssets adds one key to a bundle object without re-encoding what is in it.
//
// A bundle that is not a JSON object comes back UNTOUCHED rather than wrapped or
// refused: `parseBundle` means that cannot happen on the way in, and a reader
// that invented a shape for an unparseable row would be the one place a store
// read could lie about what was published.
func withAssets(bundle json.RawMessage, assets []Asset) []byte {
	payload, err := json.Marshal(map[string]any{"assets": assets})
	if err != nil {
		return bundle
	}
	body := []byte(strings.TrimSpace(string(bundle)))
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return bundle
	}
	// An empty object has nothing to put a comma after.
	if len(strings.TrimSpace(string(body[1:len(body)-1]))) == 0 {
		return payload
	}
	out := make([]byte, 0, len(body)+len(payload))
	out = append(out, body[:len(body)-1]...)
	out = append(out, ',')
	out = append(out, payload[1:]...) // drop the opening brace, keep "assets":[…]}
	return out
}

// Asset → GET /nodes/{id}/assets/* : the bytes, streamed THROUGH the server.
// Never a bucket or presigned URL — the same rule profile media and a game's art
// follow.
func (h *Handler) Asset(w http.ResponseWriter, r *http.Request) {
	id := nodeID(r)
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if !ValidNodeID(id) || !ValidAssetPath(path) {
		httpx.WriteError(w, http.StatusNotFound, errNodeNotFound)
		return
	}

	meta, err := GetAsset(r.Context(), h.pool, id, path)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errNodeNotFound)
		return
	}
	if err != nil {
		logx.Error("nodes: get asset %s/%s: %v", id, path, err)
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
			httpx.WriteError(w, http.StatusNotFound, errNodeNotFound)
			return
		}
		logx.Error("nodes: read asset %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch asset")
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	// Assets are immutable for a given published version, and the client writes
	// them to disk at install anyway; this only spares a re-download when it
	// doesn't.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if _, err := io.Copy(w, body); err != nil {
		logx.Error("nodes: stream asset %s/%s: %v", id, path, err)
	}
}

// PublishAsset → PUT /nodes/{id}/assets/* : one file's raw bytes. Admin-only.
//
// Called once per file AFTER the bundle lands, exactly as a game's art is. The
// Content-Type header is taken at its word — the operator is the only one who
// can reach this.
func (h *Handler) PublishAsset(w http.ResponseWriter, r *http.Request) {
	id := nodeID(r)
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	// Both halves of the object key are validated BEFORE anything touches
	// storage: between them they are the whole of what could escape nodes/.
	if !ValidNodeID(id) {
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
		// A node that ships assets needs somewhere to put them. Say so plainly
		// rather than recording a row pointing at nothing.
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	// An asset belongs to a published node: POST /nodes comes first.
	published, err := Exists(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("nodes: check %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	if !published {
		httpx.WriteError(w, http.StatusNotFound, errNodeNotFound)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := h.storage.PutObject(r.Context(), AssetKey(id, path), data, contentType); err != nil {
		logx.Error("nodes: upload %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}

	// Record it alongside whatever is already there — each PUT adds one file, so
	// the rows accumulate across the calls that follow a publish.
	existing, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("nodes: list assets %s: %v", id, err)
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
		logx.Error("nodes: record asset %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"path": path, "size_bytes": len(data)})
}

// Publish → POST /nodes : upsert a {manifest, files} bundle into the store.
// ADMIN-ONLY (gated by admin.RequireAdmin at the route). This is the same
// operation the console's `publish` performs, reached over HTTP so an operator
// can publish from the client instead of copying a bundle onto the server box
// and typing at its prompt.
//
// The body IS the bundle, byte for byte — it's stored opaquely, so re-encoding
// it here would only risk changing what the client meant to publish.
// nodeID reads the {id} route parameter, decoded.
//
// A node id carries a namespace and therefore a COLON — `core:weather` — which
// a client percent-encodes into the path. chi routes on the raw path whenever
// it differs from the decoded one, and hands back the raw segment, so this
// arrived as `core%3Aweather` and every lookup missed. The catalogue was fine
// (no id in the URL) and installing was not, which made it look like the
// PUBLISH had failed rather than the read.
//
// A decode failure keeps the raw value: a malformed escape is a lookup that
// should miss, not a request that should crash.
func nodeID(r *http.Request) string {
	raw := chi.URLParam(r, "id")
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

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
	// REPUBLISHING STARTS THE ASSET FOLDER OVER, so a file removed from the node
	// stops being served instead of lingering forever — which also means the
	// uploads that follow are not optional for a node that ships art. Rows
	// first: an asset row pointing at bytes that are gone is worse than bytes
	// nobody references. Neither failure undoes the publish, which has already
	// succeeded; both are the operator's to see in the log.
	if err := ReplaceAssets(r.Context(), h.pool, info.ID, nil); err != nil {
		logx.Error("nodes: clear asset rows %s: %v", info.ID, err)
	}
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(info.ID, "")); err != nil {
			logx.Error("nodes: clear assets %s: %v", info.ID, err)
		}
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
	id := nodeID(r)
	removed, err := Unpublish(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("nodes: unpublish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to unpublish")
		return
	}
	// The asset ROWS cascade with the node; the bytes are ours to remove.
	if removed && h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(id, "")); err != nil {
			logx.Error("nodes: remove assets %s: %v", id, err)
		}
	}
	if removed {
		logx.Info("nodes: unpublished %s", id)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "removed": removed})
}
