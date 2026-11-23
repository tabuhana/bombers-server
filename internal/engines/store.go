package engines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BundleLimit caps a published store bundle — engine sources are small text
// files; 4 MiB is the same ceiling the app and node stores carry.
const BundleLimit = 4 << 20

// dbExecutor is the subset of pgx methods the queries need; both *pgxpool.Pool
// and pgx.Tx satisfy it.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// CatalogEntry is one store listing: the denormalized columns plus the light
// display fields pulled out of the bundle's manifest at read time (engine,
// description) — never the heavy files. There is no icon and no tags: an
// engine is not browsed on a shelf, it is fetched because a game needs it.
type CatalogEntry struct {
	ID      string
	Name    string
	Version string
	// Engine is the manifest's `engine` as text (the engine's type, e.g. "2d"),
	// `""` when absent. The server never judges it.
	Engine      string
	Description string
	UpdatedAt   time.Time
	// RequiresApp is the OLDEST app version this works on, from the manifest's
	// `requiresApp`. The client refuses to install or update onto anything
	// older and says so. Empty means no floor.
	RequiresApp string
}

// BundleInfo is what publishing extracts from a {manifest, files} bundle for
// the denormalized listing columns.
type BundleInfo struct {
	ID      string
	Name    string
	Version string
}

// parseBundle validates a raw bundle: valid JSON, a manifest object with a
// non-empty id + name, and at least one string-valued file.
//
// IT DOES NOT KNOW WHAT AN ENGINE IS, and must not learn. The engine's type
// (`engine`, e.g. "2d") and everything else it declares are the client's
// vocabulary; the server stores the manifest opaquely, exactly as it does an
// app's and a game's, so a new engine type is a client release and never a
// deploy.
func parseBundle(raw []byte) (BundleInfo, error) {
	if len(raw) == 0 {
		return BundleInfo{}, errors.New("bundle is empty")
	}
	if len(raw) > BundleLimit {
		return BundleInfo{}, fmt.Errorf("bundle is %d bytes — the cap is %d", len(raw), BundleLimit)
	}
	var b struct {
		Manifest struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"manifest"`
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return BundleInfo{}, fmt.Errorf("not a valid {manifest, files} JSON bundle: %w", err)
	}
	id := strings.TrimSpace(b.Manifest.ID)
	name := strings.TrimSpace(b.Manifest.Name)
	if id == "" || name == "" {
		return BundleInfo{}, errors.New("bundle manifest needs a non-empty id and name")
	}
	// THE ID IS HALF AN OBJECT KEY. An engine ships assets under engines/<id>/<path>,
	// so an id with a separator or a dot segment in it is a path waiting to be
	// built. Checked here rather than at the upload so the refusal lands where
	// the operator is standing — at the publish.
	if !ValidEngineID(id) {
		return BundleInfo{}, fmt.Errorf("%q has characters an engine id may not use (letters, numbers, - . _ : only)", id)
	}
	if len(b.Files) == 0 {
		return BundleInfo{}, errors.New("bundle has no files (or file contents aren't strings)")
	}
	return BundleInfo{ID: id, Name: name, Version: strings.TrimSpace(b.Manifest.Version)}, nil
}

// catalogSQL lists every published engine, light fields only. engine/
// description live inside the manifest; missing ones coalesce so a minimal
// manifest still lists cleanly. `engine` is lifted as TEXT (`->>`): it is a
// short type name like "2d", and a manifest that gave something else still
// lists rather than failing the catalogue.
const catalogSQL = `
SELECT id, name, version,
       COALESCE(bundle->'manifest'->>'engine', ''),
       COALESCE(bundle->'manifest'->>'description', ''),
       updated_at,
       COALESCE(bundle->'manifest'->>'requiresApp', '')
FROM engines
ORDER BY name
`

// Catalog returns every published engine's listing metadata, ordered by name.
func Catalog(ctx context.Context, db dbExecutor) ([]CatalogEntry, error) {
	rows, err := db.Query(ctx, catalogSQL)
	if err != nil {
		return nil, fmt.Errorf("list catalog: %w", err)
	}
	defer rows.Close()

	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.Version, &e.Engine, &e.Description, &e.UpdatedAt, &e.RequiresApp); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog: %w", err)
	}
	return out, nil
}

// Bundle returns the full {manifest, files} JSON for one engine, or
// pgx.ErrNoRows for an unknown id.
func Bundle(ctx context.Context, db dbExecutor, id string) (json.RawMessage, error) {
	var bundle json.RawMessage
	if err := db.QueryRow(ctx, `SELECT bundle FROM engines WHERE id = $1`, id).Scan(&bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

const upsertSQL = `
INSERT INTO engines (id, name, version, bundle, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, version = EXCLUDED.version,
  bundle = EXCLUDED.bundle, updated_at = now()
`

// PublishBundle validates a raw {manifest, files} bundle and publishes (or
// replaces) it in the store, keyed by its manifest id.
//
// THE ID IS NOT CHECKED AGAINST ANY OTHER STORE, deliberately. Two tables, two
// ids, and the caller says which store it is asking about.
func PublishBundle(ctx context.Context, db dbExecutor, raw []byte) (BundleInfo, error) {
	info, err := parseBundle(raw)
	if err != nil {
		return BundleInfo{}, err
	}
	if _, err := db.Exec(ctx, upsertSQL, info.ID, info.Name, info.Version, raw); err != nil {
		return BundleInfo{}, fmt.Errorf("publish %s: %w", info.ID, err)
	}
	return info, nil
}

// Unpublish removes an engine from the store. Reports whether it existed.
func Unpublish(ctx context.Context, db dbExecutor, id string) (bool, error) {
	tag, err := db.Exec(ctx, `DELETE FROM engines WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("unpublish %s: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ── assets ───────────────────────────────────────────────────────────────────
//
// THE SAME ROAD AN APP'S AND A GAME'S ART TRAVELS, not another pipeline: an
// opaque bundle published first, then one raw-bytes PUT per file, bytes in
// object storage under <kind>/<id>/<path>, served back through the server.
//
// IT IS WRITTEN OUT HERE RATHER THAN IMPORTED, which is this codebase's
// standing trade (`messaging`, `profiles`, `media` and `noderecords` each own
// their own narrow `areFriends` query for the same reason, and `apps` says
// exactly this about these very functions): a domain does not reach into
// another domain's internals, so the shapes repeat and the coupling does not.
// The parts worth keeping identical are `ValidAssetPath` and the key rule, and
// both are a handful of lines whose tests live beside them.

// Asset is one file an engine ships — its manifest row, never its bytes.
type Asset struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size_bytes"`
}

// AssetLimit caps one uploaded file. Matches what the desktop client refuses to
// read out of a folder, so an engine that installs locally is an engine that publishes.
const AssetLimit = 8 << 20 // 8 MiB

// AssetKey is where an asset's bytes live in object storage. One rule, used by
// both the writer and the reader, so they can never disagree.
//
// THE ID GOES IN VERBATIM, colon and all. Sanitising it into the key would make
// two different ids able to name one object — the one mistake an object key
// must not allow. `ValidEngineID` is what keeps the key safe instead.
//
// `engines/` IS ITS OWN PREFIX, distinct from `apps/`, `nodes/` and
// `activities/`, so an engine and any other kind may carry the same id without
// their bytes colliding or one's unpublish clearing the other's.
func AssetKey(engineID, path string) string {
	return fmt.Sprintf("engines/%s/%s", engineID, path)
}

// ValidEngineID guards half of an object key.
//
// EXACTLY THE APP STORE'S RULE, because the ids are the same vocabulary: an id
// may be namespaced (`core:engine-2d`), so `:` is part of it, and `.` shows up
// in ids the Builder scaffolds. Neither separator is here, so a path cannot be built out of an id;
// `..` is refused outright, and a leading dot is refused so an id can never
// name a hidden entry in the filesystem backend.
func ValidEngineID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.HasPrefix(id, ".") || strings.Contains(id, "..") {
		return false
	}
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

// ValidAssetPath guards the other half: a path that escapes its engine's folder.
// Published bundles come from the operator, but a traversal bug would be the
// worst kind, so the rule is narrow and positive — forward slashes, no leading
// slash, no dot segments, bounded length. Character for character the app,
// node and game stores' rule, because the keys are the same shape.
func ValidAssetPath(p string) bool {
	if p == "" || len(p) > 256 {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

const existsSQL = `SELECT EXISTS (SELECT 1 FROM engines WHERE id = $1)`

// Exists reports whether an engine is published. Asset uploads check it so a
// typo'd id leaves no orphaned bytes in the store.
func Exists(ctx context.Context, db dbExecutor, id string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, existsSQL, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check engine: %w", err)
	}
	return ok, nil
}

const clearAssetsSQL = `DELETE FROM engine_assets WHERE engine_id = $1`

const insertAssetSQL = `
INSERT INTO engine_assets (engine_id, path, content_type, size_bytes, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (engine_id, path) DO UPDATE SET
  content_type = EXCLUDED.content_type,
  size_bytes   = EXCLUDED.size_bytes,
  updated_at   = NOW()
`

// ReplaceAssets records exactly the assets a publish shipped, dropping any that
// are no longer part of the engine. Passing nil clears the set, which is what a
// republish does before the uploads that follow it.
func ReplaceAssets(ctx context.Context, pool *pgxpool.Pool, engineID string, assets []Asset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin assets tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, clearAssetsSQL, engineID); err != nil {
		return fmt.Errorf("clear assets: %w", err)
	}
	for _, a := range assets {
		if _, err := tx.Exec(ctx, insertAssetSQL, engineID, a.Path, a.ContentType, a.Size); err != nil {
			return fmt.Errorf("record asset %q: %w", a.Path, err)
		}
	}
	return tx.Commit(ctx)
}

const listAssetsSQL = `
SELECT path, content_type, size_bytes
FROM engine_assets
WHERE engine_id = $1
ORDER BY path
`

// ListAssets returns an engine's asset manifest — what an installer needs to
// fetch, and how big the download is. Never nil, so an engine with no art
// serialises as an empty list rather than a null a client has to branch on.
func ListAssets(ctx context.Context, db dbExecutor, engineID string) ([]Asset, error) {
	rows, err := db.Query(ctx, listAssetsSQL, engineID)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()

	assets := []Asset{}
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.Path, &a.ContentType, &a.Size); err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

const getAssetSQL = `
SELECT content_type, size_bytes
FROM engine_assets
WHERE engine_id = $1 AND path = $2
`

// GetAsset returns one asset's metadata; the bytes come from object storage.
func GetAsset(ctx context.Context, db dbExecutor, engineID, path string) (*Asset, error) {
	a := Asset{Path: path}
	err := db.QueryRow(ctx, getAssetSQL, engineID, path).Scan(&a.ContentType, &a.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	return &a, nil
}

// ErrNotFound is returned for an asset this engine does not ship. The BUNDLE
// lookup answers `pgx.ErrNoRows`, matching the app store, so the two read the
// same way to anyone moving between them.
var ErrNotFound = errors.New("engine asset not found")
