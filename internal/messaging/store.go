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
// Nonce is the sender's idempotency key. It IS stored, behind the partial unique
// index messages_sender_recipient_nonce_idx, and findByNonceSQL matches on it, but
// it is write-only in the sense that matters here: it is not in messageColumns, so no
// SELECT returns it and no read fills this field in. ReplyToID is the message this one
// answers, set at insert and never changed; PinnedAt is nil until a participant
// pins it. pinned_by is written with the pin and never read, so it has no field.
type messageRecord struct {
	ID          string
	Seq         int64
	SenderID    string
	RecipientID string
	Body        string
	CreatedAt   time.Time
	EditedAt    *time.Time
	DeletedAt   *time.Time
	ReplyToID   *string
	PinnedAt    *time.Time
	Nonce       string
	// Kind is what sort of message this is (kinds.go); Payload is what a non-text
	// kind carries, raw JSON, and nil for text and for a tombstone.
	Kind    string
	Payload []byte
}

// messageColumns is every SELECT's and RETURNING's column list, in the order
// scanMessage reads them. One list, so a column added to the shape is added in
// one place and a query can't return a message with a field missing.
const messageColumns = `id, seq, sender_id, recipient_id, body, created_at, edited_at, deleted_at, reply_to_id, pinned_at, kind, payload`

// rowScanner is what pgx.Row and pgx.Rows share.
type rowScanner interface {
	Scan(dest ...any) error
}

