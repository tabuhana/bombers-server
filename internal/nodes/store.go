package nodes

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

// BundleLimit caps a published store bundle — node sources are small text
// files; 4 MiB is generous (same ceiling as nodeshare's friend transfers).
const BundleLimit = 4 << 20

// dbExecutor is the subset of pgx methods the queries need; both *pgxpool.Pool
// and pgx.Tx satisfy it.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// CatalogEntry is one store listing: the denormalized columns plus the light
// display fields pulled out of the bundle's manifest at read time (icon,
// description, optional tags) — never the heavy files.
type CatalogEntry struct {
	ID          string
	Name        string
	Version     string
	Icon        string
	Description string
	Tags        []string
	UpdatedAt   time.Time
	// RequiresApp is the OLDEST app version this works on, from the manifest's
	// `requiresApp`. The client refuses to install or update onto anything
	// older and says so. Empty means no floor — everything published before
	// the field existed.
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
// non-empty id + name, and at least one string-valued file. Everything else
// inside the bundle stays opaque — the client owns the manifest shape.
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
	// THE ID IS HALF AN OBJECT KEY NOW. A node ships assets under
	// nodes/<id>/<path> (see below), so an id with a separator or a dot segment
	// in it is a path waiting to be built. Checked here rather than at the upload
	// so the refusal lands where the operator is standing — at the publish — and
	// so the console and the HTTP route cannot disagree about it.
	if !ValidNodeID(id) {
		return BundleInfo{}, fmt.Errorf("%q has characters a node id may not use (letters, numbers, - . _ : only)", id)
	}
	if len(b.Files) == 0 {
		return BundleInfo{}, errors.New("bundle has no files (or file contents aren't strings)")
	}
	return BundleInfo{ID: id, Name: name, Version: strings.TrimSpace(b.Manifest.Version)}, nil
}

// catalogSQL lists every published node, light fields only. icon/description/
// tags live inside the manifest; missing ones coalesce to empty so old or
// minimal manifests still list cleanly.
const catalogSQL = `
SELECT id, name, version,
       COALESCE(bundle->'manifest'->>'icon', ''),
       COALESCE(bundle->'manifest'->>'description', ''),
       COALESCE(bundle->'manifest'->'tags', '[]'::jsonb),
       updated_at,
       COALESCE(bundle->'manifest'->>'requiresApp', '')
FROM nodes
ORDER BY name
`

// Catalog returns every published node's listing metadata, ordered by name.
func Catalog(ctx context.Context, db dbExecutor) ([]CatalogEntry, error) {
	rows, err := db.Query(ctx, catalogSQL)
	if err != nil {
		return nil, fmt.Errorf("list catalog: %w", err)
	}
	defer rows.Close()

	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		var rawTags json.RawMessage
		if err := rows.Scan(&e.ID, &e.Name, &e.Version, &e.Icon, &e.Description, &rawTags, &e.UpdatedAt, &e.RequiresApp); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		// Tags are an optional manifest nicety — a malformed value just means
		// no tags, never a failed listing.
		if err := json.Unmarshal(rawTags, &e.Tags); err != nil || e.Tags == nil {
			e.Tags = []string{}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog: %w", err)
	}
	return out, nil
}

