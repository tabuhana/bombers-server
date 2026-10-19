package messaging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// There is no Postgres in these tests, on purpose. fakeDB satisfies dbExecutor and
// answers each query this package makes by doing in Go what that query's SQL
// says. That makes it a check on the handlers' wiring — who is asked, what is
// written, what is pushed — and NOT on the SQL itself, which no Go fake can
// execute. So the clauses the SQL has to contain are pinned separately, as text,
// in the TestSQL* tests below: a fake that agreed with a query that had lost its
// `deleted_at IS NULL` would prove nothing.

// call is one statement the fake was asked to run.
type call struct {
	sql  string
	args []any
	inTx bool
}

type fakeDB struct {
	mu sync.Mutex

	users   map[string]bool
	friends map[[2]string]bool
	msgs    []messageRecord
	reads   map[[2]string]readMark // keyed (reader, peer)

	seq   int64
	clock time.Time
	now   time.Time // the database's wall clock: no read-state statement may consult it, so a test sets it far from every message to prove none does

	calls       []call
	commits     int
	rollbacks   int
	beginErr    error
	failOnQuery string // an SQL constant whose execution returns an error
}

func newFakeDB(users ...string) *fakeDB {
	db := &fakeDB{
		users:   map[string]bool{},
		friends: map[[2]string]bool{},
		reads:   map[[2]string]readMark{},
		clock:   time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	}
	db.now = db.clock.Add(48 * time.Hour) // later than anything a test seeds: the decoy a clock-based marker would have taken
	for _, u := range users {
		db.users[u] = true
	}
	return db
}

// readMark is a message_reads row as the fake keeps it. An empty id is the NULL
// last_read_id of a marker written before positions existed.
type readMark struct {
	at time.Time
	id string
}

// sortsAfter is the unread comparison: (created_at, id) > (at, id), an absent id sorting below every id.
func sortsAfter(m messageRecord, mark readMark) bool {
	if !m.CreatedAt.Equal(mark.at) {
		return m.CreatedAt.After(mark.at)
	}
	return m.ID > mark.id
}

// unreadFor is unreadMessagesSQL for one received message: unread when it is not
// deleted and either no marker exists or it sorts after the marker.
func (d *fakeDB) unreadFor(m messageRecord, reader string) bool {
	if m.RecipientID != reader || m.DeletedAt != nil {
		return false
	}
	mark, ok := d.reads[[2]string{reader, m.SenderID}]
	return !ok || sortsAfter(m, mark)
}

func pair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (d *fakeDB) befriend(a, b string) { d.friends[pair(a, b)] = true }

// seed stores a message the way insertMessageSQL would, with a chosen id.
func (d *fakeDB) seed(id, sender, recipient, body string) messageRecord {
	d.seq++
	m := messageRecord{
		ID: id, Seq: d.seq, SenderID: sender, RecipientID: recipient, Body: body,
		CreatedAt: d.clock.Add(time.Duration(d.seq) * time.Minute),
	}
	d.msgs = append(d.msgs, m)
	return m
}

// seedAt stores a message created at a chosen moment, for the cases where two
// messages must share one.
func (d *fakeDB) seedAt(id, sender, recipient, body string, at time.Time) messageRecord {
	m := d.seed(id, sender, recipient, body)
	d.find(id).CreatedAt = at
	m.CreatedAt = at
	return m
}

func (d *fakeDB) find(id string) *messageRecord {
	for i := range d.msgs {
		if d.msgs[i].ID == id {
			return &d.msgs[i]
		}
	}
	return nil
}

func (d *fakeDB) record(sql string, args []any, inTx bool) {
	d.calls = append(d.calls, call{sql: sql, args: args, inTx: inTx})
}

// callsTo returns the recorded statements that ran the given SQL.
func (d *fakeDB) callsTo(sql string) []call {
	var out []call
	for _, c := range d.calls {
		if c.sql == sql {
			out = append(out, c)
		}
	}
	return out
}

func (d *fakeDB) begin(_ context.Context) (pgx.Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return &fakeTx{db: d}, nil
}

func (d *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return d.exec(sql, args, false)
}

func (d *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return d.queryRow(sql, args, false)
}

func (d *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	return d.query(sql, args, false)
}

func (d *fakeDB) exec(sql string, args []any, inTx bool) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record(sql, args, inTx)
	if d.failOnQuery == sql {
		return pgconn.CommandTag{}, errors.New("fake: forced failure")
	}
	switch sql {
	case lockSeqSQL:
		return pgconn.CommandTag{}, nil
	}
	return pgconn.CommandTag{}, fmt.Errorf("fake: unexpected exec %q", sql)
}

func (d *fakeDB) queryRow(sql string, args []any, inTx bool) pgx.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record(sql, args, inTx)
	if d.failOnQuery == sql {
		return fakeRow{err: errors.New("fake: forced failure")}
	}
	switch sql {
	case userExistsSQL:
		return fakeRow{vals: []any{d.users[args[0].(string)]}}
	case areFriendsSQL:
		return fakeRow{vals: []any{d.friends[pair(args[0].(string), args[1].(string))]}}
	case markReadSQL:
		// The position is the newest live message the peer sent the reader, by
		// (created_at, id), read from the table; no clock and no argument but the two
		// ids. No such message is no row, and so is a position that does not sort
		// strictly after the stored one.
		reader, peer := args[0].(string), args[1].(string)
		var newest *messageRecord
		for i := range d.msgs {
			m := &d.msgs[i]
			if m.SenderID != peer || m.RecipientID != reader || m.DeletedAt != nil {
				continue
			}
			if newest == nil || olderThan(newest.CreatedAt, newest.ID, cursor{CreatedAt: m.CreatedAt, ID: m.ID}) {
				newest = m
			}
		}
		if newest == nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		key := [2]string{reader, peer}
		if cur, ok := d.reads[key]; ok && !sortsAfter(*newest, cur) {
			return fakeRow{err: pgx.ErrNoRows}
		}
		d.reads[key] = readMark{at: newest.CreatedAt, id: newest.ID}
		return fakeRow{vals: []any{newest.CreatedAt, newest.ID}}
	case historyCursorSQL:
		a, b, id := args[0].(string), args[1].(string), args[2].(string)
		if m := d.find(id); m != nil && inPair(*m, a, b) {
			return fakeRow{vals: []any{m.CreatedAt}}
		}
		return fakeRow{err: pgx.ErrNoRows}
	case inboxCursorSQL:
		user, id := args[0].(string), args[1].(string)
		if m := d.find(id); m != nil && (m.SenderID == user || m.RecipientID == user) {
			return fakeRow{vals: []any{m.CreatedAt}}
		}
		return fakeRow{err: pgx.ErrNoRows}
	case insertMessageSQL:
		// The unique index messages_sender_recipient_nonce_idx: a second row for the
		// same (sender, recipient, nonce) is refused the way Postgres refuses it.
		// An empty nonce is NULL, which never collides.
		sender, recipient, nonce := args[1].(string), args[2].(string), args[4].(string)
		if nonce != "" {
			for _, m := range d.msgs {
				if m.SenderID == sender && m.RecipientID == recipient && m.Nonce == nonce {
					return fakeRow{err: &pgconn.PgError{Code: "23505", ConstraintName: "messages_sender_recipient_nonce_idx"}}
				}
			}
		}
		m := d.seed(args[0].(string), sender, recipient, args[3].(string))
		d.find(m.ID).Nonce = nonce
		return messageRow(m)
	case findByNonceSQL:
		sender, recipient, nonce := args[0].(string), args[1].(string), args[2].(string)
		for _, m := range d.msgs {
			if m.SenderID == sender && m.RecipientID == recipient && m.Nonce != "" && m.Nonce == nonce {
				return messageRow(m)
			}
		}
		return fakeRow{err: pgx.ErrNoRows}
	case ownMessageRecipientSQL:
		m := d.find(args[0].(string))
		if m == nil || m.SenderID != args[1].(string) || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		return fakeRow{vals: []any{m.RecipientID}}
	case editMessageSQL:
		m := d.find(args[0].(string))
		if m == nil || m.SenderID != args[1].(string) || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		d.seq++
		at := d.clock.Add(time.Duration(d.seq) * time.Minute)
		m.Body, m.EditedAt, m.Seq = args[2].(string), &at, d.seq
		return messageRow(*m)
	case deleteMessageSQL:
		m := d.find(args[0].(string))
		if m == nil || m.SenderID != args[1].(string) || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		d.seq++
		at := d.clock.Add(time.Duration(d.seq) * time.Minute)
		m.Body, m.DeletedAt, m.Seq = "", &at, d.seq
		return messageRow(*m)
	}
	return fakeRow{err: fmt.Errorf("fake: unexpected query row %q", sql)}
}

