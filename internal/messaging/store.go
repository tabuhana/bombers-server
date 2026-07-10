package messaging

import (
	"context"
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

// messageRecord is the in-memory view of a messages row.
type messageRecord struct {
	ID          string
	SenderID    string
	RecipientID string
	Body        string
	CreatedAt   time.Time
}

const insertMessageSQL = `
INSERT INTO messages (id, sender_id, recipient_id, body)
VALUES ($1, $2, $3, $4)
RETURNING id, sender_id, recipient_id, body, created_at
`

func insertMessage(ctx context.Context, db dbExecutor, m *messageRecord) (*messageRecord, error) {
	var out messageRecord
	err := db.QueryRow(ctx, insertMessageSQL, m.ID, m.SenderID, m.RecipientID, m.Body).Scan(
		&out.ID, &out.SenderID, &out.RecipientID, &out.Body, &out.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	return &out, nil
}

// listConversationSQL pulls the most recent `limit` messages exchanged between
// an unordered pair, newest first. The pair is matched in either direction so
// the caller needn't canonicalize. The handler reverses the slice to ascending
// before returning, so the client renders oldest→newest.
const listConversationSQL = `
SELECT id, sender_id, recipient_id, body, created_at
FROM messages
WHERE (sender_id = $1 AND recipient_id = $2)
   OR (sender_id = $2 AND recipient_id = $1)
ORDER BY created_at DESC, id DESC
LIMIT $3
`

func listConversation(ctx context.Context, db dbExecutor, a, b string, limit int) ([]messageRecord, error) {
	rows, err := db.Query(ctx, listConversationSQL, a, b, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversation: %w", err)
	}
	defer rows.Close()

	var out []messageRecord
	for rows.Next() {
		var m messageRecord
		if err := rows.Scan(&m.ID, &m.SenderID, &m.RecipientID, &m.Body, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

// userExists guards SendMessage so a DM to a nonexistent recipient collapses to
// the same opaque not-found as messaging a non-friend.
const userExistsSQL = `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`

func userExists(ctx context.Context, db dbExecutor, id string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, userExistsSQL, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check user exists: %w", err)
	}
	return ok, nil
}

// areFriends reports whether two users have an accepted friendship. Messaging
// owns this narrow query against the friendships table rather than importing
// internal/friends — the same loose-coupling tradeoff profiles and friends
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

// UnreadEntry is one peer's unread tally: how many messages that peer sent the
// caller that the caller hasn't read yet.
type UnreadEntry struct {
	PeerID string
	Count  int64
}

// markReadSQL upserts the caller's last-read marker for a peer conversation.
// It is MONOTONIC: on conflict we keep the GREATEST of the stored and incoming
// timestamps, so a late/out-of-order call (e.g. a slow device) can never rewind
// read state that another device already advanced.
const markReadSQL = `
INSERT INTO message_reads (user_id, peer_id, last_read_at)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, peer_id)
DO UPDATE SET last_read_at = GREATEST(message_reads.last_read_at, EXCLUDED.last_read_at)
`

func markRead(ctx context.Context, db dbExecutor, userID, peerID string, at time.Time) error {
	if _, err := db.Exec(ctx, markReadSQL, userID, peerID, at); err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	return nil
}

// unreadCountsSQL tallies, per sender, how many messages the caller has received
// that are newer than their last-read marker for that sender (or all of them when
// no marker exists — the LEFT JOIN leaves last_read_at NULL). Peers with nothing
// unread don't appear. Ordered by peer for a stable response.
const unreadCountsSQL = `
SELECT m.sender_id, COUNT(*)::bigint
FROM messages m
LEFT JOIN message_reads r ON r.user_id = $1 AND r.peer_id = m.sender_id
WHERE m.recipient_id = $1 AND (r.last_read_at IS NULL OR m.created_at > r.last_read_at)
GROUP BY m.sender_id
ORDER BY m.sender_id
`

func unreadCounts(ctx context.Context, db dbExecutor, userID string) ([]UnreadEntry, error) {
	rows, err := db.Query(ctx, unreadCountsSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("unread counts: %w", err)
	}
	defer rows.Close()

	var out []UnreadEntry
	for rows.Next() {
		var e UnreadEntry
		if err := rows.Scan(&e.PeerID, &e.Count); err != nil {
			return nil, fmt.Errorf("scan unread count: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unread counts: %w", err)
	}
	return out, nil
}
