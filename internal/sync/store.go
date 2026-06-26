package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// itemRecord is the in-memory view of a published_items row. Content is the
// opaque client-owned JSON blob, carried as raw bytes (the server never inspects
// it). UpdatedAt is the client's timestamp; ServerUpdatedAt is when the server
// last wrote the row.
type itemRecord struct {
	ID              string
	Type            string
	Content         json.RawMessage
	UpdatedAt       time.Time
	Deleted         bool
	ServerUpdatedAt time.Time
}

// upsertResult reports what a single push did: "applied" when the server took
// the client's version (insert or newer overwrite), "stale" when the server kept
// a newer-or-equal stored version and the client should pull it.
type upsertResult struct {
	ID     string
	Status string
}

const (
	statusApplied = "applied"
	statusStale   = "stale"
)

// upsertItemSQL applies last-write-wins by the client's updated_at. On conflict
// it overwrites ONLY when the incoming updated_at is newer-or-equal; otherwise
// the DO UPDATE's WHERE is false, no row is written, and RETURNING yields
// nothing — which the caller reads as "stale" (server kept a newer copy).
const upsertItemSQL = `
INSERT INTO published_items (owner_id, id, type, content, updated_at, deleted, server_updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (owner_id, id) DO UPDATE SET
  type              = EXCLUDED.type,
  content           = EXCLUDED.content,
  updated_at        = EXCLUDED.updated_at,
  deleted           = EXCLUDED.deleted,
  server_updated_at = NOW()
WHERE EXCLUDED.updated_at >= published_items.updated_at
RETURNING id
`

// pushItems upserts a batch in one transaction (all-or-nothing) and returns a
// per-item applied/stale result, plus the server time the push completed at.
// Bumping sync_state records the account's "last synced" for cross-device "last
// synced X ago".
func pushItems(ctx context.Context, pool *pgxpool.Pool, ownerID string, items []itemRecord) ([]upsertResult, time.Time, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("begin push: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a no-op

	results := make([]upsertResult, len(items))
	for i, it := range items {
		var returnedID string
		err := tx.QueryRow(ctx, upsertItemSQL,
			ownerID, it.ID, it.Type, it.Content, it.UpdatedAt, it.Deleted,
		).Scan(&returnedID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			results[i] = upsertResult{ID: it.ID, Status: statusStale}
		case err != nil:
			return nil, time.Time{}, fmt.Errorf("upsert item %s: %w", it.ID, err)
		default:
			results[i] = upsertResult{ID: it.ID, Status: statusApplied}
		}
	}

	var syncedAt time.Time
	if err := tx.QueryRow(ctx, touchSyncStateSQL, ownerID).Scan(&syncedAt); err != nil {
		return nil, time.Time{}, fmt.Errorf("touch sync state: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, time.Time{}, fmt.Errorf("commit push: %w", err)
	}
	return results, syncedAt, nil
}

const touchSyncStateSQL = `
INSERT INTO sync_state (owner_id, last_synced_at)
VALUES ($1, NOW())
ON CONFLICT (owner_id) DO UPDATE SET last_synced_at = NOW()
RETURNING last_synced_at
`

// pullItems returns the owner's items, optionally only those the server has
// written since `since` (incremental refresh). Tombstones (deleted=true) ARE
// included so deletes propagate to other devices; the client applies them.
// Ordered by server_updated_at ascending so the client can advance its cursor.
const pullItemsSQL = `
SELECT id, type, content, updated_at, deleted, server_updated_at
FROM published_items
WHERE owner_id = $1
  AND ($2::timestamptz IS NULL OR server_updated_at > $2)
ORDER BY server_updated_at ASC
`

func pullItems(ctx context.Context, pool *pgxpool.Pool, ownerID string, since *time.Time) ([]itemRecord, error) {
	rows, err := pool.Query(ctx, pullItemsSQL, ownerID, since)
	if err != nil {
		return nil, fmt.Errorf("pull items: %w", err)
	}
	defer rows.Close()

	var out []itemRecord
	for rows.Next() {
		var it itemRecord
		if err := rows.Scan(&it.ID, &it.Type, &it.Content, &it.UpdatedAt, &it.Deleted, &it.ServerUpdatedAt); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate items: %w", err)
	}
	return out, nil
}

// syncStatus is the account's sync bookkeeping: when it last synced (nil if
// never) and how many live (non-deleted) items it has on the server.
type syncStatus struct {
	LastSyncedAt *time.Time
	ItemCount    int
}

const syncStatusSQL = `
SELECT
  (SELECT last_synced_at FROM sync_state WHERE owner_id = $1),
  (SELECT COUNT(*) FROM published_items WHERE owner_id = $1 AND deleted = FALSE)
`

func getSyncStatus(ctx context.Context, pool *pgxpool.Pool, ownerID string) (*syncStatus, error) {
	var s syncStatus
	if err := pool.QueryRow(ctx, syncStatusSQL, ownerID).Scan(&s.LastSyncedAt, &s.ItemCount); err != nil {
		return nil, fmt.Errorf("sync status: %w", err)
	}
	return &s, nil
}