func (d *fakeDB) query(sql string, args []any, inTx bool) (pgx.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record(sql, args, inTx)
	if d.failOnQuery == sql {
		return nil, errors.New("fake: forced failure")
	}
	switch sql {
	case listUpdatesSQL:
		user, since, limit := args[0].(string), args[1].(int64), args[2].(int)
		var hit []messageRecord
		for _, m := range d.msgs {
			peer := m.SenderID
			if peer == user {
				peer = m.RecipientID
			}
			// acceptedCTE: only people the caller is still friends with.
			if (m.SenderID == user || m.RecipientID == user) && m.Seq > since && d.friends[pair(user, peer)] {
				hit = append(hit, m)
			}
		}
		sort.Slice(hit, func(i, j int) bool { return hit[i].Seq < hit[j].Seq })
		return messageRows(clip(hit, limit)), nil

	case listConversationSQL, listConversationBeforeSQL:
		a, b := args[0].(string), args[1].(string)
		var (
			before *cursor
			limit  int
		)
		if sql == listConversationBeforeSQL {
			before = &cursor{CreatedAt: args[2].(time.Time), ID: args[3].(string)}
			limit = args[4].(int)
		} else {
			limit = args[2].(int)
		}
		var hit []messageRecord
		for _, m := range d.msgs {
			if inPair(m, a, b) && m.DeletedAt == nil && (before == nil || olderThan(m.CreatedAt, m.ID, *before)) {
				hit = append(hit, m)
			}
		}
		newestFirst(hit)
		return messageRows(clip(hit, limit)), nil

	case listSummariesSQL:
		user, limit := args[0].(string), args[3].(int)
		var before *cursor
		if args[1] != nil {
			before = &cursor{CreatedAt: args[1].(time.Time), ID: args[2].(string)}
		}
		newest := map[string]messageRecord{}
		for _, m := range d.msgs {
			if m.DeletedAt != nil || (m.SenderID != user && m.RecipientID != user) {
				continue
			}
			peer := m.SenderID
			if peer == user {
				peer = m.RecipientID
			}
			if !d.friends[pair(user, peer)] {
				continue
			}
			if cur, ok := newest[peer]; !ok || olderThan(cur.CreatedAt, cur.ID, cursor{CreatedAt: m.CreatedAt, ID: m.ID}) {
				newest[peer] = m
			}
		}
		var lasts []messageRecord
		for _, m := range newest {
			if before == nil || olderThan(m.CreatedAt, m.ID, *before) {
				lasts = append(lasts, m)
			}
		}
		newestFirst(lasts)
		var data [][]any
		for _, m := range clip(lasts, limit) {
			peer := m.SenderID
			if peer == user {
				peer = m.RecipientID
			}
			var unread int64
			for _, u := range d.msgs {
				if u.SenderID == peer && d.unreadFor(u, user) {
					unread++
				}
			}
			var mineAt, theirsAt *time.Time
			var mineID, theirsID *string
			if mark, ok := d.reads[[2]string{user, peer}]; ok {
				at := mark.at
				mineAt, mineID = &at, markID(mark)
			}
			if mark, ok := d.reads[[2]string{peer, user}]; ok {
				at := mark.at
				theirsAt, theirsID = &at, markID(mark)
			}
			data = append(data, append(append([]any{peer}, messageValues(m)...), unread, mineAt, mineID, theirsAt, theirsID))
		}
		return &fakeRows{data: data}, nil

	case unreadCountsSQL:
		user := args[0].(string)
		counts := map[string]int64{}
		for _, m := range d.msgs {
			if d.unreadFor(m, user) {
				counts[m.SenderID]++
			}
		}
		var peers []string
		for p := range counts {
			peers = append(peers, p)
		}
		sort.Strings(peers)
		var data [][]any
		for _, p := range peers {
			data = append(data, []any{p, counts[p]})
		}
		return &fakeRows{data: data}, nil
	}
	return nil, fmt.Errorf("fake: unexpected query %q", sql)
}

// markID is the last_read_id column of a fake marker: NULL when it has no id.
func markID(mark readMark) *string {
	if mark.id == "" {
		return nil
	}
	id := mark.id
	return &id
}

func inPair(m messageRecord, a, b string) bool {
	return (m.SenderID == a && m.RecipientID == b) || (m.SenderID == b && m.RecipientID == a)
}

// olderThan is the keyset comparison (created_at, id) < (c.CreatedAt, c.ID).
func olderThan(createdAt time.Time, id string, c cursor) bool {
	if !createdAt.Equal(c.CreatedAt) {
		return createdAt.Before(c.CreatedAt)
	}
	return id < c.ID
}

// newestFirst orders by (created_at, id) descending, which is how history and the
// conversation list are ordered.
func newestFirst(ms []messageRecord) {
	sort.Slice(ms, func(i, j int) bool {
		if !ms[i].CreatedAt.Equal(ms[j].CreatedAt) {
			return ms[i].CreatedAt.After(ms[j].CreatedAt)
		}
		return ms[i].ID > ms[j].ID
	})
}

func clip(ms []messageRecord, limit int) []messageRecord {
	if len(ms) > limit {
		return ms[:limit]
	}
	return ms
}

// fakeTx is a transaction over the same fake: its statements are recorded as
// inside a transaction, and Commit/Rollback are counted.
type fakeTx struct {
	pgx.Tx // anything the code under test must not call panics on the nil
	db     *fakeDB
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.exec(sql, args, true)
}

func (t *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return t.db.queryRow(sql, args, true)
}

func (t *fakeTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.query(sql, args, true)
}

func (t *fakeTx) Commit(_ context.Context) error {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.commits++
	return nil
}

func (t *fakeTx) Rollback(_ context.Context) error {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.rollbacks++
	return nil
}

// fakeRow is one row of values, assigned to Scan's destinations by type.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assign(dest, r.vals)
}

func messageValues(m messageRecord) []any {
	return []any{m.ID, m.Seq, m.SenderID, m.RecipientID, m.Body, m.CreatedAt, m.EditedAt, m.DeletedAt}
}

func messageRow(m messageRecord) fakeRow { return fakeRow{vals: messageValues(m)} }

func messageRows(ms []messageRecord) *fakeRows {
	rows := &fakeRows{}
	for _, m := range ms {
		rows.data = append(rows.data, messageValues(m))
	}
	return rows
}

type fakeRows struct {
	pgx.Rows
	data [][]any
	i    int
}

