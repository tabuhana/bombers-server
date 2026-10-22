package messaging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

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

	reactions []reactionRow // message_reactions, in the order they were added

	seq   int64
	clock time.Time
	now   time.Time // the database's wall clock: no read-state statement may consult it, so a test sets it far from every message to prove none does

	calls       []call
	commits     int
	rollbacks   int
	beginErr    error
	commitErr   error  // returned by Commit AFTER the writes took effect: a commit whose answer was lost
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

// reactionRow is a message_reactions row. The primary key is (message, user, emoji).
type reactionRow struct {
	messageID, userID, emoji string
	at                       time.Time
}

// react adds a reaction the way the table would: a second identical one is the same row.
func (d *fakeDB) react(messageID, userID, emoji string) {
	for _, r := range d.reactions {
		if r.messageID == messageID && r.userID == userID && r.emoji == emoji {
			return
		}
	}
	d.reactions = append(d.reactions, reactionRow{messageID, userID, emoji, d.clock.Add(time.Duration(len(d.reactions)) * time.Second)})
}

// pinned is how many messages of the pair are pinned.
func (d *fakeDB) pinned(a, b string) int {
	n := 0
	for _, m := range d.msgs {
		if inPair(m, a, b) && m.PinnedAt != nil {
			n++
		}
	}
	return n
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
		Kind:      kindText, // the column default
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
	case deleteReactionsSQL:
		kept := d.reactions[:0:0]
		for _, r := range d.reactions {
			if r.messageID != args[0].(string) {
				kept = append(kept, r)
			}
		}
		d.reactions = kept
		return pgconn.CommandTag{}, nil
	case insertReactionSQL:
		// The primary key (message_id, user_id, emoji): the same person adding the same
		// emoji twice is refused the way Postgres refuses it. addReaction must never
		// get that far.
		messageID, userID, emoji := args[0].(string), args[1].(string), args[2].(string)
		for _, r := range d.reactions {
			if r.messageID == messageID && r.userID == userID && r.emoji == emoji {
				return pgconn.CommandTag{}, &pgconn.PgError{Code: "23505", ConstraintName: "message_reactions_pkey"}
			}
		}
		d.react(messageID, userID, emoji)
		return pgconn.CommandTag{}, nil
	case removeReactionSQL:
		messageID, userID, emoji := args[0].(string), args[1].(string), args[2].(string)
		kept := d.reactions[:0:0]
		for _, r := range d.reactions {
			if !(r.messageID == messageID && r.userID == userID && r.emoji == emoji) {
				kept = append(kept, r)
			}
		}
		d.reactions = kept
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
	case messageExistsSQL:
		return fakeRow{vals: []any{d.find(args[0].(string)) != nil}}
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
		replyTo, _ := args[5].(*string)
		kind, _ := args[6].(string)
		payload, _ := args[7].([]byte)
		if nonce != "" {
			for _, m := range d.msgs {
				if m.SenderID == sender && m.RecipientID == recipient && m.Nonce == nonce {
					return fakeRow{err: &pgconn.PgError{Code: "23505", ConstraintName: "messages_sender_recipient_nonce_idx"}}
				}
			}
		}
		m := d.seed(args[0].(string), sender, recipient, args[3].(string))
		d.find(m.ID).Nonce = nonce
		d.find(m.ID).ReplyToID = replyTo
		m.ReplyToID = replyTo
		d.find(m.ID).Kind, d.find(m.ID).Payload = kind, payload
		m.Kind, m.Payload = kind, payload
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
		if m == nil || m.SenderID != args[1].(string) || m.DeletedAt != nil || !kindHasWords(m.Kind) {
			return fakeRow{err: pgx.ErrNoRows}
		}
		return fakeRow{vals: []any{m.RecipientID}}
	case editMessageSQL:
		m := d.find(args[0].(string))
		if m == nil || m.SenderID != args[1].(string) || m.DeletedAt != nil || !kindHasWords(m.Kind) {
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
		m.Body, m.DeletedAt, m.Seq, m.PinnedAt, m.ReplyToID, m.Payload = "", &at, d.seq, nil, nil, nil
		return messageRow(*m)
	case replyTargetSQL:
		a, b, id := args[0].(string), args[1].(string), args[2].(string)
		m := d.find(id)
		return fakeRow{vals: []any{m != nil && inPair(*m, a, b)}}
	case participantMessageSQL:
		m := d.find(args[0].(string))
		user := args[1].(string)
		if m == nil || (m.SenderID != user && m.RecipientID != user) || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		return messageRow(*m)
	case countPinsSQL:
		return fakeRow{vals: []any{int64(d.pinned(args[0].(string), args[1].(string)))}}
	case pinMessageSQL:
		m := d.find(args[0].(string))
		if m == nil || m.PinnedAt != nil || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		d.seq++
		at := d.clock.Add(time.Duration(d.seq) * time.Minute)
		m.PinnedAt, m.Seq = &at, d.seq
		return messageRow(*m)
	case unpinMessageSQL:
		m := d.find(args[0].(string))
		if m == nil || m.PinnedAt == nil || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		d.seq++
		m.PinnedAt, m.Seq = nil, d.seq
		return messageRow(*m)
	case reactionStateSQL:
		messageID, emoji, userID := args[0].(string), args[1].(string), args[2].(string)
		var s reactionState
		distinct := map[string]bool{}
		for _, r := range d.reactions {
			if r.messageID != messageID {
				continue
			}
			s.Total++
			distinct[r.emoji] = true
			if r.emoji == emoji {
				s.Used = true
				if r.userID == userID {
					s.Mine = true
				}
			}
		}
		s.Distinct = int64(len(distinct))
		return fakeRow{vals: []any{s.Total, s.Distinct, s.Used, s.Mine}}
	case touchMessageSQL:
		m := d.find(args[0].(string))
		if m == nil || m.DeletedAt != nil {
			return fakeRow{err: pgx.ErrNoRows}
		}
		d.seq++
		m.Seq = d.seq
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

	case listAroundOlderSQL, listAroundNewerSQL:
		// Everything the statement says: the pair, live rows, on the right side of the
		// anchor's (created_at, id) (the older side INCLUDES the anchor), in that side's
		// order, at most the number of rows asked for.
		a, b := args[0].(string), args[1].(string)
		anchor := cursor{CreatedAt: args[2].(time.Time), ID: args[3].(string)}
		limit := args[4].(int)
		older := sql == listAroundOlderSQL
		var hit []messageRecord
		for _, m := range d.msgs {
			atOrBefore := olderThan(m.CreatedAt, m.ID, anchor) || (m.CreatedAt.Equal(anchor.CreatedAt) && m.ID == anchor.ID)
			if inPair(m, a, b) && m.DeletedAt == nil && atOrBefore == older {
				hit = append(hit, m)
			}
		}
		newestFirst(hit)
		if !older {
			slices.Reverse(hit)
		}
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

	case listPinsSQL:
		a, b, limit := args[0].(string), args[1].(string), args[2].(int)
		var hit []messageRecord
		for _, m := range d.msgs {
			if inPair(m, a, b) && m.PinnedAt != nil && m.DeletedAt == nil {
				hit = append(hit, m)
			}
		}
		sort.Slice(hit, func(i, j int) bool {
			if !hit[i].PinnedAt.Equal(*hit[j].PinnedAt) {
				return hit[i].PinnedAt.After(*hit[j].PinnedAt)
			}
			return hit[i].ID > hit[j].ID
		})
		return messageRows(clip(hit, limit)), nil

	case replyPreviewsSQL:
		// The join: the original must exist AND sit in the reply's own conversation.
		var data [][]any
		for _, id := range args[0].([]string) {
			m := d.find(id)
			if m == nil || m.ReplyToID == nil {
				continue
			}
			r := d.find(*m.ReplyToID)
			if r == nil || !inPair(*r, m.SenderID, m.RecipientID) {
				continue
			}
			data = append(data, []any{m.ID, r.ID, r.SenderID, r.Body, r.DeletedAt != nil})
		}
		return &fakeRows{data: data}, nil

	case reactionsSQL:
		// GROUP BY message, emoji; ORDER BY message, first use, emoji.
		ids, viewer := args[0].([]string), args[1].(string)
		type group struct {
			messageID, emoji string
			count            int64
			me               bool
			first            time.Time
		}
		var groups []*group
		for _, r := range d.reactions {
			if !slices.Contains(ids, r.messageID) {
				continue
			}
			var g *group
			for _, have := range groups {
				if have.messageID == r.messageID && have.emoji == r.emoji {
					g = have
				}
			}
			if g == nil {
				g = &group{messageID: r.messageID, emoji: r.emoji, first: r.at}
				groups = append(groups, g)
			}
			g.count++
			g.me = g.me || r.userID == viewer
			if r.at.Before(g.first) {
				g.first = r.at
			}
		}
		sort.Slice(groups, func(i, j int) bool {
			a, b := groups[i], groups[j]
			if a.messageID != b.messageID {
				return a.messageID < b.messageID
			}
			if !a.first.Equal(b.first) {
				return a.first.Before(b.first)
			}
			return a.emoji < b.emoji
		})
		var data [][]any
		for _, g := range groups {
			data = append(data, []any{g.messageID, g.emoji, g.count, g.me})
		}
		return &fakeRows{data: data}, nil

	case searchMessagesSQL:
		// Everything the statement says, in Go: the caller's messages with a still-
		// accepted friend (acceptedCTE), narrowed to `with` when given, live rows only,
		// whose words match, strictly older than the keyset position, newest first by
		// (created_at, id), at most the page size. How Postgres decides a match is
		// tsMatches' stand-in, not the real thing.
		user, query := args[0].(string), args[1].(string)
		with, _ := args[2].(string)
		var before *cursor
		if args[3] != nil {
			before = &cursor{CreatedAt: args[3].(time.Time), ID: args[4].(string)}
		}
		limit := args[5].(int)
		var hit []messageRecord
		for _, m := range d.msgs {
			if m.SenderID != user && m.RecipientID != user {
				continue
			}
			peer := m.SenderID
			if peer == user {
				peer = m.RecipientID
			}
			if !d.friends[pair(user, peer)] || (with != "" && peer != with) || m.DeletedAt != nil {
				continue
			}
			if !tsMatches(m.Body, query) || (before != nil && !olderThan(m.CreatedAt, m.ID, *before)) {
				continue
			}
			hit = append(hit, m)
		}
		newestFirst(hit)
		return messageRows(clip(hit, limit)), nil

	case mentionableSQL:
		// acceptedCTE narrowed to the candidates.
		user, candidates := args[0].(string), args[1].([]string)
		var data [][]any
		for _, c := range candidates {
			if d.friends[pair(user, c)] {
				data = append(data, []any{c})
			}
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

// tsWords is body_tsv in miniature: the lower-cased words of a text, so an empty body
// (a tombstone's) has none. There is no stemming and there are no stop words; the
// fake checks the wiring around a match, never Postgres's idea of one.
func tsWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var tsQueryToken = regexp.MustCompile(`-?"[^"]*"|-?\S+`)

// tsMatches is body_tsv @@ websearch_to_tsquery(..) in miniature: every bare word and
// every "quoted phrase" of the query must be in the body, a -excluded one must not
// be, and a query with no words at all matches nothing (an empty tsquery).
func tsMatches(body, query string) bool {
	have := tsWords(body)
	contains := func(phrase []string) bool {
		for i := 0; i+len(phrase) <= len(have); i++ {
			if slices.Equal(have[i:i+len(phrase)], phrase) {
				return true
			}
		}
		return false
	}
	anyTerm := false
	for _, tok := range tsQueryToken.FindAllString(query, -1) {
		negated := strings.HasPrefix(tok, "-")
		words := tsWords(tok)
		if len(words) == 0 {
			continue
		}
		anyTerm = true
		if found := contains(words); found == negated {
			return false
		}
	}
	return anyTerm
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
	return t.db.commitErr
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
	return []any{m.ID, m.Seq, m.SenderID, m.RecipientID, m.Body, m.CreatedAt, m.EditedAt, m.DeletedAt, m.ReplyToID, m.PinnedAt, m.Kind, m.Payload}
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
	mustContain(t, "insertMessageSQL", insertMessageSQL, "created_at, nonce, reply_to_id, kind, payload) VALUES ($1, $2, $3, $4, clock_timestamp(),")
	mustNotContain(t, "insertMessageSQL", insertMessageSQL, "now()", "DEFAULT")
	mustContain(t, "editMessageSQL", editMessageSQL, "edited_at = now()")
	mustContain(t, "deleteMessageSQL", deleteMessageSQL, "deleted_at = now()")
	mustNotContain(t, "editMessageSQL", editMessageSQL, "created_at =")
	mustNotContain(t, "deleteMessageSQL", deleteMessageSQL, "created_at =")

	// The statement that carries the stamp runs after the lock, in the same
	// transaction, through the path a send takes.
	db := newFakeDB("a", "b")
	if _, err := writeSeq(context.Background(), db.begin, func(tx dbExecutor) (*messageRecord, error) {
		m, _, err := insertOnce(context.Background(), tx, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi"})
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
	mustContain(t, "delete", deleteMessageSQL, "SET body = '', payload = NULL, deleted_at = now()")
}

// An absent nonce is stored as NULL, so the partial unique index never sees an
// empty string, and the repeat lookup states the equality that index serves.
func TestSQLNonceIsNullWhenAbsentAndLookedUpByConversationAndNonce(t *testing.T) {
	mustContain(t, "insertMessageSQL", insertMessageSQL,
		"INSERT INTO messages (id, sender_id, recipient_id, body, created_at, nonce, reply_to_id, kind, payload) VALUES ($1, $2, $3, $4, clock_timestamp(), NULLIF($5::text, ''), $6::text, $7::text, $8::jsonb)",
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
		return insertMessage(context.Background(), tx, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi"})
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

	first, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
	if err != nil || !created {
		t.Fatalf("first = %+v, created %v, err %v", first, created, err)
	}
	seqAfterFirst := db.seq

	again, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m2", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
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
	if _, _, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := editMessage(ctx, db, "m1", "a", "hello"); err != nil {
		t.Fatal(err)
	}

	again, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m2", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
	if err != nil || created || again.Body != "hello" || again.EditedAt == nil {
		t.Errorf("repeat = %+v, created %v, err %v, want the edited original", again, created, err)
	}
}

func TestInsertOnceWithoutANonceAlwaysInserts(t *testing.T) {
	db := newFakeDB("a", "b")
	ctx := context.Background()

	for _, id := range []string{"m1", "m2"} {
		_, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: id, SenderID: "a", RecipientID: "b", Body: "same"})
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

	if _, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "from a", Nonce: "shared"}); err != nil || !created {
		t.Fatalf("a: created %v, err %v", created, err)
	}
	got, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m2", SenderID: "b", RecipientID: "a", Body: "from b", Nonce: "shared"})
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

	if _, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "to b", Nonce: "n1"}); err != nil || !created {
		t.Fatalf("to b: created %v, err %v", created, err)
	}
	got, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m2", SenderID: "a", RecipientID: "c", Body: "to c", Nonce: "n1"})
	if err != nil || !created || got.ID != "m2" || got.RecipientID != "c" {
		t.Errorf("to c = %+v, created %v, err %v, want a separate new message", got, created, err)
	}
	again, created, err := insertOnce(ctx, db, &messageRecord{Kind: kindText, ID: "m3", SenderID: "a", RecipientID: "c", Body: "to c", Nonce: "n1"})
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

	_, _, err := insertOnce(context.Background(), db, &messageRecord{Kind: kindText, ID: "m1", SenderID: "a", RecipientID: "b", Body: "hi", Nonce: "n1"})
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

// ---- replies, pins, reactions and mentions: the SQL ------------------------

// The shape has two more columns of the row itself; the other parts of it are read
// separately. The column list and its scan destinations are one pair.
func TestSQLMessageColumnsCarryTheReplyLinkAndThePin(t *testing.T) {
	mustContain(t, "messageColumns", messageColumns, ", reply_to_id, pinned_at")
	mustNotContain(t, "messageColumns", messageColumns, "pinned_by", "body_tsv", "nonce")
	if cols, dests := len(strings.Split(messageColumns, ",")), len(messageDests(&messageRecord{})); cols != dests {
		t.Errorf("messageColumns names %d columns but messageDests scans %d", cols, dests)
	}
}

// reply_to_id is written by the insert and by nothing else, which is what makes it
// immutable; the pin columns are written only by the pin statements and the delete.
func TestSQLNothingButTheInsertWritesTheReplyLinkAndOnlyDeleteClearsIt(t *testing.T) {
	mustContain(t, "insertMessageSQL", insertMessageSQL, "reply_to_id, kind, payload) VALUES", "$6::text")
	for name, sql := range map[string]string{
		"editMessageSQL": editMessageSQL, "pinMessageSQL": pinMessageSQL, "unpinMessageSQL": unpinMessageSQL,
	} {
		set, _, _ := strings.Cut(sql, "RETURNING") // the columns it hands back are not what it writes
		mustNotContain(t, name, set, "reply_to_id")
	}
	mustContain(t, "deleteMessageSQL", deleteMessageSQL, "reply_to_id = NULL")
	set, _, _ := strings.Cut(editMessageSQL, "RETURNING")
	mustNotContain(t, "editMessageSQL", set, "pinned_at", "pinned_by")
}

// Replying to a deleted message must be allowed, so the check asks about the pair
// and nothing else. It is the id of the message that matters, not its state.
func TestSQLReplyTargetIsScopedToThePairAndIgnoresDeletion(t *testing.T) {
	mustContain(t, "replyTargetSQL", replyTargetSQL,
		"WHERE id = $3 AND ((sender_id = $1 AND recipient_id = $2) OR (sender_id = $2 AND recipient_id = $1))")
	mustNotContain(t, "replyTargetSQL", replyTargetSQL, "deleted_at")
}

// The preview is resolved when read, and only ever from the reply's OWN
// conversation, in either direction.
func TestSQLReplyPreviewIsReadAtReadTimeAndScopedToTheConversation(t *testing.T) {
	mustContain(t, "replyPreviewsSQL", replyPreviewsSQL,
		"FROM messages m JOIN messages r ON r.id = m.reply_to_id",
		"WHERE m.id = ANY($1)",
		"(r.sender_id = m.sender_id AND r.recipient_id = m.recipient_id)",
		"(r.sender_id = m.recipient_id AND r.recipient_id = m.sender_id)",
		"r.deleted_at IS NOT NULL",
	)
	// A deleted original is still joined: that is how it is reported deleted.
	mustNotContain(t, "replyPreviewsSQL", replyPreviewsSQL, "r.deleted_at IS NULL")
}

// A pin draws a seq, so the catch-up feed carries it; and the WHERE makes a repeat
// zero rows, so a repeat can never draw one.
func TestSQLPinAndUnpinDrawASeqAndCannotRepeat(t *testing.T) {
	mustContain(t, "pinMessageSQL", pinMessageSQL,
		"SET pinned_at = clock_timestamp(), pinned_by = $2, seq = nextval('messages_seq')",
		"WHERE id = $1 AND pinned_at IS NULL AND deleted_at IS NULL",
		"RETURNING "+messageColumns,
	)
	mustContain(t, "unpinMessageSQL", unpinMessageSQL,
		"SET pinned_at = NULL, pinned_by = NULL, seq = nextval('messages_seq')",
		"WHERE id = $1 AND pinned_at IS NOT NULL AND deleted_at IS NULL",
		"RETURNING "+messageColumns,
	)
	mustNotContain(t, "pinMessageSQL", pinMessageSQL, "now()")
	mustContain(t, "deleteMessageSQL", deleteMessageSQL, "pinned_at = NULL, pinned_by = NULL")
}

// A pin is the conversation's: both lookups match the party as sender OR recipient,
// where edit's matches the sender only.
func TestSQLPinLookupIsAnyPartyAndNeverADeletedMessage(t *testing.T) {
	mustContain(t, "participantMessageSQL", participantMessageSQL,
		"WHERE id = $1 AND (sender_id = $2 OR recipient_id = $2) AND deleted_at IS NULL")
	mustContain(t, "ownMessageRecipientSQL", ownMessageRecipientSQL, "sender_id = $2")
	mustContain(t, "countPinsSQL", countPinsSQL, conversationPairSQLText(), "AND pinned_at IS NOT NULL")
	mustContain(t, "listPinsSQL", listPinsSQL,
		conversationPairSQLText(),
		"AND pinned_at IS NOT NULL AND deleted_at IS NULL",
		"ORDER BY pinned_at DESC, id DESC",
		"LIMIT $3",
	)
}

func conversationPairSQLText() string {
	return strings.Join(strings.Fields(conversationPairSQL), " ")
}

func TestSQLReactionsAreTalliedPerEmojiInFirstUseOrder(t *testing.T) {
	mustContain(t, "reactionsSQL", reactionsSQL,
		"SELECT message_id, emoji, COUNT(*)::bigint, bool_or(user_id = $2::text)",
		"FROM message_reactions WHERE message_id = ANY($1)",
		"GROUP BY message_id, emoji",
		"ORDER BY message_id, MIN(created_at), emoji",
	)
}

// Mentions are judged by the same definition of friend as the feed and the list.
func TestSQLMentionsReadThroughTheSharedFriendDefinition(t *testing.T) {
	mustContain(t, "mentionableSQL", mentionableSQL,
		"WITH "+strings.Join(strings.Fields(acceptedCTE), " "),
		"SELECT peer_id FROM accepted WHERE peer_id = ANY($2)",
	)
	if n := strings.Count(strings.Join(strings.Fields(mentionableSQL), " "), "state = 'accepted'"); n != 1 {
		t.Errorf("mentionableSQL states the friendship rule %d times, want once", n)
	}
}

// The migration the SQL above stands on. A generated column must be IMMUTABLE and
// only the two-argument to_tsvector is, so the one-argument form must not appear
// anywhere; and there is no backfill, because everything is nullable or generated.
func TestMigrationDMChatFeatures(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/20261020120000_dm_chat_features.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration has no Down section")
	}
	mustContain(t, "migration up", up,
		"-- +goose Up",
		"ADD COLUMN reply_to_id TEXT REFERENCES messages(id) ON DELETE SET NULL,",
		"ADD COLUMN pinned_at TIMESTAMPTZ,",
		"ADD COLUMN pinned_by TEXT REFERENCES users(id) ON DELETE SET NULL;",
		"CREATE INDEX messages_pinned_idx ON messages (pinned_at DESC) WHERE pinned_at IS NOT NULL;",
		"CREATE TABLE message_reactions (",
		"message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,",
		"user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,",
		"emoji TEXT NOT NULL,",
		"created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),",
		"PRIMARY KEY (message_id, user_id, emoji)",
		"ADD COLUMN body_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', body)) STORED;",
		"CREATE INDEX messages_body_tsv_idx ON messages USING GIN (body_tsv);",
	)
	// No backfill and nothing that rewrites a row.
	mustNotContain(t, "migration up", up, "UPDATE messages", "DELETE FROM", "pinned_at TIMESTAMPTZ NOT NULL")
	// Every use of to_tsvector names its configuration.
	for _, call := range regexp.MustCompile(`to_tsvector\([^)]*\)`).FindAllString(string(raw), -1) {
		if !strings.HasPrefix(call, "to_tsvector('english',") {
			t.Errorf("%s is not the two-argument form, which is the only immutable one", call)
		}
	}
	mustContain(t, "migration down", down,
		"DROP INDEX messages_body_tsv_idx;",
		"DROP COLUMN body_tsv;",
		"DROP TABLE message_reactions;",
		"DROP INDEX messages_pinned_idx;",
		"DROP COLUMN pinned_by,",
		"DROP COLUMN pinned_at,",
		"DROP COLUMN reply_to_id;",
	)
}

// ---- the store functions ----------------------------------------------------

func TestPinMessageReportsWhetherItChangedAnything(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.seed("m1", "a", "b", "hi")

	m, changed, err := pinMessage(context.Background(), db, "m1", "b")
	if err != nil || !changed || m.PinnedAt == nil || m.Seq != 2 {
		t.Fatalf("first pin = %+v changed=%v err=%v, want it pinned with seq 2", m, changed, err)
	}
	m, changed, err = pinMessage(context.Background(), db, "m1", "a")
	if err != nil || changed || m.Seq != 2 || db.seq != 2 {
		t.Errorf("repeat pin = %+v changed=%v err=%v seq=%d, want unchanged and no seq drawn", m, changed, err, db.seq)
	}
	m, changed, err = unpinMessage(context.Background(), db, "m1", "b")
	if err != nil || !changed || m.PinnedAt != nil || m.Seq != 3 {
		t.Errorf("unpin = %+v changed=%v err=%v, want it unpinned with seq 3", m, changed, err)
	}
	if _, changed, err = unpinMessage(context.Background(), db, "m1", "a"); err != nil || changed || db.seq != 3 {
		t.Errorf("repeat unpin changed=%v err=%v seq=%d, want a no-op", changed, err, db.seq)
	}

	// Not a party, and not a message.
	for _, who := range []struct{ id, user string }{{"m1", "c"}, {"nope", "a"}} {
		if _, _, err := pinMessage(context.Background(), db, who.id, who.user); !errors.Is(err, errNoMessage) {
			t.Errorf("pin %s by %s = %v, want errNoMessage", who.id, who.user, err)
		}
		if _, _, err := unpinMessage(context.Background(), db, who.id, who.user); !errors.Is(err, errNoMessage) {
			t.Errorf("unpin %s by %s = %v, want errNoMessage", who.id, who.user, err)
		}
	}
}

func TestPinMessageStopsAtTheCapButStillAcceptsARepeat(t *testing.T) {
	db := newFakeDB("a", "b")
	for i := range maxPins {
		id := "p" + strconv.Itoa(100+i)
		db.seed(id, "a", "b", "x")
		at := db.clock.Add(time.Duration(i) * time.Second)
		db.find(id).PinnedAt = &at
	}
	db.seed("extra", "b", "a", "one too many")
	seq := db.seq

	if _, _, err := pinMessage(context.Background(), db, "extra", "a"); !errors.Is(err, errPinLimit) {
		t.Errorf("pin past the cap = %v, want errPinLimit", err)
	}
	if db.seq != seq || db.find("extra").PinnedAt != nil {
		t.Error("a refused pin wrote something")
	}
	if _, changed, err := pinMessage(context.Background(), db, "p100", "b"); err != nil || changed {
		t.Errorf("repeat at the cap changed=%v err=%v, want a quiet success", changed, err)
	}
}

func TestPinMessageSurfacesDatabaseErrors(t *testing.T) {
	for _, sql := range []string{participantMessageSQL, countPinsSQL, pinMessageSQL} {
		db := newFakeDB("a", "b")
		db.seed("m1", "a", "b", "hi")
		db.failOnQuery = sql
		_, _, err := pinMessage(context.Background(), db, "m1", "a")
		if err == nil || errors.Is(err, errNoMessage) || errors.Is(err, errPinLimit) {
			t.Errorf("a failing %q gave %v, want a plain error", strings.Fields(sql)[0:3], err)
		}
	}
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "hi")
	pinMessage(context.Background(), db, "m1", "a")
	db.failOnQuery = unpinMessageSQL
	if _, _, err := unpinMessage(context.Background(), db, "m1", "a"); err == nil || errors.Is(err, errNoMessage) {
		t.Errorf("a failing unpin gave %v, want a plain error", err)
	}
}

func TestListPinsIsThePairsPinsNewestFirstAndNobodyElses(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	for _, m := range []struct{ id, from, to string }{{"m1", "a", "b"}, {"m2", "b", "a"}, {"m3", "a", "b"}, {"x", "a", "c"}} {
		db.seed(m.id, m.from, m.to, "x")
	}
	for _, id := range []string{"m2", "x", "m1"} {
		if _, _, err := pinMessage(context.Background(), db, id, "a"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listPins(context.Background(), db, "b", "a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"m1", "m2"}; !reflect.DeepEqual(idsOf(got), want) {
		t.Errorf("pins = %v, want %v", idsOf(got), want)
	}
	if args := db.callsTo(listPinsSQL)[0].args; args[2] != maxPins {
		t.Errorf("the list was limited to %v, want the cap %d", args[2], maxPins)
	}
}

func TestReplyTargetInPair(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.seed("ab", "a", "b", "x")
	db.seed("bc", "b", "c", "x")
	db.seed("gone", "b", "a", "x")
	db.find("gone").DeletedAt = &db.clock

	for _, tc := range []struct {
		name, a, b, id string
		want           bool
	}{
		{"sent by the first", "a", "b", "ab", true},
		{"the pair the other way round", "b", "a", "ab", true},
		{"deleted still counts", "a", "b", "gone", true},
		{"another pair's", "a", "b", "bc", false},
		{"nonexistent", "a", "b", "nope", false},
	} {
		got, err := replyTargetInPair(context.Background(), db, tc.a, tc.b, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v, want %v", tc.name, got, err, tc.want)
		}
	}
	db.failOnQuery = replyTargetSQL
	if _, err := replyTargetInPair(context.Background(), db, "a", "b", "ab"); err == nil {
		t.Error("a failing check must be an error, not a no")
	}
}

func TestDeleteMessageClearsPinReplyLinkAndReactionsAndReportsFailures(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("orig", "b", "a", "x")
	db.seed("m1", "a", "b", "pinned reply")
	link := "orig"
	db.find("m1").ReplyToID = &link
	if _, _, err := pinMessage(context.Background(), db, "m1", "a"); err != nil {
		t.Fatal(err)
	}
	db.react("m1", "b", "👍")
	db.react("orig", "a", "👍")

	m, err := deleteMessage(context.Background(), db, "m1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "" || m.PinnedAt != nil || m.ReplyToID != nil || m.DeletedAt == nil {
		t.Errorf("tombstone = %+v, want no words, no pin and no reply link", m)
	}
	if len(db.reactions) != 1 || db.reactions[0].messageID != "orig" {
		t.Errorf("reactions left = %+v, want only the other message's", db.reactions)
	}

	db.seed("m2", "a", "b", "again")
	db.failOnQuery = deleteReactionsSQL
	if _, err := deleteMessage(context.Background(), db, "m2", "a"); err == nil {
		t.Error("a failure clearing reactions must fail the delete, so it rolls back")
	}
}

func TestLoadRepliesOnlyResolvesWithinTheConversation(t *testing.T) {
	db := newFakeDB("a", "b", "c")
	db.seed("orig", "b", "a", "the original")
	db.seed("elsewhere", "b", "c", "private")
	db.seed("good", "a", "b", "re")
	db.seed("bad", "a", "b", "re")
	db.seed("none", "a", "b", "not a reply")
	good, bad := "orig", "elsewhere"
	db.find("good").ReplyToID = &good
	db.find("bad").ReplyToID = &bad

	got, err := loadReplies(context.Background(), db, []string{"good", "bad", "none"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]replyRecord{"good": {MessageID: "good", OriginalID: "orig", SenderID: "b", Body: "the original"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replies = %+v, want %+v", got, want)
	}
}

func TestLoadReactionsGroupsPerMessageAndEmoji(t *testing.T) {
	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "x")
	db.seed("m2", "a", "b", "x")
	db.react("m2", "b", "🎉")
	db.react("m1", "a", "👍")
	db.react("m1", "b", "👍")
	db.react("m1", "b", "😂")

	got, err := loadReactions(context.Background(), db, []string{"m1", "m2"}, "a")
	if err != nil {
		t.Fatal(err)
	}
	want := []reactionRecord{
		{"m1", "👍", 2, true},
		{"m1", "😂", 1, false},
		{"m2", "🎉", 1, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reactions = %+v, want %+v", got, want)
	}
	// Only the messages asked about.
	if got, _ := loadReactions(context.Background(), db, []string{"m2"}, "a"); len(got) != 1 {
		t.Errorf("asked about m2, got %+v", got)
	}
}

// ---- reactions and search: the SQL, pinned as text ---------------------------

// A reaction is a row in another table, so the message itself must be touched to draw
// a seq: that is the whole mechanism by which an offline client ever hears of it. The
// touch changes no column but seq, and never on a tombstone.
func TestSQLAReactionDrawsASeqOnTheMessageAndChangesNothingElse(t *testing.T) {
	mustContain(t, "touchMessageSQL", touchMessageSQL,
		"UPDATE messages",
		"SET seq = nextval('messages_seq')",
		"WHERE id = $1 AND deleted_at IS NULL",
		"RETURNING "+messageColumns,
	)
	set, _, _ := strings.Cut(touchMessageSQL, "RETURNING")
	mustNotContain(t, "touchMessageSQL SET", set, "body", "edited_at", "created_at", "pinned_at", "reply_to_id", "deleted_at =")

	mustContain(t, "insertReactionSQL", insertReactionSQL,
		"INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, $3)")
	// A reaction is removed by all three halves of its key: never one person's by
	// another's id, never every use of an emoji.
	mustContain(t, "removeReactionSQL", removeReactionSQL,
		"DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3")
	// Neither write is allowed to be an upsert that quietly draws nothing: the repeat is
	// decided by the lookup, in Go, inside the lock.
	mustNotContain(t, "insertReactionSQL", insertReactionSQL, "ON CONFLICT")
}

func TestSQLReactionStateIsOneRowCountingWhatTheCapsNeed(t *testing.T) {
	mustContain(t, "reactionStateSQL", reactionStateSQL,
		"SELECT COUNT(*)::bigint,",
		"COUNT(DISTINCT emoji)::bigint,",
		"COALESCE(bool_or(emoji = $2::text), false),",
		"COALESCE(bool_or(emoji = $2::text AND user_id = $3::text), false)",
		"FROM message_reactions WHERE message_id = $1",
	)
	// An aggregate with a GROUP BY would return no row for a message with no reactions.
	mustNotContain(t, "reactionStateSQL", reactionStateSQL, "GROUP BY")
}

// Search is friends-only through the SAME acceptedCTE the catch-up feed and the
// conversation list read through; a search that can reach a thread history refuses
// to serve is a hole.
func TestSQLSearchReadsThroughTheSharedFriendDefinition(t *testing.T) {
	def := strings.Join(strings.Fields(acceptedCTE), " ")
	mustContain(t, "searchMessagesSQL", searchMessagesSQL, "WITH "+def)
	norm := strings.Join(strings.Fields(searchMessagesSQL), " ")
	if n := strings.Count(norm, "state = 'accepted'"); n != 1 {
		t.Errorf("searchMessagesSQL states the friendship rule %d times, want once, through acceptedCTE", n)
	}
	// Each branch is restricted to the accepted peers BEFORE its LIMIT, so a former
	// friend's messages can't use up a page.
	for _, branch := range []string{
		"WHERE sender_id = $1 AND recipient_id IN (SELECT peer_id FROM accepted)",
		"WHERE recipient_id = $1 AND sender_id IN (SELECT peer_id FROM accepted)",
	} {
		mustContain(t, "searchMessagesSQL", searchMessagesSQL, branch)
	}
}

// Newest first by (created_at, id) and never by relevance or seq; keyset, not OFFSET;
// a tombstone never matches; and the user's text only ever reaches a function that
// does not throw on punctuation.
func TestSQLSearchOrdersByCreationPagesByKeysetAndNeverMatchesATombstone(t *testing.T) {
	norm := strings.Join(strings.Fields(searchMessagesSQL), " ")
	mustContain(t, "searchMessagesSQL", searchMessagesSQL,
		"UNION ALL",
		") hits ORDER BY created_at DESC, id DESC LIMIT $6",
		"($4::timestamptz IS NULL OR (created_at, id) < ($4::timestamptz, $5::text))",
		"($3::text IS NULL OR recipient_id = $3::text)",
		"($3::text IS NULL OR sender_id = $3::text)",
	)
	mustNotContain(t, "searchMessagesSQL", searchMessagesSQL, "ts_rank", "ORDER BY seq", "OFFSET", "plainto_tsquery", "phraseto_tsquery")
	if n := strings.Count(norm, "ORDER BY created_at DESC, id DESC"); n != 3 {
		t.Errorf("searchMessagesSQL orders %d times by (created_at DESC, id DESC), want once per branch and once overall", n)
	}
	if n := strings.Count(norm, "deleted_at IS NULL"); n != 2 {
		t.Errorf("searchMessagesSQL filters deleted_at %d times, want once per branch", n)
	}
	// Every match goes through the generated column's own configuration, as the one
	// function that takes what a person types.
	const match = "body_tsv @@ websearch_to_tsquery('english', $2)"
	if n := strings.Count(norm, match); n != 2 {
		t.Errorf("searchMessagesSQL contains %q %d times, want once per branch", match, n)
	}
	if strings.Count(norm, "to_tsquery(") != strings.Count(norm, "websearch_to_tsquery(") {
		t.Error("searchMessagesSQL calls a tsquery function other than websearch_to_tsquery, which throws on punctuation")
	}
	// The query text is a parameter, never part of the statement.
	mustContain(t, "searchMessagesSQL", searchMessagesSQL, "$2")
	mustNotContain(t, "searchMessagesSQL", searchMessagesSQL, "||", "format(")
}

// ---- reactions and search: the store functions -------------------------------

func TestAddAndRemoveReactionReportWhetherTheyChangedAnything(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB("a", "b", "c")
	db.seed("m1", "a", "b", "hi")

	m, changed, err := addReaction(ctx, db, "m1", "b", "👍")
	if err != nil || !changed || m.Seq != 2 || db.seq != 2 || len(db.reactions) != 1 {
		t.Fatalf("first add = %+v changed=%v err=%v rows=%d, want it added with seq 2", m, changed, err, len(db.reactions))
	}
	m, changed, err = addReaction(ctx, db, "m1", "b", "👍")
	if err != nil || changed || m.Seq != 2 || db.seq != 2 || len(db.reactions) != 1 {
		t.Errorf("repeat add = %+v changed=%v err=%v, want unchanged and no seq drawn", m, changed, err)
	}
	// The other person's identical emoji is its own row.
	if _, changed, err = addReaction(ctx, db, "m1", "a", "👍"); err != nil || !changed || len(db.reactions) != 2 || db.seq != 3 {
		t.Errorf("the other party's add changed=%v err=%v rows=%d seq=%d", changed, err, len(db.reactions), db.seq)
	}
	m, changed, err = removeReaction(ctx, db, "m1", "b", "👍")
	if err != nil || !changed || m.Seq != 4 || len(db.reactions) != 1 || db.reactions[0].userID != "a" {
		t.Errorf("remove = %+v changed=%v err=%v rows=%+v, want only b's removed, seq 4", m, changed, err, db.reactions)
	}
	if _, changed, err = removeReaction(ctx, db, "m1", "b", "👍"); err != nil || changed || db.seq != 4 {
		t.Errorf("repeat remove changed=%v err=%v seq=%d, want a no-op", changed, err, db.seq)
	}
	if _, changed, err = removeReaction(ctx, db, "m1", "b", "🎉"); err != nil || changed || db.seq != 4 {
		t.Errorf("remove of one never added changed=%v err=%v seq=%d, want a no-op", changed, err, db.seq)
	}

	// Not a party, not a message, and a tombstone.
	db.seed("gone", "a", "b", "bye")
	db.find("gone").DeletedAt = &db.clock
	for _, c := range []struct{ id, user string }{{"m1", "c"}, {"nope", "a"}, {"gone", "a"}} {
		if _, _, err := addReaction(ctx, db, c.id, c.user, "👍"); !errors.Is(err, errNoMessage) {
			t.Errorf("add %s by %s = %v, want errNoMessage", c.id, c.user, err)
		}
		if _, _, err := removeReaction(ctx, db, c.id, c.user, "👍"); !errors.Is(err, errNoMessage) {
			t.Errorf("remove %s by %s = %v, want errNoMessage", c.id, c.user, err)
		}
	}
}

// Both caps, at their boundary: the 20th distinct emoji and the 50th reaction go in,
// the next one does not, and a refusal writes nothing.
func TestAddReactionStopsAtBothCapsButAcceptsARepeat(t *testing.T) {
	ctx := context.Background()

	db := newFakeDB("a", "b")
	db.seed("m1", "a", "b", "hi")
	emoji := make([]string, maxReactionEmoji+1)
	for i := range emoji {
		emoji[i] = string(rune(0x1F600 + i))
	}
	for _, em := range emoji[:maxReactionEmoji-1] {
		db.react("m1", "a", em)
	}
	if _, changed, err := addReaction(ctx, db, "m1", "b", emoji[maxReactionEmoji-1]); err != nil || !changed {
		t.Fatalf("emoji number %d: changed=%v err=%v, want it in", maxReactionEmoji, changed, err)
	}
	seq, rows := db.seq, len(db.reactions)
	if _, _, err := addReaction(ctx, db, "m1", "b", emoji[maxReactionEmoji]); !errors.Is(err, errReactionLimit) {
		t.Errorf("emoji number %d = %v, want errReactionLimit", maxReactionEmoji+1, err)
	}
	if db.seq != seq || len(db.reactions) != rows {
		t.Error("a refused reaction wrote something")
	}
	if _, changed, err := addReaction(ctx, db, "m1", "b", emoji[0]); err != nil || !changed {
		t.Errorf("a known emoji at the distinct cap changed=%v err=%v, want it in", changed, err)
	}
	if _, changed, err := addReaction(ctx, db, "m1", "b", emoji[maxReactionEmoji-1]); err != nil || changed {
		t.Errorf("a repeat at the distinct cap changed=%v err=%v, want a quiet success", changed, err)
	}

	db = newFakeDB("a", "b")
	db.seed("m1", "a", "b", "hi")
	for i := range maxReactions - 1 {
		db.react("m1", "fan"+strconv.Itoa(i), "👍")
	}
	if _, changed, err := addReaction(ctx, db, "m1", "a", "👍"); err != nil || !changed {
		t.Fatalf("reaction number %d: changed=%v err=%v, want it in", maxReactions, changed, err)
	}
	seq = db.seq
	for _, em := range []string{"👍", "❤️"} {
		if _, _, err := addReaction(ctx, db, "m1", "b", em); !errors.Is(err, errReactionLimit) {
			t.Errorf("reaction number %d (%s) = %v, want errReactionLimit", maxReactions+1, em, err)
		}
	}
	if db.seq != seq || len(db.reactions) != maxReactions {
		t.Error("a refused reaction wrote something")
	}
	if _, changed, err := addReaction(ctx, db, "m1", "a", "👍"); err != nil || changed {
		t.Errorf("a repeat at the total cap changed=%v err=%v, want a quiet success", changed, err)
	}
}

func TestReactionWritesSurfaceDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	for _, sql := range []string{participantMessageSQL, reactionStateSQL, insertReactionSQL, touchMessageSQL} {
		db := newFakeDB("a", "b")
		db.seed("m1", "a", "b", "hi")
		db.failOnQuery = sql
		_, _, err := addReaction(ctx, db, "m1", "a", "👍")
		if err == nil || errors.Is(err, errNoMessage) || errors.Is(err, errReactionLimit) {
			t.Errorf("a failing %.30q on add gave %v, want a plain error", strings.Join(strings.Fields(sql), " "), err)
		}
	}
	for _, sql := range []string{participantMessageSQL, reactionStateSQL, removeReactionSQL, touchMessageSQL} {
		db := newFakeDB("a", "b")
		db.seed("m1", "a", "b", "hi")
		db.react("m1", "a", "👍")
		db.failOnQuery = sql
		_, _, err := removeReaction(ctx, db, "m1", "a", "👍")
		if err == nil || errors.Is(err, errNoMessage) {
			t.Errorf("a failing %.30q on remove gave %v, want a plain error", strings.Join(strings.Fields(sql), " "), err)
		}
	}
}

// touchMessage draws a seq without changing the message, and a tombstone has none to draw.
func TestTouchMessageOnlyDrawsASeqAndNeverOnATombstone(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB("a", "b")
	m := db.seed("m1", "a", "b", "words")

	got, err := touchMessage(ctx, db, "m1")
	if err != nil || got.Seq != m.Seq+1 || got.Body != "words" || got.EditedAt != nil || !got.CreatedAt.Equal(m.CreatedAt) {
		t.Errorf("touch = %+v err=%v, want the same message with seq %d", got, err, m.Seq+1)
	}
	db.find("m1").DeletedAt = &db.clock
	if _, err := touchMessage(ctx, db, "m1"); !errors.Is(err, errNoMessage) {
		t.Errorf("touching a tombstone = %v, want errNoMessage", err)
	}
	if _, err := touchMessage(ctx, db, "nope"); !errors.Is(err, errNoMessage) {
		t.Errorf("touching nothing = %v, want errNoMessage", err)
	}
}

// searchMessages hands the database NULL, not "" or a zero time, for what was not
// given: the SQL tests IS NULL.
func TestSearchMessagesPassesNullForWhatWasNotGiven(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB("a", "b")
	db.befriend("a", "b")
	db.seed("m1", "a", "b", "needle in a haystack")

	got, err := searchMessages(ctx, db, "a", "needle", "", 10, nil)
	if err != nil || len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("search = %v err=%v", idsOf(got), err)
	}
	args := db.callsTo(searchMessagesSQL)[0].args
	if args[2] != nil || args[3] != nil || args[4] != nil {
		t.Errorf("with, at, id = %v %v %v, want all NULL", args[2], args[3], args[4])
	}

	before := &cursor{CreatedAt: db.find("m1").CreatedAt, ID: "m1"}
	got, err = searchMessages(ctx, db, "a", "needle", "b", 10, before)
	if err != nil || len(got) != 0 {
		t.Fatalf("search before m1 = %v err=%v, want nothing older", idsOf(got), err)
	}
	args = db.callsTo(searchMessagesSQL)[1].args
	if args[2] != "b" || args[3] != before.CreatedAt || args[4] != "m1" || args[5] != 10 {
		t.Errorf("args = %v, want with b, the cursor's time and id, and the limit", args)
	}

	db.failOnQuery = searchMessagesSQL
	if _, err := searchMessages(ctx, db, "a", "needle", "", 10, nil); err == nil {
		t.Error("a failing search returned no error")
	}
}

// The two halves of an around page: the pair, live rows only, the anchor's (created_at,
// id) as the keyset — the older half INCLUDING it, the newer strictly after — and never
// seq. Each is one read with its own limit.
func TestSQLAroundReadsTwoKeysetHalvesOnCreationAndID(t *testing.T) {
	mustContain(t, "listAroundOlderSQL", listAroundOlderSQL,
		"WHERE "+conversationPairSQLText(),
		"AND deleted_at IS NULL",
		"AND (created_at, id) <= ($3, $4)",
		"ORDER BY created_at DESC, id DESC",
		"LIMIT $5",
	)
	mustContain(t, "listAroundNewerSQL", listAroundNewerSQL,
		"WHERE "+conversationPairSQLText(),
		"AND deleted_at IS NULL",
		"AND (created_at, id) > ($3, $4)",
		"ORDER BY created_at ASC, id ASC",
		"LIMIT $5",
	)
	for name, sql := range map[string]string{"listAroundOlderSQL": listAroundOlderSQL, "listAroundNewerSQL": listAroundNewerSQL} {
		_, filter, _ := strings.Cut(sql, "FROM messages") // after the column list, which has seq in it
		mustNotContain(t, name, filter, "seq", "OFFSET", "UNION")
	}
}