// messageDests are the Scan destinations for messageColumns, in that order. The
// one place the order is written down in Go, shared by every scan that reads a
// whole message, alone or as part of a wider row.
func messageDests(m *messageRecord) []any {
	return []any{&m.ID, &m.Seq, &m.SenderID, &m.RecipientID, &m.Body, &m.CreatedAt, &m.EditedAt, &m.DeletedAt, &m.ReplyToID, &m.PinnedAt, &m.Kind, &m.Payload}
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
// deleted, or the caller is not its sender (or, for a pin, not a party to it). The
// cases are one answer on purpose.
var errNoMessage = errors.New("message not found")

// errPinLimit means the conversation already holds maxPins pinned messages.
var errPinLimit = errors.New("too many pins")

// insertMessageSQL stores a message. An empty nonce is stored as NULL, never as
// "": the unique index is partial on NOT NULL, and an empty string is a value, so
// it would make every nonce-less send after the first collide. reply_to_id is the
// one place a message names another, and it is written here and nowhere else: no
// UPDATE in this file sets it, which is what makes it immutable. The caller has
// already proved the target belongs to this same pair (replyTargetInPair).
//
// created_at is clock_timestamp(), not the column default now(). now() is the
// instant the TRANSACTION started, and this one waits on seqLockKey before it gets
// here, so a message could be stamped before a message that committed ahead of it,
// and a reader's position, copied from the newest message they can see, could then
// sit after a message that was still on its way. clock_timestamp() is read when
// this statement runs, which is inside the lock and before the commit, so
// created_at order is seq order is commit order.
//
// kind and payload are written here and nowhere else, and are always spelled out:
// the column default is a backstop for rows that predate kinds, not something this
// code leans on. Whether a kind may be written, and with what payload, is
// checkKindPayload's call, made before the statement runs; the CHECK on the column
// is the last line.
const insertMessageSQL = `
INSERT INTO messages (id, sender_id, recipient_id, body, created_at, nonce, reply_to_id, kind, payload)
VALUES ($1, $2, $3, $4, clock_timestamp(), NULLIF($5::text, ''), $6::text, $7::text, $8::jsonb)
RETURNING ` + messageColumns + `
`

func insertMessage(ctx context.Context, db dbExecutor, m *messageRecord) (*messageRecord, error) {
	if err := checkKindPayload(m.Kind, m.Payload); err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	out, err := scanMessage(db.QueryRow(ctx, insertMessageSQL, m.ID, m.SenderID, m.RecipientID, m.Body, m.Nonce, m.ReplyToID, m.Kind, m.Payload))
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
//
// Only a TEXT message can be edited: an edit rewrites words, and an invite or a
// picture has none to rewrite. A card is not a body somebody typed, so a body put
// on one would be text standing in for a thing the server minted. The kind is in the
// lookup, so an attempt to edit one is the same zero rows as any other message that
// cannot be edited, and the same opaque 404.
const ownMessageRecipientSQL = `
SELECT recipient_id FROM messages
WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL AND kind = 'text'
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
WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL AND kind = 'text'
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
// whoever reads the table next honours it. The pin and the reply link go with
// them: a tombstone is not pinned, and what it answered was part of what it said.
// A message that OTHERS replied to keeps those replies, whose preview then reports
// it deleted. The payload goes with the body: an invite card or a picture is what
// the message said, and a tombstone says nothing. The kind stays, so the row is
// still what it was.
const deleteMessageSQL = `
UPDATE messages
SET body = '', payload = NULL, deleted_at = now(), pinned_at = NULL, pinned_by = NULL, reply_to_id = NULL,
    seq = nextval('messages_seq')
WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL
RETURNING ` + messageColumns + `
`

// deleteReactionsSQL removes every reaction on a message. A tombstone has nothing
// to react to, and other people's reactions to words that no longer exist are not
// something to keep around unseen.
const deleteReactionsSQL = `DELETE FROM message_reactions WHERE message_id = $1`

func deleteMessage(ctx context.Context, db dbExecutor, id, senderID string) (*messageRecord, error) {
	out, err := scanMessage(db.QueryRow(ctx, deleteMessageSQL, id, senderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoMessage
	}
	if err != nil {
		return nil, fmt.Errorf("delete message: %w", err)
	}
	if _, err := db.Exec(ctx, deleteReactionsSQL, id); err != nil {
		return nil, fmt.Errorf("delete reactions: %w", err)
	}
	return &out, nil
}

// replyTargetSQL asks whether message $3 belongs to the conversation of $1 and $2.
// It is the check that makes a reply safe: reply_to resolves to a preview of the
// original at read time, so a reply to a message of somebody else's thread would be
// a way to read one line of a conversation the caller is not in. Deleted messages
// pass on purpose: a reply to a message deleted a second ago is not an error, and
// its preview just reports the deletion.
const replyTargetSQL = `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $3 AND ` + conversationPairSQL + `)`

func replyTargetInPair(ctx context.Context, db dbExecutor, a, b, id string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, replyTargetSQL, a, b, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("check reply target: %w", err)
	}
	return ok, nil
}

// participantMessageSQL reads a live message the caller ($2) is a party to, as
// sender or as recipient. A pin belongs to the conversation, not to the author, so
// this is scoped to the two people and not to the sender; a message that is missing,
// deleted or between two other people is zero rows, one answer.
const participantMessageSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE id = $1 AND (sender_id = $2 OR recipient_id = $2) AND deleted_at IS NULL
`

func participantMessage(ctx context.Context, db dbExecutor, id, userID string) (*messageRecord, error) {
	out, err := scanMessage(db.QueryRow(ctx, participantMessageSQL, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoMessage
	}
	if err != nil {
		return nil, fmt.Errorf("find message: %w", err)
	}
	return &out, nil
}

// countPinsSQL counts the pins of the conversation of $1 and $2. A tombstone's pin
// is cleared when it is deleted, so every row counted is a live pin.
const countPinsSQL = `SELECT COUNT(*)::bigint FROM messages WHERE ` + conversationPairSQL + ` AND pinned_at IS NOT NULL`

// pinMessageSQL sets a pin and draws a fresh seq, so the pin reaches a client that
// was offline through the catch-up feed like any other change. pinned_at is
// clock_timestamp() for the reason created_at is: it is read inside the seq lock,
// so the order pins are shown in is the order they committed. pinned_at IS NULL is
// in the WHERE so this statement can never re-pin, and so never draws a seq for a
// repeat.
const pinMessageSQL = `
UPDATE messages
SET pinned_at = clock_timestamp(), pinned_by = $2, seq = nextval('messages_seq')
WHERE id = $1 AND pinned_at IS NULL AND deleted_at IS NULL
RETURNING ` + messageColumns + `
`

// unpinMessageSQL clears a pin and draws a fresh seq. pinned_at IS NOT NULL keeps a
// repeat from drawing one.
const unpinMessageSQL = `
UPDATE messages
SET pinned_at = NULL, pinned_by = NULL, seq = nextval('messages_seq')
WHERE id = $1 AND pinned_at IS NOT NULL AND deleted_at IS NULL
RETURNING ` + messageColumns + `
`

// pinMessage pins message id for userID, a party to it. It reports whether it
// changed anything: pinning what is already pinned is a success that writes nothing
// and draws no seq, because a repeat is not news. The lookup comes before the
// UPDATE for the same reason insertOnce's does, and the cap is counted here, so
// both must run inside writeSeq: its lock is what keeps two pins from both seeing
// room for one more.
func pinMessage(ctx context.Context, db dbExecutor, id, userID string) (*messageRecord, bool, error) {
	cur, err := participantMessage(ctx, db, id, userID)
	if err != nil {
		return nil, false, err
	}
	if cur.PinnedAt != nil {
		return cur, false, nil
	}
	var pinned int64
	if err := db.QueryRow(ctx, countPinsSQL, cur.SenderID, cur.RecipientID).Scan(&pinned); err != nil {
		return nil, false, fmt.Errorf("count pins: %w", err)
	}
	if pinned >= maxPins {
		return nil, false, errPinLimit
	}
	out, err := scanMessage(db.QueryRow(ctx, pinMessageSQL, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, errNoMessage
	}
	if err != nil {
		return nil, false, fmt.Errorf("pin message: %w", err)
	}
	return &out, true, nil
}

// unpinMessage clears the pin on message id for userID, a party to it, reporting
// whether it changed anything. Like pinMessage it must run inside writeSeq, and
// unpinning what is not pinned writes nothing.
func unpinMessage(ctx context.Context, db dbExecutor, id, userID string) (*messageRecord, bool, error) {
	cur, err := participantMessage(ctx, db, id, userID)
	if err != nil {
		return nil, false, err
	}
	if cur.PinnedAt == nil {
		return cur, false, nil
	}
	out, err := scanMessage(db.QueryRow(ctx, unpinMessageSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, errNoMessage
	}
	if err != nil {
		return nil, false, fmt.Errorf("unpin message: %w", err)
	}
	return &out, true, nil
}

// listPinsSQL is a conversation's pins, newest pinned first. The partial index
// messages_pinned_idx holds only pinned rows, so this and countPinsSQL read the
// handful of pins on the whole server rather than any conversation's history.
// LIMIT is the cap, so the list is bounded by construction.
const listPinsSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE ` + conversationPairSQL + `
  AND pinned_at IS NOT NULL AND deleted_at IS NULL
ORDER BY pinned_at DESC, id DESC
LIMIT $3
`

func listPins(ctx context.Context, db dbExecutor, a, b string) ([]messageRecord, error) {
	rows, err := db.Query(ctx, listPinsSQL, a, b, maxPins)
	if err != nil {
		return nil, fmt.Errorf("list pins: %w", err)
	}
	defer rows.Close()

	var out []messageRecord
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pin: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pins: %w", err)
	}
	return out, nil
}

// errReactionLimit means the message already holds as many distinct emoji
// (maxReactionEmoji), or as many reactions in all (maxReactions), as it may.
var errReactionLimit = errors.New("too many reactions")

// reactionStateSQL is what the cap and the repeat check need to know about the
// reactions on message $1 when person $3 reacts with emoji $2: how many there are in
// all, how many distinct emoji, whether anyone has used this emoji on the message,
// and whether $3 has. An aggregate with no GROUP BY is always exactly one row, and
// bool_or of nothing is NULL, which COALESCE turns into false.
const reactionStateSQL = `
SELECT COUNT(*)::bigint,
       COUNT(DISTINCT emoji)::bigint,
       COALESCE(bool_or(emoji = $2::text), false),
       COALESCE(bool_or(emoji = $2::text AND user_id = $3::text), false)
FROM message_reactions
WHERE message_id = $1
`

// reactionState is reactionStateSQL's row.
type reactionState struct {
	Total    int64
	Distinct int64
	Used     bool // anyone has used this emoji on the message
	Mine     bool // the asker has
}

func loadReactionState(ctx context.Context, db dbExecutor, messageID, userID, emoji string) (reactionState, error) {
	var s reactionState
	if err := db.QueryRow(ctx, reactionStateSQL, messageID, emoji, userID).Scan(&s.Total, &s.Distinct, &s.Used, &s.Mine); err != nil {
		return reactionState{}, fmt.Errorf("load reaction state: %w", err)
	}
	return s, nil
}

// insertReactionSQL adds one reaction. The primary key is (message_id, user_id,
// emoji), so the same person adding the same emoji twice would be a unique
// violation; addReaction never sends that, because it looks first, inside writeSeq's
// lock, and treats a repeat as nothing to do. created_at is the column's
// clock_timestamp(), read inside that lock, so the order emoji are shown in is the
// order they committed.
const insertReactionSQL = `INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, $3)`

const removeReactionSQL = `DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`

// touchMessageSQL draws a fresh seq for a live message without changing any column
// of its own. A reaction is a row in another table, so nothing about the message
// moved; the new seq is what puts it back in the catch-up feed, which is the only
// way a client that was offline learns of the reaction. Like every other statement
// that draws one it must run inside writeSeq.
const touchMessageSQL = `
UPDATE messages
SET seq = nextval('messages_seq')
WHERE id = $1 AND deleted_at IS NULL
RETURNING ` + messageColumns + `
`

func touchMessage(ctx context.Context, db dbExecutor, id string) (*messageRecord, error) {
	out, err := scanMessage(db.QueryRow(ctx, touchMessageSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoMessage
	}
	if err != nil {
		return nil, fmt.Errorf("touch message: %w", err)
	}
	return &out, nil
}

// addReaction records userID's reaction to message id, a live message they are a party
// to, and reports whether it changed anything: reacting with an emoji already used
// is a success that writes nothing and draws no seq, because a repeat is not news.
// The caps apply only to a reaction that would be new, so a repeat at the cap is
// still just a success. The lookups come before the writes and the counts are read
// here, so all of it must run inside writeSeq: its lock is what keeps two reactions
// from both seeing room for one more.
func addReaction(ctx context.Context, db dbExecutor, id, userID, emoji string) (*messageRecord, bool, error) {
	cur, err := participantMessage(ctx, db, id, userID)
	if err != nil {
		return nil, false, err
	}
	state, err := loadReactionState(ctx, db, id, userID, emoji)
	if err != nil {
		return nil, false, err
	}
	if state.Mine {
		return cur, false, nil
	}
	if state.Total >= maxReactions || (!state.Used && state.Distinct >= maxReactionEmoji) {
		return nil, false, errReactionLimit
	}
	if _, err := db.Exec(ctx, insertReactionSQL, id, userID, emoji); err != nil {
		return nil, false, fmt.Errorf("add reaction: %w", err)
	}
	out, err := touchMessage(ctx, db, id)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// removeReaction takes userID's reaction off message id, a live message they are a
// party to, reporting whether it changed anything. Removing one that is not there is
// a success that writes nothing and draws no seq. Like addReaction it runs inside
// writeSeq.
func removeReaction(ctx context.Context, db dbExecutor, id, userID, emoji string) (*messageRecord, bool, error) {
	cur, err := participantMessage(ctx, db, id, userID)
	if err != nil {
		return nil, false, err
	}
	state, err := loadReactionState(ctx, db, id, userID, emoji)
	if err != nil {
		return nil, false, err
	}
	if !state.Mine {
		return cur, false, nil
	}
	if _, err := db.Exec(ctx, removeReactionSQL, id, userID, emoji); err != nil {
		return nil, false, fmt.Errorf("remove reaction: %w", err)
	}
	out, err := touchMessage(ctx, db, id)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// replyRecord is what a reply points at, read at the moment the reply is: the
// reply's own id, and the original's id, sender, body and whether it was deleted.
type replyRecord struct {
	MessageID  string
	OriginalID string
	SenderID   string
	Body       string
	Deleted    bool
}

// replyPreviewsSQL resolves, for the messages in $1, the original each one answers.
// It is read every time and never stored, so an edit of the original shows through.
// The join insists the original is in the SAME conversation as the reply: the write
// path already guarantees it, and checking again here means the preview cannot leak
// a line from another thread even if some row were ever wrong.
const replyPreviewsSQL = `
SELECT m.id, r.id, r.sender_id, r.body, r.deleted_at IS NOT NULL
FROM messages m
JOIN messages r ON r.id = m.reply_to_id
WHERE m.id = ANY($1)
  AND ((r.sender_id = m.sender_id AND r.recipient_id = m.recipient_id)
    OR (r.sender_id = m.recipient_id AND r.recipient_id = m.sender_id))
`

func loadReplies(ctx context.Context, db dbExecutor, messageIDs []string) (map[string]replyRecord, error) {
	rows, err := db.Query(ctx, replyPreviewsSQL, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("load replies: %w", err)
	}
	defer rows.Close()

	out := map[string]replyRecord{}
	for rows.Next() {
		var r replyRecord
		if err := rows.Scan(&r.MessageID, &r.OriginalID, &r.SenderID, &r.Body, &r.Deleted); err != nil {
			return nil, fmt.Errorf("scan reply: %w", err)
		}
		out[r.MessageID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate replies: %w", err)
	}
	return out, nil
}

// reactionRecord is one emoji on one message: how many people added it, and
// whether the person asking is one of them.
type reactionRecord struct {
	MessageID string
	Emoji     string
	Count     int64
	Me        bool
}

// reactionsSQL tallies the reactions on the messages in $1 as seen by $2. Within a
// message the emoji come in the order each was first used, which is the order a
// client lays the bar out in.
const reactionsSQL = `
SELECT message_id, emoji, COUNT(*)::bigint, bool_or(user_id = $2::text)
FROM message_reactions
WHERE message_id = ANY($1)
GROUP BY message_id, emoji
ORDER BY message_id, MIN(created_at), emoji
`

func loadReactions(ctx context.Context, db dbExecutor, messageIDs []string, viewerID string) ([]reactionRecord, error) {
	rows, err := db.Query(ctx, reactionsSQL, messageIDs, viewerID)
	if err != nil {
		return nil, fmt.Errorf("load reactions: %w", err)
	}
	defer rows.Close()

	var out []reactionRecord
	for rows.Next() {
		var r reactionRecord
		if err := rows.Scan(&r.MessageID, &r.Emoji, &r.Count, &r.Me); err != nil {
			return nil, fmt.Errorf("scan reaction: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reactions: %w", err)
	}
	return out, nil
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

// listAroundOlderSQL and listAroundNewerSQL are the two halves of a page centred on a
// position: $3/$4 is the (created_at, id) of the anchor, $5 the number of rows to read.
// Older is everything AT OR BEFORE the anchor, newest first, so a live anchor is the
// first row; newer is everything strictly after it, oldest first. Each is one keyset
// read on (created_at, id) served by the pair's indexes, never seq, and each leaves
// tombstones out like history. A tombstone anchor positions the page and is simply not
// among the rows.
const listAroundOlderSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE ` + conversationPairSQL + `
  AND deleted_at IS NULL
  AND (created_at, id) <= ($3, $4)
ORDER BY created_at DESC, id DESC
LIMIT $5
`

const listAroundNewerSQL = `
SELECT ` + messageColumns + `
FROM messages
WHERE ` + conversationPairSQL + `
  AND deleted_at IS NULL
  AND (created_at, id) > ($3, $4)
ORDER BY created_at ASC, id ASC
LIMIT $5
`

// listAround reads the two halves of a page centred on anchor. It asks each side for
// one row more than the page has room for (olderN and newerN), so the caller learns
// whether there is more in that direction from whether the extra row came back. The
// older half comes back newest-first and the newer oldest-first; the extra row is
// the last of each.
func listAround(ctx context.Context, db dbExecutor, a, b string, anchor cursor, olderN, newerN int) (older, newer []messageRecord, err error) {
	older, err = readMessages(ctx, db, listAroundOlderSQL, a, b, anchor.CreatedAt, anchor.ID, olderN+1)
	if err != nil {
		return nil, nil, fmt.Errorf("list around (older): %w", err)
	}
	newer, err = readMessages(ctx, db, listAroundNewerSQL, a, b, anchor.CreatedAt, anchor.ID, newerN+1)
	if err != nil {
		return nil, nil, fmt.Errorf("list around (newer): %w", err)
	}
	return older, newer, nil
}

// readMessages runs a statement that selects messageColumns and scans every row.
func readMessages(ctx context.Context, db dbExecutor, sql string, args ...any) ([]messageRecord, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
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

// mentionableSQL narrows the candidate ids in $2 to those that are $1's accepted
// friends. It reads through acceptedCTE like the catch-up feed and the
// conversation list, so a mention is judged by the same definition of "friend".
const mentionableSQL = `
WITH ` + acceptedCTE + `
SELECT peer_id FROM accepted WHERE peer_id = ANY($2)
`

func loadMentionable(ctx context.Context, db dbExecutor, userID string, candidates []string) (map[string]bool, error) {
	rows, err := db.Query(ctx, mentionableSQL, userID, candidates)
	if err != nil {
		return nil, fmt.Errorf("load mentionable: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan mentionable: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mentionable: %w", err)
	}
	return out, nil
}

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

// searchMessagesSQL finds live messages whose words match query $2, among the
// conversations of $1 with people $1 is still accepted friends with. $3 narrows to
// one peer (NULL for all), and $4/$5 are the keyset position, strictly older than
// which the page starts (both NULL for the first page); $6 is the page size.
//
//   - Friends-only through acceptedCTE, the same set the catch-up feed and the
//     conversation list read through, so there is one definition of "my friends" and
//     a search cannot open a thread history refuses to serve. The peer filter sits
//     inside each branch before its LIMIT, as in listUpdatesSQL, so a page is a true
//     prefix of the results and a former friend's messages cannot use it up.
//   - Two indexed branches unioned instead of an OR, one for what I sent and one for
//     what I received, so each can use its own (user, user, created_at) index.
//   - body_tsv @@ websearch_to_tsquery('english', $2): websearch_to_tsquery takes
//     what a person types (quoted phrases, -exclusions) and never raises on
//     punctuation, where to_tsquery would. A query with no searchable words yields
//     an empty tsquery that matches nothing. The two-argument form names the same
//     configuration body_tsv was built with; the GIN index messages_body_tsv_idx
//     serves the @@.
//   - deleted_at IS NULL is stated even though a tombstone's body is empty and its
//     vector empty: a tombstone must not match, and that must not rest on the body
//     having been cleared.
//   - Ordered (created_at DESC, id DESC), never by relevance and never by seq, which
//     an edit or a reaction moves. The keyset is the (created_at, id) of a message
//     the client holds, like history's.
const searchMessagesSQL = `
WITH ` + acceptedCTE + `
SELECT ` + messageColumns + ` FROM (
  (SELECT ` + messageColumns + ` FROM messages
   WHERE sender_id = $1 AND recipient_id IN (SELECT peer_id FROM accepted)
     AND ($3::text IS NULL OR recipient_id = $3::text)
     AND deleted_at IS NULL
     AND body_tsv @@ websearch_to_tsquery('english', $2)
     AND ($4::timestamptz IS NULL OR (created_at, id) < ($4::timestamptz, $5::text))
   ORDER BY created_at DESC, id DESC LIMIT $6)
  UNION ALL
  (SELECT ` + messageColumns + ` FROM messages
   WHERE recipient_id = $1 AND sender_id IN (SELECT peer_id FROM accepted)
     AND ($3::text IS NULL OR sender_id = $3::text)
     AND deleted_at IS NULL
     AND body_tsv @@ websearch_to_tsquery('english', $2)
     AND ($4::timestamptz IS NULL OR (created_at, id) < ($4::timestamptz, $5::text))
   ORDER BY created_at DESC, id DESC LIMIT $6)
) hits
ORDER BY created_at DESC, id DESC
LIMIT $6
`

// searchMessages returns one page of matches, newest first: the newest page when
// before is nil, else the page strictly older than before. with, when not empty,
// narrows to that one peer.
func searchMessages(ctx context.Context, db dbExecutor, userID, query, with string, limit int, before *cursor) ([]messageRecord, error) {
	var withArg, at, id any // NULL unless given
	if with != "" {
		withArg = with
	}
	if before != nil {
		at, id = before.CreatedAt, before.ID
	}
	rows, err := db.Query(ctx, searchMessagesSQL, userID, query, withArg, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var out []messageRecord
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate results: %w", err)
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