func (r *fakeRows) Next() bool {
	r.i++
	return r.i <= len(r.data)
}
func (r *fakeRows) Scan(dest ...any) error { return assign(dest, r.data[r.i-1]) }
func (r *fakeRows) Err() error             { return nil }
func (r *fakeRows) Close()                 {}

// assign copies vals into the pointers Scan was given. It is strict about types
// on purpose: a column added to messageColumns without a matching field fails here
// rather than scanning into the wrong place.
func assign(dest, vals []any) error {
	if len(dest) != len(vals) {
		return fmt.Errorf("fake: scanning %d columns into %d destinations", len(vals), len(dest))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d).Elem()
		vv := reflect.ValueOf(vals[i])
		if !vv.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("fake: column %d is %s, destination is %s", i, vv.Type(), dv.Type())
		}
		dv.Set(vv)
	}
	return nil
}

// ---- the SQL, pinned as text ------------------------------------------------

func mustContain(t *testing.T, name, sql string, want ...string) {
	t.Helper()
	norm := strings.Join(strings.Fields(sql), " ")
	for _, w := range want {
		if !strings.Contains(norm, w) {
			t.Errorf("%s is missing %q\n%s", name, w, norm)
		}
	}
}

func mustNotContain(t *testing.T, name, sql string, bad ...string) {
	t.Helper()
	norm := strings.Join(strings.Fields(sql), " ")
	for _, b := range bad {
		if strings.Contains(norm, b) {
			t.Errorf("%s must not contain %q\n%s", name, b, norm)
		}
	}
}

// A conversation is ordered by (created_at, id) and never by seq: an edit moves
// seq, and a message that jumps to the bottom when corrected is a bug every chat
// app has had once. History also leaves tombstones out.
func TestSQLHistoryOrdersByCreationAndOmitsTombstones(t *testing.T) {
	mustContain(t, "listConversationSQL", listConversationSQL,
		"AND deleted_at IS NULL",
		"ORDER BY created_at DESC, id DESC",
	)
	mustNotContain(t, "listConversationSQL", listConversationSQL, "ORDER BY seq", "seq >")
}

// The catch-up feed is the reverse: tombstones are the point of it, and it is
// ordered by seq. It reads both directions as two indexed branches, each filtered
// to accepted friends BEFORE its own LIMIT so the page stays a true prefix.
func TestSQLCatchUpKeepsTombstonesAndOrdersBySeq(t *testing.T) {
	mustNotContain(t, "listUpdatesSQL", listUpdatesSQL, "deleted_at IS NULL", "ORDER BY created_at")
	mustContain(t, "listUpdatesSQL", listUpdatesSQL,
		"WHERE sender_id = $1 AND seq > $2 AND recipient_id IN (SELECT peer_id FROM accepted) ORDER BY seq LIMIT $3)",
		"WHERE recipient_id = $1 AND seq > $2 AND sender_id IN (SELECT peer_id FROM accepted) ORDER BY seq LIMIT $3)",
		"UNION ALL",
		") updates ORDER BY seq LIMIT $3",
	)
	// The two directions are two branches, never one OR over the pair of columns.
	mustNotContain(t, "listUpdatesSQL", listUpdatesSQL,
		"sender_id = $1 OR", "recipient_id = $1 OR", "OR sender_id", "OR recipient_id")
}

// "My friends" is defined once. The feed and the conversation list read through
// the very same CTE text, so they cannot disagree about who is still a friend, and
// that text is the accepted-friendship rule areFriendsSQL asks of one pair.
func TestSQLCatchUpAndConversationListShareOneDefinitionOfFriends(t *testing.T) {
	def := strings.Join(strings.Fields(acceptedCTE), " ")
	mustContain(t, "acceptedCTE", acceptedCTE,
		"WHERE state = 'accepted' AND (user_a_id = $1 OR user_b_id = $1)")
	mustContain(t, "listUpdatesSQL", listUpdatesSQL, "WITH "+def)
	mustContain(t, "listSummariesSQL", listSummariesSQL, "WITH "+def)
	for name, sql := range map[string]string{"listUpdatesSQL": listUpdatesSQL, "listSummariesSQL": listSummariesSQL} {
		if n := strings.Count(strings.Join(strings.Fields(sql), " "), "state = 'accepted'"); n != 1 {
			t.Errorf("%s states the friendship rule %d times, want once", name, n)
		}
	}
}

// created_at is taken with clock_timestamp() while the seq lock is held, not with
// the column default now(), which is the instant the transaction STARTED. A
// message must be stamped no earlier than the moment it is allowed to commit, or
// a read position copied from the newest visible message can sit after a message
// that is still on its way. So the stamp is explicit, and the lock comes first.
// Edits and deletes leave their own columns alone.
func TestSQLInsertStampsCreatedAtUnderTheLockNotAtTransactionStart(t *testing.T) {
	mustContain(t, "insertMessageSQL", insertMessageSQL, "created_at, nonce) VALUES ($1, $2, $3, $4, clock_timestamp(),")
	mustNotContain(t, "insertMessageSQL", insertMessageSQL, "now()", "DEFAULT")
	mustContain(t, "editMessageSQL", editMessageSQL, "edited_at = now()")
	mustContain(t, "deleteMessageSQL", deleteMessageSQL, "deleted_at = now()")
	mustNotContain(t, "editMessageSQL", editMessageSQL, "created_at =")
	mustNotContain(t, "deleteMessageSQL", deleteMessageSQL, "created_at =")

	// The statement that carries the stamp runs after the lock, in the same
	// transaction, through the path a send takes.
	db := newFakeDB("a", "b")
	if _, err := writeSeq(context.Background(), db.begin, func(tx dbExecutor) (*messageRecord, error) {
		m, _, err := insertOnce(context.Background(), tx, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi"})
		return m, err
	}); err != nil {
		t.Fatal(err)
	}
	var stmts []string
	for _, c := range db.calls {
		if c.sql == lockSeqSQL || c.sql == insertMessageSQL {
			if !c.inTx {
				t.Errorf("%q ran outside the transaction", c.sql)
			}
			stmts = append(stmts, c.sql)
		}
	}
	if !reflect.DeepEqual(stmts, []string{lockSeqSQL, insertMessageSQL}) {
		t.Errorf("statements = %v, want the lock, then the insert that stamps created_at", stmts)
	}
}

// A deleted message must not keep a badge alive.
func TestSQLUnreadIgnoresDeletedMessages(t *testing.T) {
	mustContain(t, "unreadCountsSQL", unreadCountsSQL, "m.deleted_at IS NULL")
}

// Editing and deleting match on the sender and on a live row in the WHERE clause
// — that is what makes "not yours", "not there" and "already deleted" one answer
// — and both draw a fresh seq. A delete clears the words.
func TestSQLEditAndDeleteAreSenderOnlyAndBumpSeq(t *testing.T) {
	for name, sql := range map[string]string{"edit": editMessageSQL, "delete": deleteMessageSQL} {
		mustContain(t, name, sql,
			"WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL",
			"seq = nextval('messages_seq')",
			"RETURNING "+messageColumns,
		)
		mustNotContain(t, name, sql, "created_at =")
	}
	mustContain(t, "edit", editMessageSQL, "SET body = $3, edited_at = now()")
	mustContain(t, "delete", deleteMessageSQL, "SET body = '', deleted_at = now()")
}

// An absent nonce is stored as NULL, so the partial unique index never sees an
// empty string, and the repeat lookup states the equality that index serves.
func TestSQLNonceIsNullWhenAbsentAndLookedUpByConversationAndNonce(t *testing.T) {
	mustContain(t, "insertMessageSQL", insertMessageSQL,
		"INSERT INTO messages (id, sender_id, recipient_id, body, created_at, nonce) VALUES ($1, $2, $3, $4, clock_timestamp(), NULLIF($5::text, ''))",
		"RETURNING "+messageColumns,
	)
	mustContain(t, "findByNonceSQL", findByNonceSQL,
		"SELECT "+messageColumns+" FROM messages WHERE sender_id = $1 AND recipient_id = $2 AND nonce = $3",
	)
	// The repeat path must never lean on ON CONFLICT: the losing insert would still
	// draw a seq from the column default.
	mustNotContain(t, "insertMessageSQL", insertMessageSQL, "ON CONFLICT")
}

// The migration is what the SQL above relies on: a nullable column and a PARTIAL
// unique index on (sender_id, recipient_id, nonce), read from the file the server embeds.
func TestMigrationAddsANullableNonceAndAPartialUniqueIndex(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/20261019130000_messages_nonce.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "migration", string(raw),
		"ALTER TABLE messages ADD COLUMN nonce TEXT;",
		"CREATE UNIQUE INDEX messages_sender_recipient_nonce_idx ON messages (sender_id, recipient_id, nonce) WHERE nonce IS NOT NULL;",
		"DROP INDEX messages_sender_recipient_nonce_idx;",
		"ALTER TABLE messages DROP COLUMN nonce;",
	)
	mustNotContain(t, "migration", string(raw), "nonce TEXT NOT NULL")
}

