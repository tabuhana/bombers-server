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
       updated_at
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
		if err := rows.Scan(&e.ID, &e.Name, &e.Version, &e.Icon, &e.Description, &rawTags, &e.UpdatedAt); err != nil {
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
