// Package engines serves the OFFICIAL ENGINE STORE: this server's
// operator-published game engines in the {manifest, files} bundle format. Any
// authenticated user browses the catalog and downloads bundles to install;
// PUBLISHING is admin-only — POST /engines, PUT /engines/{id}/assets/* and
// DELETE /engines/{id}, reached over HTTP so an operator can publish from the
// client.
//
// AN ENGINE IS THE CODE A GAME RUNS ON: the table, the wire, the fixed step,
// smooth online motion, controller mapping. It never owns a game's look. A game
// names the engine it needs in its own manifest (`requiresEngine`), and the
// activities listing reads that out of the stored bundle, so nothing here points
// at games. It is its own table and route family, never a `kind` on another
// store, and this package never looks across at apps, nodes or games.
//
// It is a PARALLEL COPY of internal/apps rather than shared code, which is the
// same trade `apps`, `activities` and `packs` already make: a domain does not
// reach into another domain's internals, so the shapes repeat and the coupling
// does not.
package engines

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
	errEngineNotFound   = "engine_not_found"
	errInvalidBody      = "invalid_body"
	errInvalidID        = "invalid_engine_id"
	errInvalidAssetPath = "invalid_asset_path"
	errAssetTooLarge    = "asset_too_large"
	errBundleTooLarge   = "bundle_too_large"
	errNoMediaStore     = "no_media_store"
)

type Handler struct {
	pool *pgxpool.Pool
	// Where an asset's BYTES live. The same object store profile media and
	// every other store's art use, through the same narrow interface.
	storage media.Store
}

func NewHandler(pool *pgxpool.Pool, storage media.Store) *Handler {
	return &Handler{pool: pool, storage: storage}
}

// catalogResponse is the wire shape of one store listing — light manifest
// fields only, never the files. An engine is shown on the store's Engines shelf
// with its icon, and is still also fetched automatically when a game needs it.
type catalogResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Engine is the manifest's `engine` (the engine's type, e.g. "2d"), `""`
	// when absent. The server does not judge it.
	Engine      string `json:"engine"`
	Description string `json:"description"`
	// Icon is an asset path, listed only when that asset was really uploaded;
	// omitted otherwise.
	Icon string `json:"icon,omitempty"`
	// Tags is always an array, `[]` when the manifest has none.
	Tags []string `json:"tags"`
	// Replaces is always an array, `[]` when the manifest names none; ids of
	// games this engine takes over.
	Replaces []string `json:"replaces"`
	// Assets and AssetBytes are the file count and total size the engine ships,
	// always present (0 when none).
	Assets     int       `json:"assets"`
	AssetBytes int64     `json:"asset_bytes"`
	UpdatedAt  time.Time `json:"updated_at"`
	// RequiresApp is the OLDEST app version this works on, from the manifest's
	// `requiresApp`. The client refuses to install or update onto anything
	// older and says so. Empty means no floor.
	RequiresApp string `json:"requires_app,omitempty"`
}

// List → GET /engines : the store catalog. An empty store is a 200 with an empty
// list.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	entries, err := Catalog(r.Context(), h.pool)
	if err != nil {
		logx.Error("engines: catalog: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to list engines")
		return
	}
	out := make([]catalogResponse, len(entries))
	for i, e := range entries {
		out[i] = catalogResponse{
			ID:          e.ID,
			Name:        e.Name,
			Version:     e.Version,
			Engine:      e.Engine,
			Description: e.Description,
			Icon:        e.Icon,
			Tags:        e.Tags,
			Replaces:    e.Replaces,
			Assets:      e.Assets,
			AssetBytes:  e.AssetBytes,
			UpdatedAt:   e.UpdatedAt,
			RequiresApp: e.RequiresApp,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"engines": out})
}

// Download → GET /engines/{id}/bundle : the full {manifest, files} JSON for
// install, plus the ASSET MANIFEST so an installer knows what else to fetch in
// one round trip.
//
// `assets` IS SPLICED IN BESIDE THE BUNDLE'S OWN KEYS rather than wrapped in an
// envelope, matching the app store this client already reads: the body IS the
// bundle, so a reader takes `manifest` and `files` off the top level and an
// extra key is additive. (The game store wraps because its body was an envelope
// from the first line it served.)
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := engineID(r)
	bundle, err := Bundle(r.Context(), h.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, errEngineNotFound)
		return
	}
	if err != nil {
		logx.Error("engines: bundle %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to read bundle")
		return
	}

	assets, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("engines: list assets %s: %v", id, err)
		assets = []Asset{} // the engine is still installable without its art
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