// ---- the store functions ----------------------------------------------------

func TestEditAndDeleteReportNoMessageForZeroRows(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "hi")

	if _, err := editMessage(context.Background(), db, "missing", "a", "x"); !errors.Is(err, errNoMessage) {
		t.Errorf("edit of a missing message = %v, want errNoMessage", err)
	}
	if _, err := editMessage(context.Background(), db, "m1", "b", "x"); !errors.Is(err, errNoMessage) {
		t.Errorf("edit by a non-sender = %v, want errNoMessage", err)
	}
	if _, err := deleteMessage(context.Background(), db, "m1", "b"); !errors.Is(err, errNoMessage) {
		t.Errorf("delete by a non-sender = %v, want errNoMessage", err)
	}
	if got := db.find("m1"); got.Body != "hi" || got.DeletedAt != nil || got.EditedAt != nil {
		t.Errorf("a refused write changed the message: %+v", got)
	}
}

// Any other database error is NOT "not found": reporting it as one would tell a
// sender their message does not exist when the database is down.
func TestEditPassesOtherErrorsThrough(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "hi")
	db.failOnQuery = editMessageSQL

	_, err := editMessage(context.Background(), db, "m1", "a", "x")
	if err == nil || errors.Is(err, errNoMessage) {
		t.Errorf("a database failure = %v, want a real error", err)
	}
}

func TestEditKeepsPositionAndBumpsSeq(t *testing.T) {
	db := newFakeDB("a", "b")
	first := db.seed("m1", "a", "b", "hi")
	db.seed("m2", "b", "a", "yo")

	edited, err := editMessage(context.Background(), db, "m1", "a", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Seq <= first.Seq || edited.Seq <= 2 {
		t.Errorf("seq = %d, want it past every earlier draw (first was %d)", edited.Seq, first.Seq)
	}
	if !edited.CreatedAt.Equal(first.CreatedAt) || edited.ID != first.ID {
		t.Error("an edit moved the message's created_at or id")
	}
	if edited.EditedAt == nil || edited.DeletedAt != nil || edited.Body != "hello" {
		t.Errorf("edited = %+v", edited)
	}
}

func TestDeleteClearsTheBody(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "secret words")

	gone, err := deleteMessage(context.Background(), db, "m1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if gone.Body != "" || gone.DeletedAt == nil {
		t.Errorf("returned message = %+v, want an empty body and deleted_at set", gone)
	}
	if got := db.find("m1"); got.Body != "" || got.DeletedAt == nil {
		t.Errorf("stored message = %+v, want the words gone from the row itself", got)
	}
}

