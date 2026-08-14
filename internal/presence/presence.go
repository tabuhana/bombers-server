// Package presence is who's around.
//
// It is deliberately the smallest thing that could work: one row per user, one
// write to update it, one read to see your friends. No WebSocket, no fan-out, no
// subscription — a heartbeat every minute from a few dozen people is nothing,
// and the socket layer that would make this "live" doesn't exist yet. When it
// does, this table is still the state; the socket only saves the polling.
//
// The one idea worth keeping straight: a stored status is a WISH, not a fact.
// What a friend actually sees is computed from the wish and the heartbeat
// together, at read time, and never written down.
package presence

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// StaleAfter is how long a heartbeat lasts before its owner counts as gone.
//
// Three minutes against a one-minute heartbeat, so a laptop that slept briefly,
// a network that hiccuped, or a machine that was busy for a moment doesn't
// flicker its owner offline. The cost of being generous is that someone who
// closed the app looks present for up to three minutes; the cost of being tight
// is that people flicker, which is worse — a presence dot nobody trusts is a
// presence dot nobody reads.
const StaleAfter = 3 * time.Minute

// Effective is what a friend actually sees, from what you chose and when you
// were last heard from.
//
// Offline is honoured exactly: it means invisible AND disconnected, so a fresh
// heartbeat doesn't override it. Everything else expires — an `online` from a
// machine that stopped talking three minutes ago is a claim nobody is backing.
func Effective(status Status, updatedAt time.Time, now time.Time) Status {
	if status == Offline {
		return Offline
	}
	if now.Sub(updatedAt) > StaleAfter {
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

// Friends returns the caller's friends and their EFFECTIVE statuses.
func Friends(ctx context.Context, pool *pgxpool.Pool, userID string, now time.Time) ([]Record, error) {
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
		r.Status = Effective(Status(status), r.UpdatedAt, now)
		out = append(out, r)
	}
	return out, rows.Err()
}