// Asset → GET /engines/{id}/assets/* : the bytes, streamed THROUGH the server.
// Never a bucket or presigned URL — the same rule profile media and every other
// store's art follow.
//
// EVERY MISS IS THE SAME CODE on the read side: an unknown engine, an id that
// isn't one, a path that isn't one, a row with no bytes behind it. A reader has
// no business learning which.
func (h *Handler) Asset(w http.ResponseWriter, r *http.Request) {
	id := engineID(r)
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if !ValidEngineID(id) || !ValidAssetPath(path) {
		httpx.WriteError(w, http.StatusNotFound, errEngineNotFound)
		return
	}

	meta, err := GetAsset(r.Context(), h.pool, id, path)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errEngineNotFound)
		return
	}
	if err != nil {
		logx.Error("engines: get asset %s/%s: %v", id, path, err)
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
			httpx.WriteError(w, http.StatusNotFound, errEngineNotFound)
			return
		}
		logx.Error("engines: read asset %s/%s: %v", id, path, err)
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
		logx.Error("engines: stream asset %s/%s: %v", id, path, err)
	}
}

// PublishAsset → PUT /engines/{id}/assets/* : one file's raw bytes. Admin-only.
//
// Called once per file AFTER the bundle lands. The Content-Type header is taken
// at its word — the operator is the only one who can reach this.
func (h *Handler) PublishAsset(w http.ResponseWriter, r *http.Request) {
	id := engineID(r)
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	// Both halves of the object key are validated BEFORE anything touches
	// storage: between them they are the whole of what could escape engines/.
	if !ValidEngineID(id) {
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
		// An engine that ships assets needs somewhere to put them. Say so plainly
		// rather than recording a row pointing at nothing.
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	// An asset belongs to a published engine: POST /engines comes first.
	published, err := Exists(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("engines: check %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}
	if !published {
		httpx.WriteError(w, http.StatusNotFound, errEngineNotFound)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := h.storage.PutObject(r.Context(), AssetKey(id, path), data, contentType); err != nil {
		logx.Error("engines: upload %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}

	// Record it alongside whatever is already there — each PUT adds one file, so
	// the rows accumulate across the calls that follow a publish.
	existing, err := ListAssets(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("engines: list assets %s: %v", id, err)
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
		logx.Error("engines: record asset %s/%s: %v", id, path, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store asset")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"path": path, "size_bytes": len(data)})
}

// engineID reads the {id} route parameter, decoded.
//
// An engine id may carry a namespace and therefore a COLON (`core:engine-2d`),
// which a client percent-encodes into the path. chi routes on the raw path
// whenever it differs from the decoded one and hands back the raw segment, so
// without this the id arrives as `core%3Aengine-2d` and every lookup misses: the
// catalogue looks fine and installing does not, which reads as a failed PUBLISH
// rather than a failed read. That bug was paid for once in the node store and
// once more copied into the app store.
//
// A decode failure keeps the raw value: a malformed escape is a lookup that
// should miss, not a request that should crash.
func engineID(r *http.Request) string {
	raw := chi.URLParam(r, "id")
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// Publish → POST /engines : upsert a {manifest, files} bundle into the store.
// ADMIN-ONLY (gated by admin.RequireAdmin at the route).
//
// The body IS the bundle, byte for byte — it's stored opaquely, so re-encoding
// it here would only risk changing what the client meant to publish.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	// Cap before reading: an unbounded ReadAll is how a server gets
	// memory-exhausted by one request.
	raw, err := io.ReadAll(io.LimitReader(r.Body, BundleLimit+1))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	if len(raw) > BundleLimit {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
		return
	}
	info, err := PublishBundle(r.Context(), h.pool, raw)
	if err != nil {
		// parseBundle's failures are all "your bundle is wrong" — report the
		// reason, since the operator is the one who can fix it.
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// REPUBLISHING STARTS THE ASSET FOLDER OVER, so a file removed from the engine
	// stops being served instead of lingering forever — which also means the
	// uploads that follow are not optional for an engine that ships art. Rows
	// first: an asset row pointing at bytes that are gone is worse than bytes
	// nobody references. Neither failure undoes the publish, which has already
	// succeeded; both are the operator's to see in the log.
	if err := ReplaceAssets(r.Context(), h.pool, info.ID, nil); err != nil {
		logx.Error("engines: clear asset rows %s: %v", info.ID, err)
	}
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(info.ID, "")); err != nil {
			logx.Error("engines: clear assets %s: %v", info.ID, err)
		}
	}

	logx.Info("engines: published %s (%s) v%s", info.ID, info.Name, info.Version)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":      info.ID,
		"name":    info.Name,
		"version": info.Version,
	})
}

// Unpublish → DELETE /engines/{id} : remove an engine from the store. ADMIN-ONLY.
// Idempotent — removing what isn't there reports `removed: false` rather than
// 404, so a retry after a dropped response isn't an error.
func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	id := engineID(r)
	removed, err := Unpublish(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("engines: unpublish %s: %v", id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "failed to unpublish")
		return
	}
	// The asset ROWS cascade with the engine; the bytes are ours to remove.
	if removed && h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), AssetKey(id, "")); err != nil {
			logx.Error("engines: remove assets %s: %v", id, err)
		}
	}
	if removed {
		logx.Info("engines: unpublished %s", id)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "removed": removed})
}
