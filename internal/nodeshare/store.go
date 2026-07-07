package nodeshare

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dbExecutor is the subset of pgx methods the queries need; both *pgxpool.Pool
// and pgx.Tx satisfy it.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// transferRecord is the in-memory view of a node_transfers row, with the
// sender's username joined in for the inbox listing. Bundle is the opaque
// client-owned {manifest, files} JSON, carried as raw bytes — the server never
// inspects it.
type transferRecord struct {
	ID             string
	SenderID       string
	SenderUsername string
	RecipientID    string
	Bundle         json.RawMessage
	CreatedAt      time.Time
}

const insertTransferSQL = `
INSERT INTO node_transfers (id, sender_id, recipient_id, bundle)
VALUES ($1, $2, $3, $4)
RETURNING created_at
`

func insertTransfer(ctx context.Context, db dbExecutor, t *transferRecord) error {
	err := db.QueryRow(ctx, insertTransferSQL, t.ID, t.SenderID, t.RecipientID, t.Bundle).
		Scan(&t.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert transfer: %w", err)
	}
	return nil
}

// listReceivedSQL is the inbox read: everything sent TO the user, newest first,
// with the sender's username so the client can show "from <friend>".
const listReceivedSQL = `
SELECT t.id, t.sender_id, u.username, t.recipient_id, t.bundle, t.created_at
FROM node_transfers t
JOIN users u ON u.id = t.sender_id
WHERE t.recipient_id = $1
ORDER BY t.created_at DESC, t.id DESC
`

func listReceived(ctx context.Context, db dbExecutor, recipientID string) ([]transferRecord, error) {
	rows, err := db.Query(ctx, listReceivedSQL, recipientID)
	if err != nil {
		return nil, fmt.Errorf("list received: %w", err)
	}
	defer rows.Close()

	var out []transferRecord
	for rows.Next() {
		var t transferRecord
		if err := rows.Scan(&t.ID, &t.SenderID, &t.SenderUsername, &t.RecipientID, &t.Bundle, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan transfer: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transfers: %w", err)
	}
	return out, nil
}

// deleteReceived removes one transfer, but only if the caller is its recipient.
// Reports whether a row was actually deleted (false → no such transfer, or not
// theirs — the handler collapses both to the same opaque 404).
const deleteReceivedSQL = `DELETE FROM node_transfers WHERE id = $1 AND recipient_id = $2`

func deleteReceived(ctx context.Context, db dbExecutor, id, recipientID string) (bool, error) {
	tag, err := db.Exec(ctx, deleteReceivedSQL, id, recipientID)
	if err != nil {
		return false, fmt.Errorf("delete transfer: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// userExists guards Send so a bundle to a nonexistent recipient collapses to
// the same opaque not-found as sending to a non-friend.
const userExistsSQL = `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`

func userExists(ctx context.Context, db dbExecutor, id string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, userExistsSQL, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check user exists: %w", err)
	}
	return ok, nil
}

// areFriends reports whether two users have an accepted friendship. Nodeshare
// owns this narrow query against the friendships table rather than importing
// internal/friends — the same loose-coupling tradeoff messaging and profiles
// already make. The pair is matched in either order.
const areFriendsSQL = `
SELECT EXISTS (
  SELECT 1 FROM friendships
  WHERE state = 'accepted'
    AND ((user_a_id = $1 AND user_b_id = $2) OR (user_a_id = $2 AND user_b_id = $1))
)
`

func areFriends(ctx context.Context, db dbExecutor, a, b string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, areFriendsSQL, a, b).Scan(&ok); err != nil {
		return false, fmt.Errorf("check friendship: %w", err)
	}
	return ok, nil
}
