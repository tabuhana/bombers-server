package noderecords

import (
	"context"
	"encoding/json"
	"errors"
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

// record is the in-memory view of a node_records row, with the owner's username
// joined in so a reader can say who it came from. Body is the client's own JSON,
// carried as raw bytes — the server never inspects it.
type record struct {
	ID            string
	NodeID        string
	OwnerID       string
	OwnerUsername string
	Body          json.RawMessage
	Audience      []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// response is one person's answer to a record, with their username for the same
// reason.
type response struct {
	RecordID       string
	AuthorID       string
	AuthorUsername string
	Body           json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const insertRecordSQL = `
INSERT INTO node_records (id, node_id, owner_id, body, audience)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at, updated_at
`

func insertRecord(ctx context.Context, db dbExecutor, rec *record) error {
	err := db.QueryRow(ctx, insertRecordSQL, rec.ID, rec.NodeID, rec.OwnerID, rec.Body, rec.Audience).
		Scan(&rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert record: %w", err)
	}
	return nil
}

// updateRecordSQL replaces the body and the audience of a record the caller
// owns. Owner-scoped in the WHERE rather than checked first, so there is no
// window between the check and the write — and so "not mine" and "not there"
// come back as one answer.
const updateRecordSQL = `
UPDATE node_records
SET body = $3, audience = $4, updated_at = now()
WHERE id = $1 AND owner_id = $2
RETURNING node_id, created_at, updated_at
`

func updateRecord(ctx context.Context, db dbExecutor, rec *record) (bool, error) {
	err := db.QueryRow(ctx, updateRecordSQL, rec.ID, rec.OwnerID, rec.Body, rec.Audience).
		Scan(&rec.NodeID, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("update record: %w", err)
	}
	return true, nil
}

// deleteRecordSQL withdraws a record. The responses go with it (ON DELETE
// CASCADE): they are answers to a question that no longer exists, and keeping
// them would leave rows nobody can read or reach.
const deleteRecordSQL = `DELETE FROM node_records WHERE id = $1 AND owner_id = $2`

func deleteRecord(ctx context.Context, db dbExecutor, id, ownerID string) (bool, error) {
	tag, err := db.Exec(ctx, deleteRecordSQL, id, ownerID)
	if err != nil {
		return false, fmt.Errorf("delete record: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// listReceivedSQL is "what this node has put in front of me", newest first.
// Scoped to one node id: a reader asks on behalf of a node, and another node's
// records are not its business.
const listReceivedSQL = `
SELECT r.id, r.node_id, r.owner_id, u.username, r.body, r.audience, r.created_at, r.updated_at
FROM node_records r
JOIN users u ON u.id = r.owner_id
WHERE r.node_id = $1 AND $2 = ANY (r.audience)
ORDER BY r.created_at DESC, r.id DESC
`

// listSentSQL is the same question from the other end: what I have sent.
// Includes records addressed to nobody — an audience can be emptied without the
// record going away.
const listSentSQL = `
SELECT r.id, r.node_id, r.owner_id, u.username, r.body, r.audience, r.created_at, r.updated_at
FROM node_records r
JOIN users u ON u.id = r.owner_id
WHERE r.node_id = $1 AND r.owner_id = $2
ORDER BY r.created_at DESC, r.id DESC
`

func listRecords(ctx context.Context, db dbExecutor, sql, nodeID, userID string) ([]record, error) {
	rows, err := db.Query(ctx, sql, nodeID, userID)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	defer rows.Close()

	var out []record
	for rows.Next() {
		var rec record
		if err := rows.Scan(&rec.ID, &rec.NodeID, &rec.OwnerID, &rec.OwnerUsername,
			&rec.Body, &rec.Audience, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate records: %w", err)
	}
	return out, nil
}

// listResponsesSQL fetches the answers to a batch of records in one go, rather
// than once per record. Oldest first, so a pile of RSVPs reads in the order
// people answered.
//
// VISIBILITY: if you can see the record, you can see its responses. The owner
// named the audience, and a "who's coming" pile only the host could read would
// make the feature pointless for everyone invited.
const listResponsesSQL = `
SELECT s.record_id, s.author_id, u.username, s.body, s.created_at, s.updated_at
FROM node_responses s
JOIN users u ON u.id = s.author_id
WHERE s.record_id = ANY ($1)
ORDER BY s.created_at ASC, s.author_id ASC
`

func listResponses(ctx context.Context, db dbExecutor, recordIDs []string) (map[string][]response, error) {
	out := make(map[string][]response)
	if len(recordIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, listResponsesSQL, recordIDs)
	if err != nil {
		return nil, fmt.Errorf("list responses: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var res response
		if err := rows.Scan(&res.RecordID, &res.AuthorID, &res.AuthorUsername,
			&res.Body, &res.CreatedAt, &res.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan response: %w", err)
		}
		out[res.RecordID] = append(out[res.RecordID], res)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate responses: %w", err)
	}
	return out, nil
}

// upsertResponseSQL writes my answer, replacing my previous one. `created_at` is
// deliberately left alone on a change: when you first answered is a fact, and
// `updated_at` says when you changed your mind.
const upsertResponseSQL = `
INSERT INTO node_responses (record_id, author_id, body)
VALUES ($1, $2, $3)
ON CONFLICT (record_id, author_id)
DO UPDATE SET body = EXCLUDED.body, updated_at = now()
RETURNING created_at, updated_at
`

func upsertResponse(ctx context.Context, db dbExecutor, res *response) error {
	err := db.QueryRow(ctx, upsertResponseSQL, res.RecordID, res.AuthorID, res.Body).
		Scan(&res.CreatedAt, &res.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert response: %w", err)
	}
	return nil
}

// deleteResponseSQL takes my answer back — which is not the same as answering
// "no". That is a body like any other; this is "I haven't said".
const deleteResponseSQL = `DELETE FROM node_responses WHERE record_id = $1 AND author_id = $2`

func deleteResponse(ctx context.Context, db dbExecutor, recordID, authorID string) (bool, error) {
	tag, err := db.Exec(ctx, deleteResponseSQL, recordID, authorID)
	if err != nil {
		return false, fmt.Errorf("delete response: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// header is the part of a record a guard or a nudge needs: whose it is, which
// node's, and who it was addressed to.
type header struct {
	NodeID   string
	OwnerID  string
	Audience []string
}

const recordHeaderSQL = `SELECT node_id, owner_id, audience FROM node_records WHERE id = $1`

// recordHeader reads one record's header, or nil when there is no such record.
func recordHeader(ctx context.Context, db dbExecutor, id string) (*header, error) {
	var h header
	err := db.QueryRow(ctx, recordHeaderSQL, id).Scan(&h.NodeID, &h.OwnerID, &h.Audience)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read record header: %w", err)
	}
	return &h, nil
}

// acceptedFriendsSQL narrows a list of user ids down to the ones that really
// are my accepted friends, in ONE query — the audience is checked as a whole, so
// a caller never learns WHICH id was the problem. Owns this narrow query against
// the friendships table rather than importing internal/friends, the same loose
// coupling messaging, profiles and nodeshare already accept.
const acceptedFriendsSQL = `
SELECT CASE WHEN user_a_id = $1 THEN user_b_id ELSE user_a_id END
FROM friendships
WHERE state = 'accepted'
  AND ((user_a_id = $1 AND user_b_id = ANY ($2)) OR (user_b_id = $1 AND user_a_id = ANY ($2)))
`

func acceptedFriends(ctx context.Context, db dbExecutor, userID string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, acceptedFriendsSQL, userID, ids)
	if err != nil {
		return nil, fmt.Errorf("check friendships: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan friendship: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate friendships: %w", err)
	}
	return out, nil
}
