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

// maxBundleBytes caps one published bundle. Source code, not media — assets are
// separate objects, so this stays small on purpose.
const maxBundleBytes = 4 << 20 // 4 MiB

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
	if len(rec.Bundle) > maxBundleBytes {
		return fmt.Errorf("bundle is %d bytes; the limit is %d", len(rec.Bundle), maxBundleBytes)
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
