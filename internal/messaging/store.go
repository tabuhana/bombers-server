package messaging

import (
	"context"
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

// beginFunc opens a transaction. *pgxpool.Pool's Begin satisfies it; a test
// hands in a fake.
type beginFunc func(ctx context.Context) (pgx.Tx, error)

// messageRecord is the in-memory view of a messages row. EditedAt and DeletedAt
// are nil until set; a deleted message keeps its row (a tombstone) with Body "".
// Nonce is the sender's idempotency key and is only ever written: it is not in
// messageColumns, so no read fills it in.
type messageRecord struct {
	ID          string
	Seq         int64
	SenderID    string
	RecipientID string
	Body        string
	CreatedAt   time.Time
	EditedAt    *time.Time
	DeletedAt   *time.Time
	Nonce       string
}

// messageColumns is every SELECT's and RETURNING's column list, in the order
// scanMessage reads them. One list, so a column added to the shape is added in
// one place and a query can't return a message with a field missing.
const messageColumns = `id, seq, sender_id, recipient_id, body, created_at, edited_at, deleted_at`

// rowScanner is what pgx.Row and pgx.Rows share.
type rowScanner interface {
	Scan(dest ...any) error
}

// messageDests are the Scan destinations for messageColumns, in that order. The
// one place the order is written down in Go, shared by every scan that reads a
// whole message, alone or as part of a wider row.
func messageDests(m *messageRecord) []any {
	return []any{&m.ID, &m.Seq, &m.SenderID, &m.RecipientID, &m.Body, &m.CreatedAt, &m.EditedAt, &m.DeletedAt}
}

func scanMessage(row rowScanner) (messageRecord, error) {
	var m messageRecord
	err := row.Scan(messageDests(&m)...)
	return m, err
}

// seqLockKey names the advisory lock every write that draws a message's seq takes
// first.
//
// A sequence hands out numbers in the order writes START, but a client's catch-up
// cursor is only safe if rows become VISIBLE in that same order: otherwise a
// write that drew seq 10, then lost the race to commit against a write that drew
// 11, is invisible to a client that has already read up to 11 — and the cursor
// never goes back for it. Taking this lock before nextval() and holding it to
// commit (it is a transaction-level lock) makes draw order and commit order the
// same. Writes to messages are a chat's worth of traffic, so serialising them
// costs nothing that matters. The number only has to be unique among this
// database's advisory locks; nothing else takes one.
const seqLockKey int64 = 0x6d73675f736571 // "msg_seq"

const lockSeqSQL = `SELECT pg_advisory_xact_lock($1)`

// writeSeq runs one write that draws a seq inside a transaction holding
// seqLockKey from before the number is drawn until the commit.
func writeSeq(ctx context.Context, begin beginFunc, write func(tx dbExecutor) (*messageRecord, error)) (*messageRecord, error) {
	tx, err := begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// A no-op once committed; on any early return it releases the lock.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, lockSeqSQL, seqLockKey); err != nil {
		return nil, fmt.Errorf("lock seq: %w", err)
	}
	m, err := write(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return m, nil
}

// errNoMessage means no message matched: it does not exist, it is already
// deleted, or the caller is not its sender. The three are one answer on purpose.
var errNoMessage = errors.New("message not found")

// insertMessageSQL stores a message. An empty nonce is stored as NULL, never as
// "": the unique index is partial on NOT NULL, and an empty string is a value, so
// it would make every nonce-less send after the first collide.
//
// created_at is clock_timestamp(), not the column default now(). now() is the
// instant the TRANSACTION started, and this one waits on seqLockKey before it gets
// here, so a message could be stamped before a message that committed ahead of it,
// and a reader's position, copied from the newest message they can see, could then
// sit after a message that was still on its way. clock_timestamp() is read when
// this statement runs, which is inside the lock and before the commit, so
// created_at order is seq order is commit order.
const insertMessageSQL = `
INSERT INTO messages (id, sender_id, recipient_id, body, created_at, nonce)
VALUES ($1, $2, $3, $4, clock_timestamp(), NULLIF($5::text, ''))
RETURNING ` + messageColumns + `
`

func insertMessage(ctx context.Context, db dbExecutor, m *messageRecord) (*messageRecord, error) {
	out, err := scanMessage(db.QueryRow(ctx, insertMessageSQL, m.ID, m.SenderID, m.RecipientID, m.Body, m.Nonce))
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	return &out, nil
}

// findByNonceSQL reads the message a sender already made to a recipient under a
// nonce. The idempotency key is (sender_id, recipient_id, nonce): the recipient is
// in it because a retry always has the same recipient as the send it repeats, and
// the same nonce sent to someone else is a different message that must be
// delivered. Its equality on nonce is what the partial index
// messages_sender_recipient_nonce_idx serves, and it returns the row as it is
// now: an edit or delete since the first send is part of the answer, not
// something to hide.
const findByNonceSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE sender_id = $1 AND recipient_id = $2 AND nonce = $3
`

// insertOnce stores m, unless its sender already made a message to the same
// recipient under the same nonce, in which case it returns that message and
// stores nothing. created says which happened. A message with no nonce is always
// stored.
//
// The lookup comes BEFORE the insert, and ON CONFLICT DO NOTHING would not do the
// same job: a column default is evaluated before the conflict is found, so the
// losing insert would still draw a seq from messages_seq and leave a hole in the
// catch-up numbering for a message that does not exist. It must run inside
// writeSeq, whose lock serialises every write to messages: nothing can insert the
// same (sender, recipient, nonce) between the lookup and the insert, so the unique index is
// the backstop for a writer that bypasses this path, not something this path is
// expected to trip.
func insertOnce(ctx context.Context, db dbExecutor, m *messageRecord) (saved *messageRecord, created bool, err error) {
	if m.Nonce != "" {
		existing, err := scanMessage(db.QueryRow(ctx, findByNonceSQL, m.SenderID, m.RecipientID, m.Nonce))
		if err == nil {
			return &existing, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("find message by nonce: %w", err)
		}
	}
	saved, err = insertMessage(ctx, db, m)
	if err != nil {
		return nil, false, err
	}
	return saved, true, nil
}

// ownMessageRecipientSQL names who a live message of the caller's was sent to. It
// is scoped to the sender and to a live row exactly as editMessageSQL is, so a
// message that is someone else's, missing or deleted is zero rows here too: the
// friendship gate on an edit needs the recipient, and the only person who may learn
// it is the one who sent the message.
const ownMessageRecipientSQL = `
SELECT recipient_id FROM messages
WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL
`

// ownMessageRecipient returns the recipient of a live message sent by senderID, or
// errNoMessage when there is no such message — missing, not theirs and deleted
// being one answer, as in editMessage.
func ownMessageRecipient(ctx context.Context, db dbExecutor, id, senderID string) (string, error) {
	var recipientID string
	err := db.QueryRow(ctx, ownMessageRecipientSQL, id, senderID).Scan(&recipientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNoMessage
	}
	if err != nil {
		return "", fmt.Errorf("find message recipient: %w", err)
	}
	return recipientID, nil
}

// editMessageSQL rewrites a message's body. Sender and deleted_at IS NULL are in
// the WHERE clause, so "not yours", "not there" and "already deleted" are all
// zero rows rather than three cases for the caller to tell apart. seq is drawn
// again so the catch-up feed carries the edit; id and created_at are untouched,
// which is what keeps the message where it was in the conversation.
const editMessageSQL = `
UPDATE messages
SET body = $3, edited_at = now(), seq = nextval('messages_seq')
WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL
RETURNING ` + messageColumns + `
`

func editMessage(ctx context.Context, db dbExecutor, id, senderID, body string) (*messageRecord, error) {
	out, err := scanMessage(db.QueryRow(ctx, editMessageSQL, id, senderID, body))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoMessage
	}
	if err != nil {
		return nil, fmt.Errorf("edit message: %w", err)
	}
	return &out, nil
}

// deleteMessageSQL turns a message into a tombstone. The body is overwritten, not
// flagged: a delete has to delete the words, because a flag is a promise that
// whoever reads the table next honours it.
const deleteMessageSQL = `
UPDATE messages
SET body = '', deleted_at = now(), seq = nextval('messages_seq')
WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL
RETURNING ` + messageColumns + `
`

func deleteMessage(ctx context.Context, db dbExecutor, id, senderID string) (*messageRecord, error) {
	out, err := scanMessage(db.QueryRow(ctx, deleteMessageSQL, id, senderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoMessage
	}
	if err != nil {
		return nil, fmt.Errorf("delete message: %w", err)
	}
	return &out, nil
}

// errNoCursor means the message id a client paged from is not one it may page
// from: it does not exist, or it is not part of the conversation (for the
// conversation list: not one of the caller's own messages). The cases are one
// answer so a message id can't be probed for through a cursor.
var errNoCursor = errors.New("cursor message not found")

// cursor is a position in an ordering by (created_at, id): the place of a
// message the client already holds. Paging goes strictly older than it.
type cursor struct {
	CreatedAt time.Time
	ID        string
}

// conversationPairSQL is the WHERE condition for one conversation: the
// unordered pair ($1, $2) matched in either direction, so the caller needn't
// canonicalize.
const conversationPairSQL = `((sender_id = $1 AND recipient_id = $2)
    OR (sender_id = $2 AND recipient_id = $1))`

// historyCursorSQL reads the position of message $3 within the conversation of
// $1 and $2. It ignores deleted_at on purpose: the oldest message a client holds
// may have been deleted since, and its tombstone is still a place to page from.
// Scoped to the pair, so a message id from anywhere else is the same "no such
// cursor" as one that does not exist.
const historyCursorSQL = `
SELECT created_at FROM messages
WHERE id = $3 AND ` + conversationPairSQL + `
`

func historyCursor(ctx context.Context, db dbExecutor, a, b, id string) (*cursor, error) {
	c := cursor{ID: id}
	err := db.QueryRow(ctx, historyCursorSQL, a, b, id).Scan(&c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoCursor
	}
	if err != nil {
		return nil, fmt.Errorf("history cursor: %w", err)
	}
	return &c, nil
}

// listConversationSQL pulls the newest `limit` messages exchanged between an
// unordered pair, newest first. The handler reverses the slice to ascending
// before returning, so the client renders oldest→newest.
//
// Tombstones are left out: a reader opening a conversation fresh wants the
// conversation, not the record of what was removed from it. The order is
// (created_at, id) and never seq, which an edit moves.
const listConversationSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE ` + conversationPairSQL + `
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $3
`

// listConversationBeforeSQL is the same page strictly older than a message the
// client holds: $3 is that message's created_at and $4 its id. Keyset on
// (created_at, id) rather than OFFSET, so the page doesn't shift as messages
// arrive, and served by the (sender, recipient, created_at DESC) indexes.
const listConversationBeforeSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE ` + conversationPairSQL + `
  AND deleted_at IS NULL
  AND (created_at, id) < ($3, $4)
ORDER BY created_at DESC, id DESC
LIMIT $5
`

// listConversation returns one page of a conversation, newest first: the newest
// page when before is nil, else the page strictly older than before.
func listConversation(ctx context.Context, db dbExecutor, a, b string, limit int, before *cursor) ([]messageRecord, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if before == nil {
		rows, err = db.Query(ctx, listConversationSQL, a, b, limit)
	} else {
		rows, err = db.Query(ctx, listConversationBeforeSQL, a, b, before.CreatedAt, before.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list conversation: %w", err)
	}
	defer rows.Close()

	var out []messageRecord
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

// conversationRecord is one line of the conversation list: the peer, the newest
// message of the pair that has not been deleted, how many of the peer's messages
// the caller has not read, and both read markers. A marker is a position, so each
// is two fields; both are nil until that person has read anything, and a marker
// written before positions existed has its time and a nil id.
type conversationRecord struct {
	PeerID     string
	Last       messageRecord
	Unread     int64
	MyReadAt   *time.Time
	MyReadID   *string
	PeerReadAt *time.Time
	PeerReadID *string
}

// inboxCursorSQL reads the position of message $2 in the conversation list of
// user $1. Any message the user sent or received will do — the cursor is a place
// in the ordering, not a claim that its conversation still ends there — and a
// deleted one keeps its place. A message between two other people is the same
// "no such cursor" as one that does not exist.
const inboxCursorSQL = `
SELECT created_at FROM messages
WHERE id = $2 AND (sender_id = $1 OR recipient_id = $1)
`

func inboxCursor(ctx context.Context, db dbExecutor, userID, id string) (*cursor, error) {
	c := cursor{ID: id}
	err := db.QueryRow(ctx, inboxCursorSQL, userID, id).Scan(&c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoCursor
	}
	if err != nil {
		return nil, fmt.Errorf("inbox cursor: %w", err)
	}
	return &c, nil
}

// acceptedCTE is the one definition of "the people the caller is still friends
// with" as a set, for a query to put after WITH: $1 is the caller, and the CTE
// yields one peer_id per accepted friendship. The conversation list and the
// catch-up feed both read through it, so the two can't disagree about who counts
// as a friend; areFriendsSQL is the same rule asked of one pair.
const acceptedCTE = `accepted AS (
  SELECT CASE WHEN user_a_id = $1 THEN user_b_id ELSE user_a_id END AS peer_id
  FROM friendships
  WHERE state = 'accepted' AND (user_a_id = $1 OR user_b_id = $1)
)`

// listSummariesSQL is the whole conversation list in one statement; nothing
// loops over friends asking each one a question. $1 is the caller, $2 and $3 the
// keyset position (both NULL for the first page), $4 the page size.
//
//   - accepted: the people the caller is still friends with. A conversation with
//     someone since removed or blocked is not listed, matching the friends-only
//     gate on reading one (areFriendsSQL, as a set rather than a pair).
//   - exchanged: every live message in either direction, labelled with the other
//     person. Two branches rather than an OR, so each uses its own
//     (user, user, created_at) index.
//   - newest: DISTINCT ON keeps one row per peer, the newest live message,
//     ordered (created_at, id) like history and never seq. A pair whose every
//     message was deleted has no row, so it does not appear.
//   - unread: the caller's unread tally per peer, from unreadMessagesSQL, the one
//     definition of "unread" that GET /messages/unread counts with too.
//
// The two read markers are both directions of message_reads, each a position
// (last_read_at, last_read_id): the caller's own (what they have read) and the
// peer's (what the peer has read of the caller's side). The page is keyset on the last message's (created_at, id), strictly
// older than the position given.
const listSummariesSQL = `
WITH ` + acceptedCTE + `,
exchanged AS (
  SELECT recipient_id AS peer_id, ` + messageColumns + `
  FROM messages WHERE sender_id = $1 AND deleted_at IS NULL
  UNION ALL
  SELECT sender_id AS peer_id, ` + messageColumns + `
  FROM messages WHERE recipient_id = $1 AND deleted_at IS NULL
),
newest AS (
  SELECT DISTINCT ON (peer_id) peer_id, ` + messageColumns + `
  FROM exchanged JOIN accepted USING (peer_id)
  ORDER BY peer_id, created_at DESC, id DESC
),
unread AS (
  SELECT m.sender_id AS peer_id, COUNT(*)::bigint AS n` + unreadMessagesSQL + `GROUP BY m.sender_id
)
SELECT newest.peer_id, ` + messageColumns + `,
  COALESCE(unread.n, 0), mine.last_read_at, mine.last_read_id, theirs.last_read_at, theirs.last_read_id
FROM newest
LEFT JOIN unread ON unread.peer_id = newest.peer_id
LEFT JOIN message_reads mine ON mine.user_id = $1 AND mine.peer_id = newest.peer_id
LEFT JOIN message_reads theirs ON theirs.user_id = newest.peer_id AND theirs.peer_id = $1
WHERE ($2::timestamptz IS NULL OR (newest.created_at, newest.id) < ($2, $3))
ORDER BY newest.created_at DESC, newest.id DESC
LIMIT $4
`

// listSummaries returns one page of the conversation list, newest conversation
// first: the first page when before is nil, else the page strictly older than
// before.
func listSummaries(ctx context.Context, db dbExecutor, userID string, limit int, before *cursor) ([]conversationRecord, error) {
	var at, id any // both NULL on the first page
	if before != nil {
		at, id = before.CreatedAt, before.ID
	}
	rows, err := db.Query(ctx, listSummariesSQL, userID, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []conversationRecord
	for rows.Next() {
		var c conversationRecord
		dest := append([]any{&c.PeerID}, messageDests(&c.Last)...)
		dest = append(dest, &c.Unread, &c.MyReadAt, &c.MyReadID, &c.PeerReadAt, &c.PeerReadID)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversations: %w", err)
	}
	return out, nil
}

// listUpdatesSQL is the catch-up feed: every message I sent or received, with a
// person I am still friends with, tombstones included, that changed after the
// cursor, in seq order. Two indexed reads unioned rather than one OR, so each uses
// its own (user, seq) index. Each branch is capped at the page size by itself,
// since the page is the first `limit` of the merge and neither side can contribute
// more than that. UNION ALL is exact: a message cannot be both sent and received by
// the same user (the table forbids a self-message), so no row appears twice.
//
// The feed is friends-only like every other way to read a conversation
// (areFriendsSQL gates history, acceptedCTE gates the list): without it, asking
// from since=0 would rebuild a thread the history endpoint refuses to serve. The
// peer filter sits INSIDE each branch, before its LIMIT, so a page is still a true
// prefix of the feed — filtering after the limit would let messages of a former
// friend use up the page and starve the rest. A friendship that is later
// restored brings the thread back into the feed, since nothing here remembers it
// was ever excluded.
const listUpdatesSQL = `
WITH ` + acceptedCTE + `
SELECT ` + messageColumns + ` FROM (
  (SELECT ` + messageColumns + ` FROM messages
   WHERE sender_id = $1 AND seq > $2
     AND recipient_id IN (SELECT peer_id FROM accepted)
   ORDER BY seq LIMIT $3)
  UNION ALL
  (SELECT ` + messageColumns + ` FROM messages
   WHERE recipient_id = $1 AND seq > $2
     AND sender_id IN (SELECT peer_id FROM accepted)
   ORDER BY seq LIMIT $3)
) updates
ORDER BY seq
LIMIT $3
`

func listUpdates(ctx context.Context, db dbExecutor, userID string, since int64, limit int) ([]messageRecord, error) {
	rows, err := db.Query(ctx, listUpdatesSQL, userID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list updates: %w", err)
	}
	defer rows.Close()

	var out []messageRecord
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan update: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate updates: %w", err)
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

// readPosition is a read marker: the (created_at, id) of one message, the pair
// history and the conversation list order by. At is the last_read_at column and ID
// is last_read_id. It is only ever a message's own position, copied out of the
// messages table, never a time somebody supplied or a clock reading.
type readPosition struct {
	At time.Time
	ID string
}

// markReadSQL moves the caller's ($1) read marker for the conversation with $2 up
// to the newest message $2 has sent them that is not deleted, and returns the
// marker as stored only when it MOVED. $1 is the reader and $2 the sender, in that
// order everywhere in this statement. The whole thing is one statement, so the
// message it reads and the marker it writes can't be pulled apart by a write in
// between.
//
// The position is read out of messages and never from a clock. A clock reading can
// be later than a message that is stamped but not yet committed, which would file
// that message as already read; a position copied from a message the reader can
// see can only be at or before every message that commits after it, because
// insertMessageSQL stamps created_at under the lock that orders commits. With no
// such message the SELECT yields no row, nothing is inserted, and so nothing
// advances: opening an empty or already-read conversation is not an event. The
// ORDER BY is the same (created_at, id) the unread comparison uses, and is served
// by messages_recipient_sender_idx.
//
// It is MONOTONIC: on conflict the row is updated only while the stored position
// sorts strictly before the incoming one, so a late call from a slow device, or a
// newest message that has since been deleted, can never rewind read state that is
// already further on. COALESCE gives a marker written before last_read_id existed
// the empty string, which sorts below every id, so it is replaced by any message at or
// after its time. A row the WHERE refuses is neither updated nor returned, so zero
// rows means "did not advance" — which is what lets the handler stay silent
// instead of telling the peer about a read that changed nothing. The $1/$2 casts
// keep the parameter types fixed rather than inferred from the select list.
const markReadSQL = `
INSERT INTO message_reads (user_id, peer_id, last_read_at, last_read_id)
SELECT $1::text, $2::text, m.created_at, m.id
FROM messages m
WHERE m.sender_id = $2 AND m.recipient_id = $1 AND m.deleted_at IS NULL
ORDER BY m.created_at DESC, m.id DESC
LIMIT 1
ON CONFLICT (user_id, peer_id)
DO UPDATE SET last_read_at = EXCLUDED.last_read_at, last_read_id = EXCLUDED.last_read_id
WHERE (message_reads.last_read_at, COALESCE(message_reads.last_read_id, ''))
    < (EXCLUDED.last_read_at, EXCLUDED.last_read_id)
RETURNING last_read_at, last_read_id
`

// markRead advances the caller's marker for peerID to the newest message peerID
// has sent them that is not deleted. It returns the position as stored and true
// when it moved, or false when there was nothing to read or the stored position
// was already at or past it.
func markRead(ctx context.Context, db dbExecutor, userID, peerID string) (readPosition, bool, error) {
	var pos readPosition
	err := db.QueryRow(ctx, markReadSQL, userID, peerID).Scan(&pos.At, &pos.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return readPosition{}, false, nil
	}
	if err != nil {
		return readPosition{}, false, fmt.Errorf("mark read: %w", err)
	}
	return pos, true, nil
}

// unreadMessagesSQL is the one definition of "unread": the messages the caller
// ($1) has received that sort after their read position for that sender, or all of
// them when no marker exists (the LEFT JOIN leaves last_read_at NULL). It compares
// (created_at, id) as a row against the position, never a bare timestamp, so two
// messages with the same created_at are told apart by id. COALESCE turns the NULL
// last_read_id of a marker written before positions existed into the empty string, which sorts
// below every id: that marker still means "everything up to this instant is
// read", which is why no migration rewrites those rows. A deleted message never
// counts: a badge for words that no longer exist is one nobody can clear by
// reading. It is a FROM clause for a query to count over, grouped by m.sender_id.
const unreadMessagesSQL = `
FROM messages m
LEFT JOIN message_reads r ON r.user_id = $1 AND r.peer_id = m.sender_id
WHERE m.recipient_id = $1 AND m.deleted_at IS NULL
  AND (r.last_read_at IS NULL
       OR (m.created_at, m.id) > (r.last_read_at, COALESCE(r.last_read_id, '')))
`

// unreadCountsSQL tallies the unread messages per sender. Peers with nothing
// unread don't appear. Ordered by peer for a stable response.
const unreadCountsSQL = `
SELECT m.sender_id, COUNT(*)::bigint` + unreadMessagesSQL + `GROUP BY m.sender_id
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