// writeSeq's whole job: the lock goes first, the write is inside the same
// transaction, and the commit is last. Draw order has to equal commit order.
func TestWriteSeqLocksBeforeWritingAndCommits(t *testing.T) {
	db := newFakeDB("a", "b")

	_, err := writeSeq(context.Background(), db.begin, func(tx dbExecutor) (*messageRecord, error) {
		return insertMessage(context.Background(), tx, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi"})
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(db.calls) != 2 || db.calls[0].sql != lockSeqSQL || db.calls[1].sql != insertMessageSQL {
		t.Fatalf("statements = %v, want the lock, then the insert", db.calls)
	}
	if got := db.calls[0].args[0]; got != seqLockKey {
		t.Errorf("lock key = %v, want %v", got, seqLockKey)
	}
	for _, c := range db.calls {
		if !c.inTx {
			t.Errorf("%q ran outside the transaction", c.sql)
		}
	}
	if db.commits != 1 {
		t.Errorf("commits = %d, want 1", db.commits)
	}
}

func TestWriteSeqDoesNotCommitAFailedWrite(t *testing.T) {
	db := newFakeDB("a", "b")
	boom := errors.New("write failed")

	_, err := writeSeq(context.Background(), db.begin, func(dbExecutor) (*messageRecord, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the write's own error", err)
	}
	if db.commits != 0 || db.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d, want 0 and 1", db.commits, db.rollbacks)
	}
}

func TestWriteSeqStopsIfItCannotTakeTheLock(t *testing.T) {
	db := newFakeDB("a", "b")
	db.failOnQuery = lockSeqSQL
	ran := false

	_, err := writeSeq(context.Background(), db.begin, func(dbExecutor) (*messageRecord, error) {
		ran = true
		return &messageRecord{}, nil
	})
	if err == nil || ran || db.commits != 0 {
		t.Errorf("err=%v ran=%v commits=%d: a write ran without the lock", err, ran, db.commits)
	}
}

func TestWriteSeqReportsABeginFailure(t *testing.T) {
	db := newFakeDB("a", "b")
	db.beginErr = errors.New("pool exhausted")

	if _, err := writeSeq(context.Background(), db.begin, func(dbExecutor) (*messageRecord, error) {
		t.Fatal("write ran without a transaction")
		return nil, nil
	}); err == nil {
		t.Error("expected the begin failure to surface")
	}
}

// listUpdates returns what the SQL says: both directions, tombstones included,
// by seq — and a tombstone is a row the feed carries but history does not.
func TestCatchUpIncludesTombstonesAndHistoryOmitsThem(t *testing.T) {
	db := newFakeDB("a", "b")
	db.befriend("a", "b")
	db.seed("m1", "a", "b", "one")
	db.seed("m2", "b", "a", "two")
	if _, err := deleteMessage(context.Background(), db, "m1", "a"); err != nil {
		t.Fatal(err)
	}

	feed, err := listUpdates(context.Background(), db, "a", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(feed); !reflect.DeepEqual(ids, []string{"m2", "m1"}) {
		t.Errorf("catch-up = %v, want [m2 m1]: both, by seq, the tombstone last", ids)
	}
	if feed[1].DeletedAt == nil || feed[1].Body != "" {
		t.Errorf("the catch-up should carry the tombstone: %+v", feed[1])
	}

	hist, err := listConversation(context.Background(), db, "a", "b", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(hist); !reflect.DeepEqual(ids, []string{"m2"}) {
		t.Errorf("history = %v, want only the live message", ids)
	}
}

func TestUnreadIgnoresDeletedMessages(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "one")
	db.seed("m2", "a", "b", "two")

	got, err := unreadCounts(context.Background(), db, "b")
	if err != nil || len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("before the delete: %v, %v", got, err)
	}
	if _, err := deleteMessage(context.Background(), db, "m1", "a"); err != nil {
		t.Fatal(err)
	}
	got, err = unreadCounts(context.Background(), db, "b")
	if err != nil || len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("after deleting one: %v, %v", got, err)
	}
	if _, err := deleteMessage(context.Background(), db, "m2", "a"); err != nil {
		t.Fatal(err)
	}
	got, err = unreadCounts(context.Background(), db, "b")
	if err != nil || len(got) != 0 {
		t.Fatalf("after deleting both: %v, %v", got, err)
	}
}

// insertOnce: a repeat of (sender, recipient, nonce) is the original message, and nothing is
// inserted or drawn for it.
func TestInsertOnceReturnsTheOriginalForARepeatAndDrawsNoSeq(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()

	first, created, err := insertOnce(ctx, db, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
	if err != nil || !created {
		t.Fatalf("first = %+v, created %v, err %v", first, created, err)
	}
	seqAfterFirst := db.seq

	again, created, err := insertOnce(ctx, db, &messageRecord{ID: "m2", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
	if err != nil || created {
		t.Fatalf("repeat = %+v, created %v, err %v, want the original and created=false", again, created, err)
	}
	if again.ID != "m1" || again.Seq != first.Seq || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("repeat = %+v, want the original %+v", again, first)
	}
	if len(db.msgs) != 1 || db.seq != seqAfterFirst {
		t.Errorf("rows = %d, seq %d -> %d: a repeat stored or drew something", len(db.msgs), seqAfterFirst, db.seq)
	}
	if n := len(db.callsTo(insertMessageSQL)); n != 1 {
		t.Errorf("insert statements = %d, want 1: the repeat must not even try", n)
	}
}

// A repeat answers with the message as it is now, edits included.
func TestInsertOnceReturnsTheCurrentStateOfTheOriginal(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()
	if _, _, err := insertOnce(ctx, db, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := editMessage(ctx, db, "m1", "a", "hello"); err != nil {
		t.Fatal(err)
	}

	again, created, err := insertOnce(ctx, db, &messageRecord{ID: "m2", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
	if err != nil || created || again.Body != "hello" || again.EditedAt == nil {
		t.Errorf("repeat = %+v, created %v, err %v, want the edited original", again, created, err)
	}
}

func TestInsertOnceWithoutANonceAlwaysInserts(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()

	for _, id := range []string{"m1", "m2"} {
		_, created, err := insertOnce(ctx, db, &messageRecord{ID: id, SenderID: "a", RecipientID: "b", Body: "same"})
		if err != nil || !created {
			t.Fatalf("%s: created %v, err %v", id, created, err)
		}
	}
	if len(db.msgs) != 2 {
		t.Errorf("rows = %d, want 2: no nonce means no idempotency", len(db.msgs))
	}
	if n := len(db.callsTo(findByNonceSQL)); n != 0 {
		t.Errorf("lookups = %d, want 0 without a nonce", n)
	}
}

// The key includes the sender: the same nonce from someone else is its own message.
func TestInsertOnceKeysOnTheSenderToo(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()

	if _, created, err := insertOnce(ctx, db, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "from a", Nonce: "shared"}); err != nil || !created {
		t.Fatalf("a: created %v, err %v", created, err)
	}
	got, created, err := insertOnce(ctx, db, &messageRecord{ID: "m2", SenderID: "b", RecipientID: "a", Body: "from b", Nonce: "shared"})
	if err != nil || !created || got.ID != "m2" || got.Body != "from b" {
		t.Errorf("b = %+v, created %v, err %v, want a separate new message", got, created, err)
	}
	if len(db.msgs) != 2 {
		t.Errorf("rows = %d, want 2", len(db.msgs))
	}
}

// The key includes the recipient: the same sender's same nonce to somebody else is
// a new message, and the fake refuses a duplicate on all three columns, so a path
// that dropped the recipient from the lookup would hit the 23505 here.
func TestInsertOnceKeysOnTheRecipientToo(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	ctx := context.Background()

	if _, created, err := insertOnce(ctx, db, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "to b", Nonce: "n1"}); err != nil || !created {
		t.Fatalf("to b: created %v, err %v", created, err)
	}
	got, created, err := insertOnce(ctx, db, &messageRecord{ID: "m2", SenderID: "a", RecipientID: "c", Body: "to c", Nonce: "n1"})
	if err != nil || !created || got.ID != "m2" || got.RecipientID != "c" {
		t.Errorf("to c = %+v, created %v, err %v, want a separate new message", got, created, err)
	}
	again, created, err := insertOnce(ctx, db, &messageRecord{ID: "m3", SenderID: "a", RecipientID: "c", Body: "to c", Nonce: "n1"})
	if err != nil || created || again.ID != "m2" {
		t.Errorf("retry to c = %+v, created %v, err %v, want m2", again, created, err)
	}
	if len(db.msgs) != 2 {
		t.Errorf("rows = %d, want 2", len(db.msgs))
	}
}

// A failed lookup is a failure, not "no such message": treating it as the latter
// would insert, and a retry whose first copy exists would be stored twice.
func TestInsertOnceStopsWhenTheLookupFails(t *testing.T) {
	db := newFakeDB("a", "b")
	db.failOnQuery = findByNonceSQL

	_, _, err := insertOnce(context.Background(), db, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
	if err == nil {
		t.Fatal("expected the lookup failure to surface")
	}
	if len(db.msgs) != 0 {
		t.Error("a message was inserted after the lookup failed")
	}
}

func idsOf(ms []messageRecord) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// ---- pagination and the conversation list: the SQL as text -----------------

// Both pages of a conversation are keyset on (created_at, id) and never seq or
// OFFSET: an edit moves seq, and an OFFSET shifts under a conversation that is
// still receiving messages.
func TestSQLHistoryPagesByKeysetOnCreationAndID(t *testing.T) {
	mustContain(t, "listConversationBeforeSQL", listConversationBeforeSQL,
		"AND deleted_at IS NULL",
		"AND (created_at, id) < ($3, $4)",
		"ORDER BY created_at DESC, id DESC",
		"LIMIT $5",
	)
	mustNotContain(t, "listConversationBeforeSQL", listConversationBeforeSQL, "ORDER BY seq", "seq <", "OFFSET")
	mustNotContain(t, "listConversationSQL", listConversationSQL, "OFFSET")
	// One definition of "this pair", shared by every query over a conversation.
	for name, sql := range map[string]string{
		"listConversationSQL":       listConversationSQL,
		"listConversationBeforeSQL": listConversationBeforeSQL,
		"historyCursorSQL":          historyCursorSQL,
	} {
		mustContain(t, name, sql, strings.Join(strings.Fields(conversationPairSQL), " "))
	}
}

// A cursor may be a message that has since been deleted (the oldest one a client
// holds), so the lookups must not filter tombstones. History's is scoped to the
// pair and the list's to the caller, so no other message id answers.
func TestSQLCursorLookups(t *testing.T) {
	mustNotContain(t, "historyCursorSQL", historyCursorSQL, "deleted_at")
	mustContain(t, "historyCursorSQL", historyCursorSQL, "WHERE id = $3 AND ")
	mustNotContain(t, "inboxCursorSQL", inboxCursorSQL, "deleted_at")
	mustContain(t, "inboxCursorSQL", inboxCursorSQL, "WHERE id = $2 AND (sender_id = $1 OR recipient_id = $1)")
}

// The conversation list: one statement, one row per peer by DISTINCT ON in
// (created_at, id) order, live messages only, accepted friends only, keyset
// paged, and never seq.
func TestSQLConversationListIsOneKeysetStatement(t *testing.T) {
	mustContain(t, "listSummariesSQL", listSummariesSQL,
		"SELECT DISTINCT ON (peer_id) peer_id,",
		"ORDER BY peer_id, created_at DESC, id DESC",
		"FROM messages WHERE sender_id = $1 AND deleted_at IS NULL UNION ALL SELECT sender_id AS peer_id",
		"FROM messages WHERE recipient_id = $1 AND deleted_at IS NULL",
		"WHERE state = 'accepted' AND (user_a_id = $1 OR user_b_id = $1)",
		"FROM exchanged JOIN accepted USING (peer_id)",
		"WHERE ($2::timestamptz IS NULL OR (newest.created_at, newest.id) < ($2, $3))",
		"ORDER BY newest.created_at DESC, newest.id DESC",
		"LIMIT $4",
		"LEFT JOIN message_reads mine ON mine.user_id = $1 AND mine.peer_id = newest.peer_id",
		"LEFT JOIN message_reads theirs ON theirs.user_id = newest.peer_id AND theirs.peer_id = $1",
	)
	mustNotContain(t, "listSummariesSQL", listSummariesSQL, "ORDER BY seq", "seq <", "seq >", "OFFSET")
}

// "Unread" is defined once. The conversation list counts with the very text
// GET /messages/unread counts with, so the two can never disagree about it.
func TestSQLUnreadHasOneDefinition(t *testing.T) {
	def := strings.Join(strings.Fields(unreadMessagesSQL), " ")
	const rule = "(m.created_at, m.id) > (r.last_read_at, COALESCE(r.last_read_id, ''))"
	mustContain(t, "unreadMessagesSQL", unreadMessagesSQL,
		"m.deleted_at IS NULL",
		// No marker at all is everything unread; a marker is a row comparison.
		"AND (r.last_read_at IS NULL OR "+rule+")",
	)
	mustNotContain(t, "unreadMessagesSQL", unreadMessagesSQL, "m.created_at > r.last_read_at")
	mustContain(t, "unreadCountsSQL", unreadCountsSQL, def)
	mustContain(t, "listSummariesSQL", listSummariesSQL, def)
	for name, sql := range map[string]string{"unreadCountsSQL": unreadCountsSQL, "listSummariesSQL": listSummariesSQL} {
		if n := strings.Count(strings.Join(strings.Fields(sql), " "), rule); n != 1 {
			t.Errorf("%s states the unread rule %d times, want once", name, n)
		}
	}
}

// The marker only ever moves forward, and the statement says whether it did: the
// upsert's WHERE compares whole positions, with the same COALESCE the unread rule
// uses so a marker with no id is replaced by any message at or after its time,
// and a refused row returns nothing.
func TestSQLMarkReadIsMonotonicAndReportsAdvance(t *testing.T) {
	mustContain(t, "markReadSQL", markReadSQL,
		"INSERT INTO message_reads (user_id, peer_id, last_read_at, last_read_id)",
		"ON CONFLICT (user_id, peer_id)",
		"DO UPDATE SET last_read_at = EXCLUDED.last_read_at, last_read_id = EXCLUDED.last_read_id",
		"WHERE (message_reads.last_read_at, COALESCE(message_reads.last_read_id, '')) < (EXCLUDED.last_read_at, EXCLUDED.last_read_id)",
		"RETURNING last_read_at, last_read_id",
	)
	mustNotContain(t, "markReadSQL", markReadSQL, "GREATEST", "<=")
}

// The marker is a position read out of messages and never a clock reading: the
// newest live message the peer sent the reader, by the same (created_at, id) the
// unread rule compares with, written in the same statement it was read in. The
// statement takes the two ids and nothing else.
func TestSQLMarkReadTakesItsPositionFromTheMessagesTable(t *testing.T) {
	mustContain(t, "markReadSQL", markReadSQL,
		"SELECT $1::text, $2::text, m.created_at, m.id FROM messages m",
		"WHERE m.sender_id = $2 AND m.recipient_id = $1 AND m.deleted_at IS NULL",
		"ORDER BY m.created_at DESC, m.id DESC LIMIT 1",
	)
	mustNotContain(t, "markReadSQL", markReadSQL, "now()", "clock_timestamp", "current_timestamp", "$3")
}

// The migration the SQL above relies on: one nullable column, no backfill, and the
// old column left alone. A NULL id is what the COALESCE in the unread rule and in
// markReadSQL is for, so the column must not be NOT NULL and no row may be given
// an id.
func TestMigrationAddsANullableLastReadIDAndRewritesNothing(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/20261019140000_message_reads_position.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "migration", string(raw),
		"ALTER TABLE message_reads ADD COLUMN last_read_id TEXT;",
		"ALTER TABLE message_reads DROP COLUMN last_read_id;",
		"There is NO backfill",
	)
	mustNotContain(t, "migration", string(raw),
		"last_read_id TEXT NOT NULL",
		"DEFAULT",
		"UPDATE message_reads",
		"DROP COLUMN last_read_at",
		"DELETE",
	)
}

// ---- pagination and the conversation list: the store functions -------------

func TestListConversationPagesStrictlyOlderAndStopsAtTheStart(t *testing.T) {
	db := newFakeDB("a", "b")
	for i := 1; i <= 5; i++ {
		db.seed(fmt.Sprintf("m%d", i), "a", "b", "x")
	}
	ctx := context.Background()

	anchor, err := historyCursor(ctx, db, "a", "b", "m4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := listConversation(ctx, db, "a", "b", 2, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got); !reflect.DeepEqual(ids, []string{"m3", "m2"}) {
		t.Errorf("page before m4 = %v, want [m3 m2] (newest first, the anchor itself excluded)", ids)
	}

	anchor, _ = historyCursor(ctx, db, "a", "b", "m1")
	got, err = listConversation(ctx, db, "a", "b", 2, anchor)
	if err != nil || len(got) != 0 {
		t.Errorf("page before the first message = %v, %v, want nothing", idsOf(got), err)
	}
}

// Two messages in the same instant are told apart by id, so a page boundary that
// falls between them neither drops one nor shows one twice.
func TestListConversationBreaksTiesOnID(t *testing.T) {
	db := newFakeDB("a", "b")
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	db.seedAt("m1", "a", "b", "x", at)
	db.seedAt("m2", "b", "a", "x", at)
	db.seedAt("m3", "a", "b", "x", at)
	ctx := context.Background()

	var seen []string
	var before *cursor
	for range 5 {
		page, err := listConversation(ctx, db, "a", "b", 1, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].ID)
		before = &cursor{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
	}
	if !reflect.DeepEqual(seen, []string{"m3", "m2", "m1"}) {
		t.Errorf("one-at-a-time paging over a tie saw %v, want [m3 m2 m1]", seen)
	}
}

func TestCursorLookups(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.seed("ab", "a", "b", "x")
	db.seed("bc", "b", "c", "x")
	ctx := context.Background()

	if c, err := historyCursor(ctx, db, "a", "b", "ab"); err != nil || c.ID != "ab" || c.CreatedAt.IsZero() {
		t.Errorf("a message of the conversation: %+v, %v", c, err)
	}
	if _, err := historyCursor(ctx, db, "a", "b", "bc"); !errors.Is(err, errNoCursor) {
		t.Errorf("a message of another conversation = %v, want errNoCursor", err)
	}
	if _, err := historyCursor(ctx, db, "a", "b", "nope"); !errors.Is(err, errNoCursor) {
		t.Errorf("a missing message = %v, want errNoCursor", err)
	}

	if c, err := inboxCursor(ctx, db, "b", "ab"); err != nil || c.ID != "ab" {
		t.Errorf("a message the caller received: %+v, %v", c, err)
	}
	if c, err := inboxCursor(ctx, db, "b", "bc"); err != nil || c.ID != "bc" {
		t.Errorf("a message the caller sent: %+v, %v", c, err)
	}
	if _, err := inboxCursor(ctx, db, "a", "bc"); !errors.Is(err, errNoCursor) {
		t.Errorf("a message between two other people = %v, want errNoCursor", err)
	}

	// A database failure is not "no such cursor".
	db.failOnQuery = historyCursorSQL
	if _, err := historyCursor(ctx, db, "a", "b", "ab"); err == nil || errors.Is(err, errNoCursor) {
		t.Errorf("a failing lookup = %v, want a real error", err)
	}
}

func TestListSummariesPicksTheNewestLiveMessagePerFriend(t *testing.T) {
	db := newFakeDB("a", "b", "c", "d")
	db.befriend("a", "b")
	db.befriend("a", "c")
	db.befriend("a", "d")
	db.seed("ab1", "a", "b", "one")
	db.seed("ab2", "b", "a", "two")
	db.seed("ac1", "c", "a", "three")
	db.seed("ab3", "b", "a", "deleted newest")
	db.seed("ad1", "a", "d", "all deleted")
	if _, err := deleteMessage(context.Background(), db, "ab3", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteMessage(context.Background(), db, "ad1", "a"); err != nil {
		t.Fatal(err)
	}

	got, err := listSummaries(context.Background(), db, "a", 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PeerID != "c" || got[1].PeerID != "b" {
		t.Fatalf("conversations = %+v, want c then b (d has only a deleted message)", got)
	}
	if got[1].Last.ID != "ab2" {
		t.Errorf("last message with b = %q, want ab2: the newest LIVE one", got[1].Last.ID)
	}
	if got[0].Unread != 1 || got[1].Unread != 1 {
		t.Errorf("unread = %d and %d, want 1 and 1 (b's deleted message is not counted)", got[0].Unread, got[1].Unread)
	}
}

func TestListSummariesPagesStrictlyOlderThanTheCursor(t *testing.T) {
	db := newFakeDB("a", "b", "c", "d")
	for _, p := range []string{"b", "c", "d"} {
		db.befriend("a", p)
		db.seed("m-"+p, p, "a", "x")
	}
	ctx := context.Background()

	first, err := listSummaries(ctx, db, "a", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].PeerID != "d" || first[1].PeerID != "c" {
		t.Fatalf("first page = %+v", first)
	}
	next, err := listSummaries(ctx, db, "a", 2, &cursor{CreatedAt: first[1].Last.CreatedAt, ID: first[1].Last.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].PeerID != "b" {
		t.Errorf("second page = %+v, want only b", next)
	}
}

// markRead copies the position of the newest live message the peer sent the reader
// into the marker, and says whether the marker moved.
func TestMarkReadMovesToTheNewestLiveMessageFromThePeer(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	ctx := context.Background()
	db.seed("m1", "b", "a", "one")
	m2 := db.seed("m2", "b", "a", "two")
	db.seed("m3", "a", "b", "mine: not the peer's to have read")
	db.seed("m4", "c", "a", "another peer")

	got, advanced, err := markRead(ctx, db, "a", "b")
	if err != nil || !advanced {
		t.Fatalf("first read = %+v, %v, %v, want it to advance", got, advanced, err)
	}
	if want := (readPosition{At: m2.CreatedAt, ID: "m2"}); got != want {
		t.Errorf("position = %+v, want the newest message from b: %+v", got, want)
	}
	if stored := db.reads[[2]string{"a", "b"}]; stored != (readMark{at: m2.CreatedAt, id: "m2"}) {
		t.Errorf("stored = %+v, want exactly what was returned", stored)
	}
	if _, ok := db.reads[[2]string{"a", "c"}]; ok {
		t.Error("reading b's messages marked c's")
	}

	// Nothing new: the same position again is not an advance.
	if _, advanced, err := markRead(ctx, db, "a", "b"); err != nil || advanced {
		t.Errorf("a repeat read = %v, %v, want no advance", advanced, err)
	}

	// Something new: it moves to that message.
	m5 := db.seed("m5", "b", "a", "three")
	got, advanced, err = markRead(ctx, db, "a", "b")
	if err != nil || !advanced || got != (readPosition{At: m5.CreatedAt, ID: "m5"}) {
		t.Errorf("read after m5 = %+v, %v, %v, want it to advance to m5", got, advanced, err)
	}

	db.failOnQuery = markReadSQL
	if _, _, err := markRead(ctx, db, "a", "b"); err == nil {
		t.Error("a failing write must be an error, not a silent no-advance")
	}
}

// The marker never rewinds. The realistic way to try: the message the marker points
// at is deleted, so the newest LIVE message is older than the marker.
func TestMarkReadNeverRewinds(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()
	db.seed("m1", "b", "a", "one")
	m2 := db.seed("m2", "b", "a", "two")
	if _, advanced, err := markRead(ctx, db, "a", "b"); err != nil || !advanced {
		t.Fatalf("read = %v, %v", advanced, err)
	}

	if _, err := deleteMessage(ctx, db, "m2", "b"); err != nil {
		t.Fatal(err)
	}
	if got, advanced, err := markRead(ctx, db, "a", "b"); err != nil || advanced {
		t.Errorf("read after the marked message was deleted = %+v, %v, %v, want no advance", got, advanced, err)
	}
	if stored := db.reads[[2]string{"a", "b"}]; stored != (readMark{at: m2.CreatedAt, id: "m2"}) {
		t.Errorf("stored = %+v, want it held at m2", stored)
	}

	// A marker already further on than anything there is (another device read
	// ahead) is held too.
	ahead := readMark{at: m2.CreatedAt.Add(time.Hour), id: "m9"}
	db.reads[[2]string{"a", "b"}] = ahead
	if _, advanced, err := markRead(ctx, db, "a", "b"); err != nil || advanced {
		t.Errorf("read behind the marker = %v, %v, want no advance", advanced, err)
	}
	if stored := db.reads[[2]string{"a", "b"}]; stored != ahead {
		t.Errorf("stored = %+v, was rewound from %+v", stored, ahead)
	}
}

// With nothing from the peer to read, nothing is written and nothing advances: no
// messages at all, only the reader's own, or only deleted ones.
func TestMarkReadWithNothingToReadWritesNothing(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()

	check := func(name string) {
		t.Helper()
		if got, advanced, err := markRead(ctx, db, "a", "b"); err != nil || advanced || got != (readPosition{}) {
			t.Errorf("%s: read = %+v, %v, %v, want a no-op", name, got, advanced, err)
		}
		if len(db.reads) != 0 {
			t.Errorf("%s: wrote a marker: %+v", name, db.reads)
		}
	}
	check("an empty conversation")

	db.seed("m1", "a", "b", "only mine")
	check("only the reader's own messages")

	db.seed("m2", "b", "a", "gone")
	if _, err := deleteMessage(ctx, db, "m2", "b"); err != nil {
		t.Fatal(err)
	}
	check("only deleted messages")
}

// The marker is copied from a message, never from a clock. The wall clock is set
// hours past every message: a marker taken from it would sit after a message that
// was stamped before the read but commits after it, and swallow it.
func TestAMessageCommittedAfterTheMarkerIsStillUnread(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()
	m1 := db.seed("m1", "b", "a", "one")

	got, advanced, err := markRead(ctx, db, "a", "b")
	if err != nil || !advanced {
		t.Fatalf("read = %v, %v", advanced, err)
	}
	if got.At.Equal(db.now) || !got.At.Equal(m1.CreatedAt) {
		t.Errorf("marker = %v, want m1's created_at %v and not the clock %v", got.At, m1.CreatedAt, db.now)
	}

	// m2 was stamped after m1 and before the wall clock, and became visible only now.
	m2 := db.seedAt("m2", "b", "a", "two", m1.CreatedAt.Add(time.Microsecond))
	if !m2.CreatedAt.Before(db.now) {
		t.Fatal("the test needs the wall clock past m2, or it proves nothing")
	}
	counts, err := unreadCounts(ctx, db, "a")
	if err != nil || len(counts) != 1 || counts[0].PeerID != "b" || counts[0].Count != 1 {
		t.Fatalf("unread = %+v, %v, want exactly m2", counts, err)
	}
}

// Two messages can share a created_at; the id tells them apart, so a position on
// one of them splits the pair.
func TestUnreadTellsMessagesWithTheSameTimestampApartByID(t *testing.T) {
	db := newFakeDB("a", "b")
	at := db.clock.Add(time.Hour)
	db.seedAt("m1", "b", "a", "one", at)
	db.seedAt("m2", "b", "a", "two", at)
	db.reads[[2]string{"a", "b"}] = readMark{at: at, id: "m1"}

	counts, err := unreadCounts(context.Background(), db, "a")
	if err != nil || len(counts) != 1 || counts[0].Count != 1 {
		t.Fatalf("unread = %+v, %v, want only m2 (it sorts after m1)", counts, err)
	}
}

// The no-backfill promise: a marker written before positions existed has a time and
// no id, and it means what it always meant, "everything up to this instant is
// read".
func TestATimestampOnlyMarkerKeepsItsOldMeaning(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()
	db.seed("m1", "b", "a", "one")
	m2 := db.seed("m2", "b", "a", "two")
	db.reads[[2]string{"a", "b"}] = readMark{at: m2.CreatedAt.Add(30 * time.Second)} // no id

	if counts, err := unreadCounts(ctx, db, "a"); err != nil || len(counts) != 0 {
		t.Fatalf("unread = %+v, %v, want everything before the instant read", counts, err)
	}
	// Reading with nothing past the instant does not advance it, so it is not news.
	if _, advanced, err := markRead(ctx, db, "a", "b"); err != nil || advanced {
		t.Errorf("read with nothing past the marker = %v, %v, want no advance", advanced, err)
	}

	// A message after the instant is unread, and reading it writes a full position
	// over the old marker.
	m3 := db.seed("m3", "b", "a", "three")
	if counts, err := unreadCounts(ctx, db, "a"); err != nil || len(counts) != 1 || counts[0].Count != 1 {
		t.Fatalf("unread = %+v, %v, want only m3", counts, err)
	}
	got, advanced, err := markRead(ctx, db, "a", "b")
	if err != nil || !advanced || got != (readPosition{At: m3.CreatedAt, ID: "m3"}) {
		t.Errorf("read = %+v, %v, %v, want a full position at m3", got, advanced, err)
	}
	if stored := db.reads[[2]string{"a", "b"}]; stored.id != "m3" {
		t.Errorf("stored = %+v, want the id filled in", stored)
	}
}

// With no marker at all every live message is unread.
func TestNoMarkerMeansEverythingIsUnread(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.seed("m1", "b", "a", "one")
	db.seed("m2", "b", "a", "two")
	db.seed("m3", "c", "a", "three")
	db.seed("m4", "a", "b", "mine: not unread to me")

	counts, err := unreadCounts(context.Background(), db, "a")
	if err != nil {
		t.Fatal(err)
	}
	want := []UnreadEntry{{PeerID: "b", Count: 2}, {PeerID: "c", Count: 1}}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("unread = %+v, want %+v", counts, want)
	}
}

// markRead sends the database nothing but the two ids: there is no time argument
// for a second clock to ride in on.
func TestMarkReadSendsNoTimeFromTheCaller(t *testing.T) {
	db := newFakeDB("a", "b")
	if _, _, err := markRead(context.Background(), db, "a", "b"); err != nil {
		t.Fatal(err)
	}
	calls := db.callsTo(markReadSQL)
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, []any{"a", "b"}) {
		t.Errorf("markReadSQL ran as %+v, want exactly the user and peer ids", calls)
	}
}

// ---- the edit gate's lookup, and the friends-only catch-up feed -------------

func TestSQLOwnMessageRecipientIsScopedLikeAnEdit(t *testing.T) {
	mustContain(t, "ownMessageRecipientSQL", ownMessageRecipientSQL,
		"SELECT recipient_id FROM messages WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL")
}

func TestOwnMessageRecipient(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "hi")
	ctx := context.Background()

	if got, err := ownMessageRecipient(ctx, db, "m1", "a"); err != nil || got != "b" {
		t.Errorf("the sender's own message = %q, %v, want b", got, err)
	}
	for name, args := range map[string][2]string{
		"the recipient": {"m1", "b"}, "a missing message": {"nope", "a"},
	} {
		if _, err := ownMessageRecipient(ctx, db, args[0], args[1]); !errors.Is(err, errNoMessage) {
			t.Errorf("%s = %v, want errNoMessage", name, err)
		}
	}
	if _, err := deleteMessage(ctx, db, "m1", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := ownMessageRecipient(ctx, db, "m1", "a"); !errors.Is(err, errNoMessage) {
		t.Errorf("a deleted message = %v, want errNoMessage", err)
	}

	db.seed("m2", "a", "b", "hi again")
	db.failOnQuery = ownMessageRecipientSQL
	if _, err := ownMessageRecipient(ctx, db, "m2", "a"); err == nil || errors.Is(err, errNoMessage) {
		t.Errorf("a failing lookup = %v, want a real error", err)
	}
}

// A thread with someone who is no longer an accepted friend is not in the feed, in
// either direction, while a current friend's thread is, tombstones and all.
func TestCatchUpOmitsFormerFriendsAndKeepsCurrentOnes(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.befriend("a", "b")
	db.seed("ab", "a", "b", "kept")
	db.seed("ca", "c", "a", "from a former friend")
	db.seed("ac", "a", "c", "to a former friend")
	db.seed("ba", "b", "a", "also kept")
	if _, err := deleteMessage(context.Background(), db, "ba", "b"); err != nil {
		t.Fatal(err)
	}

	feed, err := listUpdates(context.Background(), db, "a", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(feed); !reflect.DeepEqual(ids, []string{"ab", "ba"}) {
		t.Errorf("catch-up = %v, want [ab ba]: the former friend's thread absent, the current friend's tombstone present", ids)
	}
	if feed[1].DeletedAt == nil || feed[1].Body != "" {
		t.Errorf("the current friend's tombstone should be carried: %+v", feed[1])
	}
}

// The filter acts before the limit: a former friend's messages must not use up a
// page and hide a current friend's behind them, so a page is a true prefix of the
// feed and the cursor walks all of it.
func TestCatchUpPageIsAPrefixOfTheFriendsOnlyFeed(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.befriend("a", "b")
	for i := range 6 {
		db.seed(fmt.Sprintf("x%d", i), "c", "a", "not a friend")
	}
	db.seed("b1", "b", "a", "one")
	db.seed("b2", "a", "b", "two")
	db.seed("b3", "b", "a", "three")

	var seen []string
	var since int64
	for range 5 {
		page, err := listUpdates(context.Background(), db, "a", since, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, idsOf(page)...)
		since = page[len(page)-1].Seq
	}
	if want := []string{"b1", "b2", "b3"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("paging two at a time saw %v, want %v", seen, want)
	}
}
