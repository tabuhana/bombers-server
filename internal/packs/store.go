// Package packs is the server's PACK store: the packs this server
// publishes, which clients browse and install onto their Play screen.
//
// It mirrors the node store deliberately — an opaque {manifest, files} bundle
// the server never interprets, published by the operator through the console,
// with no HTTP publish path. The one thing packs have that nodes don't is
// ASSETS: sprites, audio, and later models. Their bytes live in object storage
// under packs/<id>/<path> and are served through the same authenticated
// pass-through as profile media — never a bucket URL.
package packs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for an unknown pack or asset.
var ErrNotFound = errors.New("pack not found")

// BundleLimit caps one published bundle. pack.json is metadata and theme
// variables, not media — assets are separate objects — so this stays small on
// purpose. Same ceiling the node store gives its bundles.
const BundleLimit = 4 << 20 // 4 MiB

// AssetLimit caps ONE asset file: a sound clip or a wallpaper, not a video.
// Both publish paths (the console's folder walk and the HTTP upload) measure
// against this, so the two can never disagree about what fits.
const AssetLimit = 8 << 20 // 8 MiB

// Record is a published pack as stored.
type Record struct {
	ID      string
	Name    string
	Version string
	Bundle  []byte // opaque {manifest, files} JSON
}

// Asset is one file a pack ships.
type Asset struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size_bytes"`
}

// AssetKey is where an asset's bytes live in object storage. One rule, used by
// both the writer and the reader, so they can never disagree.
func AssetKey(packID, path string) string {
	return fmt.Sprintf("packs/%s/%s", packID, path)
}

// ValidPackID guards the other half of an object key. The id is the FOLDER
// every one of a pack's assets lands in, and it comes out of pack.json — which,
// now that publishing is reachable over HTTP, means it comes out of a request
// body. An id carrying a slash or a dot segment would escape packs/ exactly as
// a bad asset path would, so it gets the same narrow, positive treatment.
func ValidPackID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	// No separators are possible above, so only a bare dot id could still name
	// a directory rather than a pack.
	return id != "." && id != ".."
}

// ValidAssetPath guards the one genuinely dangerous input here: a path that
// escapes its pack's folder. Published bundles come from the operator, but a
// traversal bug would be the worst kind, so the rule is narrow and positive —
// forward slashes, no leading slash, no dot segments, bounded length.
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

const upsertSQL = `
INSERT INTO packs (id, name, version, bundle, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (id) DO UPDATE SET
  name       = EXCLUDED.name,
  version    = EXCLUDED.version,
  bundle     = EXCLUDED.bundle,
  updated_at = NOW()
`

// Upsert publishes (or republishes) a pack.
func Upsert(ctx context.Context, pool *pgxpool.Pool, rec Record) error {
	if len(rec.Bundle) > BundleLimit {
		return fmt.Errorf("bundle is %d bytes; the limit is %d", len(rec.Bundle), BundleLimit)
	}
	if _, err := pool.Exec(ctx, upsertSQL, rec.ID, rec.Name, rec.Version, rec.Bundle); err != nil {
		return fmt.Errorf("publish pack: %w", err)
	}
	return nil
}

const deleteSQL = `DELETE FROM packs WHERE id = $1`

// Delete unpublishes a pack. Its asset ROWS cascade; the caller removes the
// stored bytes.
func Delete(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	tag, err := pool.Exec(ctx, deleteSQL, id)
	if err != nil {
		return false, fmt.Errorf("unpublish pack: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

const listSQL = `SELECT id, name, version, bundle FROM packs ORDER BY name`

// List returns every published pack (bundles included — the caller decides
// what to expose; the catalogue reads the manifest out and drops the files).
func List(ctx context.Context, pool *pgxpool.Pool) ([]Record, error) {
	rows, err := pool.Query(ctx, listSQL)
	if err != nil {
		return nil, fmt.Errorf("list packs: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.Name, &r.Version, &r.Bundle); err != nil {
			return nil, fmt.Errorf("sca pack: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const existsSQL = `SELECT EXISTS (SELECT 1 FROM packs WHERE id = $1)`

// Exists reports whether a pack is published without pulling its bundle — what
// an asset upload needs to know before it writes anything, since an asset only
// means something as part of a published pack.
func Exists(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	var found bool
	if err := pool.QueryRow(ctx, existsSQL, id).Scan(&found); err != nil {
		return false, fmt.Errorf("check pack: %w", err)
	}
	return found, nil
}

const getSQL = `SELECT id, name, version, bundle FROM packs WHERE id = $1`

func Get(ctx context.Context, pool *pgxpool.Pool, id string) (*Record, error) {
	var r Record
	err := pool.QueryRow(ctx, getSQL, id).Scan(&r.ID, &r.Name, &r.Version, &r.Bundle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get pack: %w", err)
	}
	return &r, nil
}

const replaceAssetsSQL = `DELETE FROM pack_assets WHERE pack_id = $1`

const insertAssetSQL = `
INSERT INTO pack_assets (pack_id, path, content_type, size_bytes, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (pack_id, path) DO UPDATE SET
  content_type = EXCLUDED.content_type,
  size_bytes   = EXCLUDED.size_bytes,
  updated_at   = NOW()
`

// ReplaceAssets records exactly the assets a publish shipped, dropping any that
// are no longer part of the pack.
func ReplaceAssets(ctx context.Context, pool *pgxpool.Pool, packID string, assets []Asset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin assets tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, replaceAssetsSQL, packID); err != nil {
		return fmt.Errorf("clear assets: %w", err)
	}
	for _, a := range assets {
		if _, err := tx.Exec(ctx, insertAssetSQL, packID, a.Path, a.ContentType, a.Size); err != nil {
			return fmt.Errorf("record asset %q: %w", a.Path, err)
		}
	}
	return tx.Commit(ctx)
}

// UpsertAsset records ONE asset. An HTTP publish uploads assets a request at a
// time, so it needs a single-row write: ReplaceAssets states the whole set at
// once and would delete everything uploaded so far.
func UpsertAsset(ctx context.Context, pool *pgxpool.Pool, packID string, a Asset) error {
	if _, err := pool.Exec(ctx, insertAssetSQL, packID, a.Path, a.ContentType, a.Size); err != nil {
		return fmt.Errorf("record asset %q: %w", a.Path, err)
	}
	return nil
}

const listAssetsSQL = `
SELECT path, content_type, size_bytes
FROM pack_assets
WHERE pack_id = $1
ORDER BY path
`

// ListAssets returns a pack's asset manifest — what an installer needs to
// fetch, and how big the download is.
func ListAssets(ctx context.Context, pool *pgxpool.Pool, packID string) ([]Asset, error) {
	rows, err := pool.Query(ctx, listAssetsSQL, packID)
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
FROM pack_assets
WHERE pack_id = $1 AND path = $2
`

// GetAsset returns one asset's metadata (the bytes come from object storage).
func GetAsset(ctx context.Context, pool *pgxpool.Pool, packID, path string) (*Asset, error) {
	a := Asset{Path: path}
	err := pool.QueryRow(ctx, getAssetSQL, packID, path).Scan(&a.ContentType, &a.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	return &a, nil
}
