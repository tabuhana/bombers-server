package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The person card as published to ONE viewer.
//
// Facts are the same for every friend and live on the profile row; this is the
// other half — the notes, already narrowed to their reader before they ever
// reach the server. The client decides who sees what, because the alternative is
// a permission model that understands your writing.
//
// So `content` is opaque here in the strongest sense: stored, returned,
// never parsed. The only thing this file knows about a card is how big it is
// allowed to be.

// ErrNoCard is returned when an owner has published nothing for a viewer.
var ErrNoCard = errors.New("no published card")

// CardLimit caps one viewer's card. Notes are text somebody typed about a
// person, so this is roomy by two orders of magnitude — it exists to stop a bug
// (a runaway loop appending to a note) becoming a database problem.
const CardLimit = 1 << 20 // 1 MiB

// MaxViewersPerPublish bounds one publish. A person's friends number in the
// tens; a request carrying thousands of viewers is a client that has lost track
// of what it's doing, and the honest answer is to refuse it rather than write it.
const MaxViewersPerPublish = 500

const getCardSQL = `
SELECT content FROM published_cards
WHERE owner_id = $1 AND viewer_id = $2
`

// GetCard returns what an owner published for one viewer. Friendship is the
// caller's check — this is the storage layer.
func GetCard(ctx context.Context, db dbExecutor, ownerID, viewerID string) (json.RawMessage, error) {
	var content json.RawMessage
	err := db.QueryRow(ctx, getCardSQL, ownerID, viewerID).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoCard
	}
	if err != nil {
		return nil, fmt.Errorf("get published card: %w", err)
	}
	return content, nil
}

const deleteCardsSQL = `DELETE FROM published_cards WHERE owner_id = $1`

const insertCardSQL = `
INSERT INTO published_cards (owner_id, viewer_id, content, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (owner_id, viewer_id) DO UPDATE SET
  content    = EXCLUDED.content,
  updated_at = NOW()
`

// ReplaceCards states an owner's whole publishing intent at once: everyone in
// the map gets that content, and everyone not in it gets nothing.
//
// The delete-then-insert is what makes revocation free. Unsharing a category is
// a publish that omits somebody, and the row disappearing IS the revocation —
// there is no separate grant to remember to withdraw. One transaction, so a
// viewer is never briefly without the card they should still have.
//
// A viewer who isn't an accepted friend is dropped silently rather than
// refused: the client resolves its own groups to ids, and a group can easily
// contain someone who has since been removed. Failing the whole publish over a
// stale name would mean a card that silently stops updating for everyone else.
func ReplaceCards(ctx context.Context, pool *pgxpool.Pool, ownerID string, cards map[string]json.RawMessage) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, deleteCardsSQL, ownerID); err != nil {
		return 0, fmt.Errorf("clear published cards: %w", err)
	}

	written := 0
	for viewerID, content := range cards {
		if viewerID == ownerID {
			// Your own card is the local one. A row for yourself would be a
			// second copy of it that nothing reads.
			continue
		}
		friends, err := areFriends(ctx, tx, ownerID, viewerID)
		if err != nil {
			return 0, err
		}
		if !friends {
			continue
		}
		if _, err := tx.Exec(ctx, insertCardSQL, ownerID, viewerID, content); err != nil {
			return 0, fmt.Errorf("publish card for %s: %w", viewerID, err)
		}
		written++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit publish: %w", err)
	}
	return written, nil
}

const listCardViewersSQL = `
SELECT viewer_id FROM published_cards WHERE owner_id = $1 ORDER BY viewer_id
`

// CardViewers is who an owner currently publishes to — what a publish echoes
// back, so a client can see the stored truth rather than assume its request
// landed whole.
func CardViewers(ctx context.Context, db dbExecutor, ownerID string) ([]string, error) {
	rows, err := db.Query(ctx, listCardViewersSQL, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list card viewers: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan card viewer: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
