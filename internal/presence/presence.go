// Package presence is who's around.
//
// One row per user, holding the status they CHOSE. Whether they are actually
// THERE is not in the table and never was worth storing: it is whether an app of
// theirs has a nudge socket open right now (internal/notify), which the server
// learns the instant it happens.
//
// It used to be a one-minute heartbeat against a three-minute staleness window,
// which meant a friend signing on took up to a minute to go green and closing
// the app left them green for three. The socket is the same signal with none of
// the lag: a connect and a disconnect are events, so this domain announces them
// to the person's friends and nobody polls for them.
//
// The one idea worth keeping straight: a stored status is a WISH, not a fact.
// What a friend actually sees is computed from the wish and the connection
// together, at read time, and never written down.
package presence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is what somebody chose to be.
type Status string

const (
	Online  Status = "online"
	Away    Status = "away"
	DND     Status = "dnd"
	Offline Status = "offline"
)

// Valid reports whether a string names a real status. A closed set, checked at
// the door: an unrecognised status stored here would read as "not offline" to
// every lookup that isn't specifically testing for one of these.
func Valid(s string) bool {
	switch Status(strings.TrimSpace(s)) {
	case Online, Away, DND, Offline:
		return true
	}
	return false
}

// Effective is what a friend actually sees, from what you chose and whether an
// app of yours is connected.
//
// Offline is honoured exactly: it means invisible AND disconnected, so a live
// socket doesn't override it. Everything else needs the socket — an `online`
// stored by a machine that has since been shut is a claim nobody is backing.
func Effective(status Status, connected bool) Status {
	if status == Offline {
		return Offline
	}
	if !connected {
		return Offline
	}
	return status
}

// Record is one person's presence as stored.
type Record struct {
	UserID    string
	Status    Status
	UpdatedAt time.Time
}

const upsertSQL = `
INSERT INTO presence (user_id, status, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  status     = EXCLUDED.status,
  updated_at = NOW()
RETURNING status, updated_at
`

// Set records a status and beats the heart in one write. The client sends this
// on a timer whether or not the status changed, which is why there is no
// separate heartbeat endpoint: two calls that must agree are two calls that can
// disagree.
func Set(ctx context.Context, pool *pgxpool.Pool, userID string, status Status) (Record, error) {
	rec := Record{UserID: userID}
	err := pool.QueryRow(ctx, upsertSQL, userID, string(status)).Scan(&rec.Status, &rec.UpdatedAt)
	if err != nil {
		return Record{}, fmt.Errorf("set presence: %w", err)
	}
	return rec, nil
}

// friendsSQL reads the presence of everyone the caller is accepted friends with.
//
// A LEFT JOIN, so a friend who has never opened the app since this shipped comes
// back as a row with no presence rather than vanishing from the list — the
// client wants "all my friends and what they are", not "the subset the server
// happens to have heard from".
//
// The friendship read is a narrow query against `friendships` rather than an
// import of internal/friends, the same loose coupling the other domains make.
const friendsSQL = `
SELECT u.id,
       COALESCE(p.status, 'offline'),
       COALESCE(p.updated_at, TO_TIMESTAMP(0))
FROM friendships f
JOIN users u
  ON u.id = CASE WHEN f.user_a_id = $1 THEN f.user_b_id ELSE f.user_a_id END
LEFT JOIN presence p ON p.user_id = u.id
WHERE f.state = 'accepted'
  AND (f.user_a_id = $1 OR f.user_b_id = $1)
`

// Friends returns the caller's friends and their EFFECTIVE statuses. `connected`
// answers for the whole list in one pass (the hub's ConnectedAmong), so the read
// is one query and one lock however many friends there are.
func Friends(ctx context.Context, pool *pgxpool.Pool, userID string, connected func([]string) map[string]bool) ([]Record, error) {
	rows, err := pool.Query(ctx, friendsSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("list friend presence: %w", err)
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var r Record
		var status string
		if err := rows.Scan(&r.UserID, &status, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan presence: %w", err)
		}
		r.Status = Status(status)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.UserID)
	}
	here := map[string]bool{}
	if connected != nil {
		here = connected(ids)
	}
	for i := range out {
		out[i].Status = Effective(out[i].Status, here[out[i].UserID])
	}
	return out, nil
}

// FriendIDs is who to tell when somebody arrives, leaves or picks a status: the
// accepted friends, and nobody else. The same join as the read above with the
// presence left off, because a fan-out wants ids and not statuses.
const friendIDsSQL = `
SELECT CASE WHEN f.user_a_id = $1 THEN f.user_b_id ELSE f.user_a_id END
FROM friendships f
WHERE f.state = 'accepted'
  AND (f.user_a_id = $1 OR f.user_b_id = $1)
`

func FriendIDs(ctx context.Context, pool *pgxpool.Pool, userID string) ([]string, error) {
	rows, err := pool.Query(ctx, friendIDsSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("list friend ids: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan friend id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Chosen is the status stored for one user, or Online when there is no row — the
// same default a client starts at. Read when a socket arrives or goes, where the
// wish is the half the hub cannot know.
func Chosen(ctx context.Context, pool *pgxpool.Pool, userID string) (Status, error) {
	var status string
	err := pool.QueryRow(ctx, `SELECT status FROM presence WHERE user_id = $1`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Online, nil
	}
	if err != nil {
		return "", fmt.Errorf("read chosen presence: %w", err)
	}
	return Status(status), nil
}