// Bundle returns the full {manifest, files} JSON for one node, or
// pgx.ErrNoRows for an unknown id.
func Bundle(ctx context.Context, db dbExecutor, id string) (json.RawMessage, error) {
	var bundle json.RawMessage
	if err := db.QueryRow(ctx, `SELECT bundle FROM nodes WHERE id = $1`, id).Scan(&bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

const upsertSQL = `
INSERT INTO nodes (id, name, version, bundle, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, version = EXCLUDED.version,
  bundle = EXCLUDED.bundle, updated_at = now()
`

// PublishBundle validates a raw {manifest, files} bundle and publishes (or
// replaces) it in the store, keyed by its manifest id. This is the console's
// operator-publish path — there is deliberately no HTTP publish endpoint.
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

// Unpublish removes a node from the store. Reports whether it existed.
func Unpublish(ctx context.Context, db dbExecutor, id string) (bool, error) {
	tag, err := db.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("unpublish %s: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ── assets ───────────────────────────────────────────────────────────────────
//
// THE SAME ROAD A GAME'S ART TRAVELS, not a second pipeline: an opaque bundle
// published first, then one raw-bytes PUT per file, bytes in object storage
// under <kind>/<id>/<path>, served back through the server. `internal/activities`
// has had it since the game store existed; a node could ship nothing, which is
// why an app's `art` icon could not reach anybody.
//
// IT IS WRITTEN OUT HERE RATHER THAN IMPORTED, which is this codebase's standing
// trade (`messaging`, `profiles`, `media` and `noderecords` each own their own
// narrow `areFriends` query for the same reason): a domain does not reach into
// another domain's internals, so the shapes repeat and the coupling does not.
// The parts worth keeping identical are `ValidAssetPath` and the key rule, and
// both are a handful of lines whose tests live beside them.

// Asset is one file a node ships — its manifest row, never its bytes.
type Asset struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size_bytes"`
}

// AssetLimit caps one uploaded file. Matches what the desktop client refuses to
// read out of a folder, so a node that installs locally is a node that publishes.
const AssetLimit = 8 << 20 // 8 MiB

// AssetKey is where an asset's bytes live in object storage. One rule, used by
// both the writer and the reader, so they can never disagree.
//
// THE ID GOES IN VERBATIM, colon and all. A node id carries a namespace
// (`core:weather`), and sanitising it into the key would make two different ids
// able to name one object — the one mistake an object key must not allow.
// `ValidNodeID` is what keeps the key safe instead.
func AssetKey(nodeID, path string) string {
	return fmt.Sprintf("nodes/%s/%s", nodeID, path)
}

// ValidNodeID guards half of an object key.
//
// WIDER THAN A GAME'S ON PURPOSE, and exactly as wide as it has to be: a node id
// is namespaced, so `:` is part of the vocabulary (`core:weather`), and `.` shows
// up in ids the Builder scaffolds. Neither separator is here, so a path cannot be
// built out of an id; `..` is refused outright, and a leading dot is refused so
// an id can never name a hidden entry in the filesystem backend.
func ValidNodeID(id string) bool {
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

// ValidAssetPath guards the other half: a path that escapes its node's folder.
// Published bundles come from the operator, but a traversal bug would be the
// worst kind, so the rule is narrow and positive — forward slashes, no leading
// slash, no dot segments, bounded length. Character for character the game
// store's rule, because the two keys are the same shape.
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

const existsSQL = `SELECT EXISTS (SELECT 1 FROM nodes WHERE id = $1)`

// Exists reports whether a node is published. Asset uploads check it so a typo'd
// id leaves no orphaned bytes in the store.
func Exists(ctx context.Context, db dbExecutor, id string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, existsSQL, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check node: %w", err)
	}
	return ok, nil
}

const clearAssetsSQL = `DELETE FROM node_assets WHERE node_id = $1`

const insertAssetSQL = `
INSERT INTO node_assets (node_id, path, content_type, size_bytes, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (node_id, path) DO UPDATE SET
  content_type = EXCLUDED.content_type,
  size_bytes   = EXCLUDED.size_bytes,
  updated_at   = NOW()
`

// ReplaceAssets records exactly the assets a publish shipped, dropping any that
// are no longer part of the node. Passing nil clears the set, which is what a
// republish does before the uploads that follow it.
func ReplaceAssets(ctx context.Context, pool *pgxpool.Pool, nodeID string, assets []Asset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin assets tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, clearAssetsSQL, nodeID); err != nil {
		return fmt.Errorf("clear assets: %w", err)
	}
	for _, a := range assets {
		if _, err := tx.Exec(ctx, insertAssetSQL, nodeID, a.Path, a.ContentType, a.Size); err != nil {
			return fmt.Errorf("record asset %q: %w", a.Path, err)
		}
	}
	return tx.Commit(ctx)
}

const listAssetsSQL = `
SELECT path, content_type, size_bytes
FROM node_assets
WHERE node_id = $1
ORDER BY path
`

// ListAssets returns a node's asset manifest — what an installer needs to fetch,
// and how big the download is. Never nil, so a node with no art serialises as an
// empty list rather than a null a client has to branch on.
func ListAssets(ctx context.Context, db dbExecutor, nodeID string) ([]Asset, error) {
	rows, err := db.Query(ctx, listAssetsSQL, nodeID)
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
FROM node_assets
WHERE node_id = $1 AND path = $2
`

// GetAsset returns one asset's metadata; the bytes come from object storage.
func GetAsset(ctx context.Context, db dbExecutor, nodeID, path string) (*Asset, error) {
	a := Asset{Path: path}
	err := db.QueryRow(ctx, getAssetSQL, nodeID, path).Scan(&a.ContentType, &a.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	return &a, nil
}

// ErrNotFound is returned for an asset this node does not ship. The BUNDLE
// lookup still answers `pgx.ErrNoRows`, because that is what its one caller has
// always read and changing it would be a rename wearing a bug's clothes.
var ErrNotFound = errors.New("node asset not found")
