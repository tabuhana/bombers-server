package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// ---- harness ----------------------------------------------------------------

// push is one call to the Notify func.
type push struct {
	users []string
	kind  string
	data  any
}

// listener stands in for the notify hub: it records who was told what.
type listener struct{ pushes []push }

func (l *listener) notify(users []string, kind string, data any) {
	l.pushes = append(l.pushes, push{users: append([]string(nil), users...), kind: kind, data: data})
}

// asJSON is a pushed value as the JSON a client would read.
func asJSON(t *testing.T, data any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// frames returns the frames one write pushed, keyed by the person each was sent to.
// A write is pushed as one frame per person and not one shared by both, because
// reactions' `me` and the mentions reported are relative to the reader.
func (l *listener) frames(t *testing.T) map[string]map[string]any {
	t.Helper()
	if len(l.pushes) != 2 {
		t.Fatalf("pushes = %d, want exactly 2, one per person: %+v", len(l.pushes), l.pushes)
	}
	out := map[string]map[string]any{}
	for _, p := range l.pushes {
		if len(p.users) != 1 || p.kind != l.pushes[0].kind {
			t.Fatalf("push = %+v, want one person and one kind per frame", p)
		}
		out[p.users[0]] = asJSON(t, p.data)
	}
	if len(out) != 2 {
		t.Fatalf("both frames went to the same person: %+v", l.pushes)
	}
	return out
}

// one returns the single push this test expects, for the kinds that go to one
// person only (typing and read).
func (l *listener) one(t *testing.T) (push, map[string]any) {
	t.Helper()
	if len(l.pushes) != 1 {
		t.Fatalf("pushes = %d, want exactly 1: %+v", len(l.pushes), l.pushes)
	}
	return l.pushes[0], asJSON(t, l.pushes[0].data)
}

// frame returns the push this test expects as ONE push to both people, which is what
// the two frames of a write amount to: the people between them, and the frame the
// first of them got. The frames differ only where the message is relative to the
// reader; frames() is for the tests that look at that.
func (l *listener) frame(t *testing.T) (push, map[string]any) {
	t.Helper()
	by := l.frames(t)
	var users []string
	for u := range by {
		users = append(users, u)
	}
	first := l.pushes[0]
	return push{users: users, kind: first.kind, data: first.data}, by[first.users[0]]
}

func sorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// env is a fake database and a listener behind a router that mounts the handlers
// exactly as main.go does. Registering them here is also what proves chi accepts
// `GET /messages`, `GET /messages/{userID}` and `PATCH|DELETE /messages/{messageID}`
// side by side.
type env struct {
	db *fakeDB
	ln *listener
	h  *Handler
}

func newEnv(users ...string) *env {
	db := newFakeDB(users...)
	ln := &listener{}
	return &env{db: db, ln: ln, h: newHandler(db, db.begin, ln.notify)}
}

func (e *env) router(userID string) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), userID)))
		})
	})
	r.Post("/messages", e.h.Send)
	r.Patch("/messages/{messageID}", e.h.Edit)
	r.Delete("/messages/{messageID}", e.h.Delete)
	r.Get("/messages", e.h.Updates)
	r.Get("/messages/unread", e.h.Unread)
	r.Get("/messages/{userID}", e.h.History)
	r.Get("/messages/{userID}/pins", e.h.Pins)
	r.Put("/messages/{messageID}/pin", e.h.Pin)
	r.Delete("/messages/{messageID}/pin", e.h.Unpin)
	r.Put("/messages/{messageID}/reactions", e.h.AddReaction)
	r.Delete("/messages/{messageID}/reactions/{emoji}", e.h.RemoveReaction)
	r.Get("/messages/search", e.h.Search)
	r.Post("/messages/{userID}/read", e.h.MarkRead)
	r.Post("/messages/{userID}/typing", e.h.Typing)
	r.Get("/conversations", e.h.Conversations)
	return r
}

func (e *env) do(userID, method, target, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rd)
	rec := httptest.NewRecorder()
	e.router(userID).ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the expected JSON: %q (%v)", rec.Body.String(), err)
	}
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct {
		Error string `json:"error"`
	}](t, rec).Error
}

func jsonBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type page struct {
	Messages []messageResponse `json:"messages"`
	Cursor   int64             `json:"cursor"`
	HasMore  bool              `json:"has_more"`
}

func msgIDs(ms []messageResponse) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// ---- the clamps -------------------------------------------------------------

func TestParseSince(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"412", 412},
		{"9223372036854775807", 9223372036854775807},
		{"-1", 0},
		{"-412", 0},
		{"abc", 0},
		{"1.5", 0},
		{" 7", 0},
		{"0x10", 0},
		{"9223372036854775808", 0}, // one past int64: unparseable, not wrapped
	}
	for _, tc := range cases {
		if got := parseSince(tc.raw); got != tc.want {
			t.Errorf("parseSince(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		raw      string
		def, max int
		want     int
		name     string
	}{
		{"", 200, 500, 200, "missing"},
		{"abc", 200, 500, 200, "unparseable"},
		{"0", 200, 500, 200, "zero"},
		{"-5", 200, 500, 200, "negative"},
		{"1", 200, 500, 1, "smallest"},
		{"350", 200, 500, 350, "in range"},
		{"500", 200, 500, 500, "at the max"},
		{"501", 200, 500, 500, "past the max"},
		{"99999999999", 200, 500, 200, "overflow is unparseable"},
	}
	for _, tc := range cases {
		if got := clampLimit(tc.raw, tc.def, tc.max); got != tc.want {
			t.Errorf("clampLimit(%q) [%s] = %d, want %d", tc.raw, tc.name, got, tc.want)
		}
	}

	// History keeps its own numbers.
	if got := parseLimit(""); got != 100 {
		t.Errorf("history default = %d, want 100", got)
	}
	if got := parseLimit("9999"); got != 200 {
		t.Errorf("history max = %d, want 200", got)
	}
}

func TestCleanBody(t *testing.T) {
	long := strings.Repeat("é", maxBody) // multi-byte: counted in characters, not bytes
	cases := []struct {
		raw      string
		wantBody string
		wantCode string
	}{
		{"  hello  ", "hello", ""},
		{"", "", errEmptyBody},
		{" \n\t ", "", errEmptyBody},
		{long, long, ""},
		{long + "x", "", errBodyTooLong},
	}
	for _, tc := range cases {
		body, code := cleanBody(tc.raw)
		if body != tc.wantBody || code != tc.wantCode {
			t.Errorf("cleanBody(%.12q) = (%.12q, %q), want (%.12q, %q)", tc.raw, body, code, tc.wantBody, tc.wantCode)
		}
	}
}

// ---- send, the nonce, and the frame ----------------------------------------

func TestSendEchoesTheNonceAndPushesTheMessageToBothSides(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{
		"recipient_id": "u2", "body": "  hello  ", "nonce": "  abc123  ",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	// The 201 is the message, flat, plus the nonce trimmed.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"body", "created_at", "deleted_at", "edited_at", "id", "kind", "mentions", "nonce", "pinned_at", "reactions", "recipient_id", "reply_to", "sender_id", "seq"}
	var gotKeys []string
	for k := range raw {
		gotKeys = append(gotKeys, k)
	}
	if sort.Strings(gotKeys); !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("response keys = %v, want %v", gotKeys, wantKeys)
	}
	if raw["nonce"] != "abc123" || raw["body"] != "hello" || raw["sender_id"] != "u1" || raw["recipient_id"] != "u2" {
		t.Errorf("response = %v", raw)
	}
	// Present as null, never omitted.
	if v, ok := raw["edited_at"]; !ok || v != nil {
		t.Errorf("edited_at = %v (present: %v), want present and null", v, ok)
	}
	if v, ok := raw["deleted_at"]; !ok || v != nil {
		t.Errorf("deleted_at = %v (present: %v), want present and null", v, ok)
	}

	// One push, to both people, carrying the message and the same nonce.
	p, frame := e.ln.frame(t)
	if p.kind != "dm" || !reflect.DeepEqual(sorted(p.users), []string{"u1", "u2"}) {
		t.Errorf("push = kind %q to %v, want dm to both u1 and u2", p.kind, p.users)
	}
	if frame["op"] != "new" || frame["from"] != "u1" || frame["nonce"] != "abc123" {
		t.Errorf("frame = %v", frame)
	}
	msg, _ := frame["message"].(map[string]any)
	if msg["id"] != raw["id"] || msg["body"] != "hello" || msg["seq"] != raw["seq"] {
		t.Errorf("frame message = %v, want the stored message %v", msg, raw)
	}
	if _, has := msg["nonce"]; has {
		t.Error("the nonce belongs on the frame, not inside the message")
	}
}

// The nonce is the sender's idempotency key, so it is stored with the message —
// trimmed, as it is echoed — and only the insert gets it.
func TestTheNonceIsStoredWithTheMessage(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{
		"recipient_id": "u2", "body": "hi", "nonce": "  n-1  ",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}

	inserts := e.db.callsTo(insertMessageSQL)
	if len(inserts) != 1 || len(inserts[0].args) != 8 || inserts[0].args[4] != "n-1" {
		t.Fatalf("inserts = %+v, want one with the trimmed nonce as its fifth argument", inserts)
	}
	if len(e.db.msgs) != 1 || e.db.msgs[0].Nonce != "n-1" {
		t.Errorf("stored = %+v, want the nonce kept on the row", e.db.msgs)
	}
}

func sendWithNonce(t *testing.T, e *env, sender, recipient, body, nonce string) *httptest.ResponseRecorder {
	t.Helper()
	fields := map[string]any{"recipient_id": recipient, "body": body}
	if nonce != "" {
		fields["nonce"] = nonce
	}
	return e.do(sender, http.MethodPost, "/messages", jsonBody(t, fields))
}

// A retry whose first attempt landed (only the response was lost) gets the
// ORIGINAL message back: same id, seq and created_at, 200 rather than 201, the
// nonce still echoed, nothing new stored, no seq drawn and nobody told again.
func TestARepeatSendReturnsTheOriginalMessageAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	first := sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d (%s)", first.Code, first.Body)
	}
	orig := decode[sendResponse](t, first)
	if orig.Nonce != "n-1" {
		t.Errorf("first nonce = %q, want n-1", orig.Nonce)
	}
	e.ln.pushes = nil
	seqBefore := e.db.seq

	// The retry carries the same nonce; its body is whatever the client re-sends.
	again := sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	if again.Code != http.StatusOK {
		t.Fatalf("repeat status = %d, want 200 (%s)", again.Code, again.Body)
	}
	got := decode[sendResponse](t, again)
	if got.ID != orig.ID || got.Seq != orig.Seq || !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("repeat = %+v, want the original %+v", got, orig)
	}
	if got.Nonce != "n-1" {
		t.Errorf("repeat nonce = %q, want it echoed", got.Nonce)
	}

	if len(e.ln.pushes) != 0 {
		t.Errorf("a repeat pushed %d frames: %+v", len(e.ln.pushes), e.ln.pushes)
	}
	if len(e.db.msgs) != 1 {
		t.Errorf("rows = %d, want 1: a repeat inserted a second message", len(e.db.msgs))
	}
	if n := len(e.db.callsTo(insertMessageSQL)); n != 1 {
		t.Errorf("insert statements = %d, want 1: the repeat must not draw a seq", n)
	}
	if e.db.seq != seqBefore {
		t.Errorf("seq moved %d -> %d on a repeat", seqBefore, e.db.seq)
	}
	if e.db.commits != 2 {
		t.Errorf("commits = %d, want both sends to commit their (lock-only) transaction", e.db.commits)
	}
}

// The repeat takes the seq lock like any other write, so a retry that races the
// original in another request waits for it and then sees it.
func TestARepeatSendStillRunsInsideTheSeqLock(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	e.db.calls = nil

	if rec := sendWithNonce(t, e, "u1", "u2", "hello", "n-1"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var stmts []string
	for _, c := range e.db.calls {
		if c.sql == lockSeqSQL || c.sql == findByNonceSQL {
			stmts = append(stmts, c.sql)
			if !c.inTx {
				t.Errorf("%q ran outside the transaction", c.sql)
			}
		}
	}
	if !reflect.DeepEqual(stmts, []string{lockSeqSQL, findByNonceSQL}) {
		t.Errorf("statements = %v, want the lock then the lookup", stmts)
	}
}

// Without a nonce there is nothing to be idempotent on: two identical sends,
// deliberately made, are two messages, two frames and two 201s.
func TestTwoSendsWithoutANonceAreTwoMessages(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	for i := range 2 {
		if rec := sendWithNonce(t, e, "u1", "u2", "same", ""); rec.Code != http.StatusCreated {
			t.Fatalf("send %d status = %d (%s)", i, rec.Code, rec.Body)
		}
	}
	if len(e.db.msgs) != 2 || e.db.msgs[0].ID == e.db.msgs[1].ID {
		t.Errorf("stored = %+v, want two distinct rows", e.db.msgs)
	}
	if len(e.ln.pushes) != 4 {
		t.Errorf("pushes = %d, want 4: a frame to each person, for each of two messages", len(e.ln.pushes))
	}
	if n := len(e.db.callsTo(findByNonceSQL)); n != 0 {
		t.Errorf("lookups = %d, want 0 without a nonce", n)
	}
}

// The same nonce from a DIFFERENT sender is a different message, not a collision
// and not a way to read someone else's.
func TestTheSameNonceFromAnotherSenderIsASeparateMessage(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	a := sendWithNonce(t, e, "u1", "u2", "from one", "shared")
	b := sendWithNonce(t, e, "u2", "u1", "from two", "shared")
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("statuses = %d and %d, want 201 and 201 (%s)", a.Code, b.Code, b.Body)
	}
	ma, mb := decode[sendResponse](t, a), decode[sendResponse](t, b)
	if ma.ID == mb.ID || mb.Body != "from two" || mb.SenderID != "u2" {
		t.Errorf("messages = %+v and %+v, want two separate ones", ma, mb)
	}
	if mb.Nonce != "shared" {
		t.Errorf("nonce = %q, want it echoed", mb.Nonce)
	}
	if len(e.db.msgs) != 2 || len(e.ln.pushes) != 4 {
		t.Errorf("rows = %d, pushes = %d, want 2 and 4", len(e.db.msgs), len(e.ln.pushes))
	}
}

// The same nonce from the same sender to a DIFFERENT recipient is a separate
// message: delivered, stored, pushed to that recipient, 201. Answering it with the
// first recipient's message would be a 200 for a message nobody received.
func TestTheSameNonceToAnotherRecipientIsASeparateMessage(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")

	toAlice := sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	e.ln.pushes = nil
	toBob := sendWithNonce(t, e, "u1", "u3", "hello", "n-1")
	if toAlice.Code != http.StatusCreated || toBob.Code != http.StatusCreated {
		t.Fatalf("statuses = %d and %d, want 201 and 201 (%s)", toAlice.Code, toBob.Code, toBob.Body)
	}
	a, b := decode[sendResponse](t, toAlice), decode[sendResponse](t, toBob)
	if a.ID == b.ID || b.RecipientID != "u3" || b.SenderID != "u1" {
		t.Errorf("messages = %+v and %+v, want a separate one addressed to u3", a, b)
	}
	if b.Nonce != "n-1" {
		t.Errorf("nonce = %q, want it echoed", b.Nonce)
	}
	if len(e.db.msgs) != 2 {
		t.Errorf("rows = %d, want 2", len(e.db.msgs))
	}
	p, frame := e.ln.frame(t)
	if !reflect.DeepEqual(sorted(p.users), []string{"u1", "u3"}) || frame["op"] != "new" || frame["nonce"] != "n-1" {
		t.Errorf("push = %v %v, want a new-message frame to u1 and u3", p.users, frame)
	}

	// And each conversation's own retry still lands on its own message.
	e.ln.pushes = nil
	again := sendWithNonce(t, e, "u1", "u3", "hello", "n-1")
	if again.Code != http.StatusOK || decode[sendResponse](t, again).ID != b.ID {
		t.Errorf("retry to u3 = %d %s, want 200 with u3's message %s", again.Code, again.Body, b.ID)
	}
	if len(e.ln.pushes) != 0 || len(e.db.msgs) != 2 {
		t.Errorf("pushes=%d rows=%d, want 0 and 2", len(e.ln.pushes), len(e.db.msgs))
	}
}

// A different nonce from the same sender is a new message too.
func TestADifferentNonceFromTheSameSenderIsANewMessage(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	sendWithNonce(t, e, "u1", "u2", "same", "n-1")
	if rec := sendWithNonce(t, e, "u1", "u2", "same", "n-2"); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.db.msgs) != 2 {
		t.Errorf("rows = %d, want 2", len(e.db.msgs))
	}
}

// A repeat is still a send: the recipient must be a friend. A nonce is not a way
// past the gate to learn that a message exists.
func TestARepeatSendIsStillFriendGated(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	delete(e.db.friends, pair("u1", "u2"))

	rec := sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
		t.Errorf("got %d %s, want the opaque 404", rec.Code, rec.Body)
	}
}

// A lookup that fails is a 500 that stores and pushes nothing — never a fall
// through to an insert that could duplicate the message.
func TestAFailedNonceLookupStoresAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.failOnQuery = findByNonceSQL

	rec := sendWithNonce(t, e, "u1", "u2", "hello", "n-1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 || e.db.commits != 0 {
		t.Errorf("rows=%d pushes=%d commits=%d, want none", len(e.db.msgs), len(e.ln.pushes), e.db.commits)
	}
}

func TestSendWithoutANonceLeavesItOut(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u2", "body": "hi"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "nonce") {
		t.Errorf("response mentions a nonce nobody sent: %s", rec.Body)
	}
	_, frame := e.ln.frame(t)
	if _, has := frame["nonce"]; has {
		t.Errorf("frame has a nonce nobody sent: %v", frame)
	}
}

func TestNonceLength(t *testing.T) {
	cases := []struct {
		name   string
		nonce  string
		status int
	}{
		{"64 characters", strings.Repeat("n", 64), http.StatusCreated},
		{"64 multi-byte characters", strings.Repeat("é", 64), http.StatusCreated},
		{"padding does not count", "  " + strings.Repeat("n", 64) + "  ", http.StatusCreated},
		{"65 characters", strings.Repeat("n", 65), http.StatusBadRequest},
		{"65 multi-byte characters", strings.Repeat("é", 65), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv("u1", "u2")
			e.db.befriend("u1", "u2")

			rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{
				"recipient_id": "u2", "body": "hi", "nonce": tc.nonce,
			}))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
			if tc.status == http.StatusBadRequest {
				if got := errorCode(t, rec); got != "nonce_too_long" {
					t.Errorf("error = %q, want nonce_too_long", got)
				}
				if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
					t.Error("a refused send stored or pushed something")
				}
			}
		})
	}
}

func TestSendRefusals(t *testing.T) {
	cases := []struct {
		name   string
		sender string
		body   map[string]any
		status int
		code   string
	}{
		{"empty body", "u1", map[string]any{"recipient_id": "u2", "body": "   "}, 400, "empty_body"},
		{"body too long", "u1", map[string]any{"recipient_id": "u2", "body": strings.Repeat("x", maxBody+1)}, 400, "body_too_long"},
		{"not a friend", "u1", map[string]any{"recipient_id": "u3", "body": "hi"}, 404, "recipient_not_found"},
		{"no such user", "u1", map[string]any{"recipient_id": "ghost", "body": "hi"}, 404, "recipient_not_found"},
		{"yourself", "u1", map[string]any{"recipient_id": "u1", "body": "hi"}, 404, "recipient_not_found"},
		{"nobody", "u1", map[string]any{"body": "hi"}, 404, "recipient_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv("u1", "u2", "u3")
			e.db.befriend("u1", "u2")

			rec := e.do(tc.sender, http.MethodPost, "/messages", jsonBody(t, tc.body))
			if rec.Code != tc.status || errorCode(t, rec) != tc.code {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
				t.Error("a refused send stored or pushed something")
			}
		})
	}
}

// Nil is the documented "nobody is listening", and the console and every other
// test builds handlers that way.
func TestSendWithNoListenerStillStores(t *testing.T) {
	db := newFakeDB("u1", "u2")
	db.befriend("u1", "u2")
	e := &env{db: db, ln: &listener{}, h: newHandler(db, db.begin, nil)}

	rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u2", "body": "hi"}))
	if rec.Code != http.StatusCreated || len(db.msgs) != 1 {
		t.Errorf("status = %d, stored = %d", rec.Code, len(db.msgs))
	}
}

// Every write draws its seq inside one transaction that took the lock first and
// committed after, and a write that fails pushes nothing: a frame for a message
// that did not save would show someone something that is not there.
func TestWritesRunInsideTheSeqLockAndPushOnlyAfterTheyLand(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")

	writes := []struct {
		name   string
		sql    string
		do     func() *httptest.ResponseRecorder
		status int
	}{
		{"send", insertMessageSQL, func() *httptest.ResponseRecorder {
			return e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u2", "body": "x"}))
		}, http.StatusCreated},
		{"edit", editMessageSQL, func() *httptest.ResponseRecorder {
			return e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "y"}))
		}, http.StatusOK},
		{"delete", deleteMessageSQL, func() *httptest.ResponseRecorder {
			return e.do("u1", http.MethodDelete, "/messages/m1", "")
		}, http.StatusNoContent},
	}

	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			// Succeeds: lock, write, commit — in that order, all in the transaction.
			e.db.calls, e.db.commits, e.db.rollbacks, e.ln.pushes = nil, 0, 0, nil
			if rec := w.do(); rec.Code != w.status {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}
			var stmts []string
			for _, c := range e.db.calls {
				if c.sql == lockSeqSQL || c.sql == w.sql {
					stmts = append(stmts, c.sql)
					if !c.inTx {
						t.Errorf("%q ran outside the transaction", c.sql)
					}
				}
			}
			if !reflect.DeepEqual(stmts, []string{lockSeqSQL, w.sql}) {
				t.Errorf("statements = %v, want the lock then the write", stmts)
			}
			if e.db.commits != 1 || len(e.ln.pushes) != 2 {
				t.Errorf("commits=%d pushes=%d, want 1 and 2 (a frame to each person)", e.db.commits, len(e.ln.pushes))
			}

			// Fails: 500, nothing committed, nothing pushed.
			e.db.calls, e.db.commits, e.db.rollbacks, e.ln.pushes = nil, 0, 0, nil
			e.db.failOnQuery = w.sql
			// Re-arm what the successful run consumed so the write is reachable.
			if m := e.db.find("m1"); m != nil {
				m.DeletedAt, m.Body = nil, "hi"
			}
			rec := w.do()
			e.db.failOnQuery = ""
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("a failing write answered %d, want 500", rec.Code)
			}
			if e.db.commits != 0 || len(e.ln.pushes) != 0 {
				t.Errorf("a failed write committed %d and pushed %d", e.db.commits, len(e.ln.pushes))
			}
		})
	}
}

// ---- edit -------------------------------------------------------------------

func TestEditChangesTheBodyKeepsThePlaceAndTellsBothSides(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	orig := e.db.seed("m1", "u1", "u2", "teh")
	e.db.seed("m2", "u2", "u1", "later")

	rec := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "  the  "}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if got.Body != "the" || got.EditedAt == nil || got.DeletedAt != nil {
		t.Errorf("edited = %+v", got)
	}
	if got.Seq <= 2 {
		t.Errorf("seq = %d, want a fresh draw past both earlier messages", got.Seq)
	}
	if got.ID != "m1" || !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Error("an edit changed the message's id or created_at")
	}

	p, frame := e.ln.frame(t)
	if p.kind != "dm" || !reflect.DeepEqual(sorted(p.users), []string{"u1", "u2"}) {
		t.Errorf("push = %q to %v, want dm to both", p.kind, p.users)
	}
	if frame["op"] != "edit" || frame["from"] != "u1" {
		t.Errorf("frame = %v", frame)
	}
	if _, has := frame["nonce"]; has {
		t.Error("only a new message carries a nonce")
	}
	if msg := frame["message"].(map[string]any); msg["body"] != "the" || msg["edited_at"] == nil {
		t.Errorf("frame message = %v", msg)
	}
}

// An edit adds words to a thread, so it needs the friendship that sending needed.
// Once the recipient has unfriended or blocked the sender, the edit is refused with
// the very same 404 a stranger and a missing message get, byte for byte, and
// nothing is written or pushed — no dm frame reaches the person who walked away.
func TestANonFriendCannotEditAndGetsTheSameAnswerAsAnyoneElse(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "before the falling out")
	delete(e.db.friends, pair("u1", "u2")) // u2 unfriended or blocked u1

	edit := func(user, id string) *httptest.ResponseRecorder {
		return e.do(user, http.MethodPatch, "/messages/"+id, jsonBody(t, map[string]any{"body": "new words"}))
	}
	refusals := map[string]*httptest.ResponseRecorder{
		"the sender, no longer a friend": edit("u1", "m1"),
		"the recipient":                  edit("u2", "m1"),
		"a stranger":                     edit("u3", "m1"),
		"a missing message":              edit("u1", "nope"),
	}
	var want string
	for name, rec := range refusals {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s: got %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
		if want == "" {
			want = rec.Body.String()
		}
		if rec.Body.String() != want {
			t.Errorf("%s answers %q, others answer %q: the refusals must be byte-identical", name, rec.Body.String(), want)
		}
		if ct := rec.Header().Get("Content-Type"); ct != refusals["a stranger"].Header().Get("Content-Type") {
			t.Errorf("%s: Content-Type %q differs from the stranger's", name, ct)
		}
	}

	if m := e.db.find("m1"); m.Body != "before the falling out" || m.EditedAt != nil || m.Seq != 1 {
		t.Errorf("a refused edit changed the message: %+v", m)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a refused edit pushed %d frames: %+v", len(e.ln.pushes), e.ln.pushes)
	}
	if n := len(e.db.callsTo(editMessageSQL)); n != 0 {
		t.Errorf("a refused edit reached the UPDATE %d times", n)
	}
	if e.db.commits != 0 {
		t.Errorf("a refused edit opened a write transaction (%d commits)", e.db.commits)
	}
}

// The order that keeps the refusal opaque: the message is looked up as the
// CALLER's own first. A non-sender is turned away there and never reaches the
// friendship check, so nothing about a stranger's thread (who is whose friend) can
// be read off the difference between the answers.
func TestEditLooksUpTheMessageAsTheSendersBeforeAskingAboutFriendship(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "mine")

	e.db.calls = nil
	if rec := e.do("u3", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "x"})); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.db.callsTo(areFriendsSQL)) != 0 || len(e.db.callsTo(userExistsSQL)) != 0 {
		t.Error("a non-sender reached the friendship check")
	}

	e.db.calls = nil
	if rec := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "fixed"})); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var order []string
	for _, c := range e.db.calls {
		switch c.sql {
		case ownMessageRecipientSQL, areFriendsSQL, lockSeqSQL, editMessageSQL:
			order = append(order, c.sql)
		}
	}
	want := []string{ownMessageRecipientSQL, areFriendsSQL, lockSeqSQL, editMessageSQL}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("an edit ran %d statements in the wrong order, want lookup, friend check, lock, update", len(order))
	}
	if got := e.db.callsTo(areFriendsSQL)[0].args; !reflect.DeepEqual(got, []any{"u1", "u2"}) {
		t.Errorf("the friend check was asked about %v, want the sender and the MESSAGE's recipient", got)
	}
}

// A failure while asking is the same opaque refusal for the gate (as it is for
// typing and sending) and a 500 for the lookup; neither writes nor pushes.
func TestEditGateFailuresWriteAndPushNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "keep")
	body := jsonBody(t, map[string]any{"body": "x"})

	e.db.failOnQuery = areFriendsSQL
	if rec := e.do("u1", http.MethodPatch, "/messages/m1", body); rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
		t.Errorf("a failing friend check = %d %s, want the opaque 404", rec.Code, rec.Body)
	}
	e.db.failOnQuery = ownMessageRecipientSQL
	if rec := e.do("u1", http.MethodPatch, "/messages/m1", body); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing lookup = %d, want 500", rec.Code)
	}
	if e.db.find("m1").Body != "keep" || len(e.ln.pushes) != 0 || e.db.commits != 0 {
		t.Error("a failed gate wrote or pushed something")
	}
}

// The recipient, a stranger and a message that never existed all get the same
// answer, byte for byte — so an id cannot be probed for.
func TestOnlyTheSenderMayEditOrDeleteAndEveryoneElseGetsTheSame404(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "mine")

	var bodies []string
	for _, who := range []struct{ name, user, id string }{
		{"the recipient", "u2", "m1"},
		{"a stranger", "u3", "m1"},
		{"a missing message", "u1", "nope"},
	} {
		edit := e.do(who.user, http.MethodPatch, "/messages/"+who.id, jsonBody(t, map[string]any{"body": "hijack"}))
		del := e.do(who.user, http.MethodDelete, "/messages/"+who.id, "")
		for _, rec := range []*httptest.ResponseRecorder{edit, del} {
			if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
				t.Errorf("%s: got %d %s, want 404 message_not_found", who.name, rec.Code, rec.Body)
			}
			bodies = append(bodies, rec.Body.String())
		}
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("responses differ, so one of them leaks: %q vs %q", b, bodies[0])
		}
	}

	if m := e.db.find("m1"); m.Body != "mine" || m.EditedAt != nil || m.DeletedAt != nil || m.Seq != 1 {
		t.Errorf("a refused write changed the message: %+v", m)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a refused write pushed %d frames", len(e.ln.pushes))
	}
}

func TestEditingOrDeletingADeletedMessageIs404(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "bye")

	if rec := e.do("u1", http.MethodDelete, "/messages/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first delete = %d", rec.Code)
	}
	e.ln.pushes = nil

	edit := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "back"}))
	del := e.do("u1", http.MethodDelete, "/messages/m1", "")
	for name, rec := range map[string]*httptest.ResponseRecorder{"edit": edit, "delete": del} {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s of a deleted message = %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
	}
	if m := e.db.find("m1"); m.Body != "" || m.EditedAt != nil {
		t.Errorf("a deleted message was changed: %+v", m)
	}
	if len(e.ln.pushes) != 0 {
		t.Error("a refused write pushed")
	}
}

func TestEditBodyRules(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty", "", "empty_body"},
		{"blank", "   \n", "empty_body"},
		{"too long", strings.Repeat("x", maxBody+1), "body_too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv("u1", "u2")
			e.db.seed("m1", "u1", "u2", "keep")

			rec := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": tc.body}))
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != tc.code {
				t.Errorf("got %d %s, want 400 %s", rec.Code, rec.Body, tc.code)
			}
			if e.db.find("m1").Body != "keep" || len(e.ln.pushes) != 0 {
				t.Error("a refused edit changed or pushed something")
			}
		})
	}

	e := newEnv("u1", "u2")
	e.db.seed("m1", "u1", "u2", "keep")
	if rec := e.do("u1", http.MethodPatch, "/messages/m1", "{not json"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rec.Code)
	}
}

// ---- delete -----------------------------------------------------------------

func TestDeleteClearsTheBodyAndTellsBothSides(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "words that must go")

	rec := e.do("u1", http.MethodDelete, "/messages/m1", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("got %d %q, want an empty 204", rec.Code, rec.Body)
	}

	m := e.db.find("m1")
	if m.Body != "" || m.DeletedAt == nil || m.Seq <= 1 {
		t.Errorf("stored message = %+v, want an empty body, deleted_at set and a fresh seq", m)
	}

	p, frame := e.ln.frame(t)
	if p.kind != "dm" || !reflect.DeepEqual(sorted(p.users), []string{"u1", "u2"}) {
		t.Errorf("push = %q to %v, want dm to both", p.kind, p.users)
	}
	if frame["op"] != "delete" || frame["from"] != "u1" {
		t.Errorf("frame = %v", frame)
	}
	msg := frame["message"].(map[string]any)
	if msg["body"] != "" || msg["deleted_at"] == nil || msg["id"] != "m1" {
		t.Errorf("frame message = %v, want the tombstone", msg)
	}
	if strings.Contains(rec.Body.String()+string(mustJSON(t, frame)), "words that must go") {
		t.Error("the deleted words are still on the wire")
	}
}

// Deleting is deliberately NOT friend-gated: it only removes the sender's own
// words, and its frame carries no body. Someone who was unfriended or blocked can
// still take their words back, and both people are still told.
func TestANonFriendCanStillDeleteTheirOwnMessageAndBothSidesAreTold(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "words I want back")
	delete(e.db.friends, pair("u1", "u2")) // u2 unfriended or blocked u1

	e.db.calls = nil
	rec := e.do("u1", http.MethodDelete, "/messages/m1", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("got %d %q, want an empty 204", rec.Code, rec.Body)
	}
	if m := e.db.find("m1"); m.Body != "" || m.DeletedAt == nil {
		t.Errorf("stored message = %+v, want the tombstone", m)
	}

	p, frame := e.ln.frame(t)
	if p.kind != "dm" || !reflect.DeepEqual(sorted(p.users), []string{"u1", "u2"}) {
		t.Errorf("push = %q to %v, want dm to both, the former friend included", p.kind, p.users)
	}
	msg := frame["message"].(map[string]any)
	if frame["op"] != "delete" || msg["body"] != "" || msg["deleted_at"] == nil {
		t.Errorf("frame = %v, want a body-less delete", frame)
	}
	if strings.Contains(string(mustJSON(t, frame)), "words I want back") {
		t.Error("the deleted words are on the wire")
	}
	if len(e.db.callsTo(areFriendsSQL))+len(e.db.callsTo(userExistsSQL)) != 0 {
		t.Error("delete asked about friendship, which it must never do")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEditAndDeleteNeedASignedInUser(t *testing.T) {
	e := newEnv("u1", "u2")
	for name, call := range map[string]func(w http.ResponseWriter, r *http.Request){
		"edit": e.h.Edit, "delete": e.h.Delete, "updates": e.h.Updates, "send": e.h.Send,
		"conversations": e.h.Conversations, "typing": e.h.Typing, "read": e.h.MarkRead, "history": e.h.History,
		"pin": e.h.Pin, "unpin": e.h.Unpin, "pins": e.h.Pins,
		"react": e.h.AddReaction, "unreact": e.h.RemoveReaction, "search": e.h.Search,
	} {
		rec := httptest.NewRecorder()
		call(rec, httptest.NewRequest(http.MethodGet, "/messages", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no user = %d, want 401", name, rec.Code)
		}
	}
}

// ---- the catch-up feed and history -----------------------------------------

// The scenario both cursors are about: m1 is the oldest message and is EDITED
// last. The catch-up feed must put it last (its seq moved); the conversation must
// leave it first (its created_at did not).
func TestTheFeedFollowsSeqAndTheConversationFollowsCreation(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "first")
	e.db.seed("m2", "u2", "u1", "second")
	e.db.seed("m3", "u1", "u2", "third")
	e.db.seed("x1", "u2", "u3", "not u1's")

	if rec := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "first, fixed"})); rec.Code != 200 {
		t.Fatalf("edit = %d", rec.Code)
	}
	if rec := e.do("u1", http.MethodDelete, "/messages/m3", ""); rec.Code != 204 {
		t.Fatalf("delete = %d", rec.Code)
	}

	feed := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=0", ""))
	if want := []string{"m2", "m1", "m3"}; !reflect.DeepEqual(msgIDs(feed.Messages), want) {
		t.Errorf("feed = %v, want %v (by seq: the edit and the delete moved to the end, someone else's thread absent)", msgIDs(feed.Messages), want)
	}
	last := feed.Messages[len(feed.Messages)-1]
	if last.DeletedAt == nil || last.Body != "" {
		t.Errorf("the feed must carry the tombstone: %+v", last)
	}
	for i := 1; i < len(feed.Messages); i++ {
		if feed.Messages[i].Seq <= feed.Messages[i-1].Seq {
			t.Errorf("feed is not strictly ascending by seq: %v", feed.Messages)
		}
	}
	if feed.Cursor != last.Seq || feed.HasMore {
		t.Errorf("cursor=%d has_more=%v, want %d and false", feed.Cursor, feed.HasMore, last.Seq)
	}

	hist := decode[struct {
		Messages []messageResponse `json:"messages"`
	}](t, e.do("u1", http.MethodGet, "/messages/u2", ""))
	if want := []string{"m1", "m2"}; !reflect.DeepEqual(msgIDs(hist.Messages), want) {
		t.Errorf("history = %v, want %v (by creation, the edited one still first, the tombstone absent)", msgIDs(hist.Messages), want)
	}
	if hist.Messages[0].EditedAt == nil || hist.Messages[0].Body != "first, fixed" {
		t.Errorf("history lost the edit: %+v", hist.Messages[0])
	}
}

func TestCatchUpPagesForwardUntilNothingIsLeft(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	for i := 1; i <= 5; i++ {
		e.db.seed("m"+strconv.Itoa(i), "u1", "u2", "x")
	}

	first := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=0&limit=2", ""))
	if !reflect.DeepEqual(msgIDs(first.Messages), []string{"m1", "m2"}) || first.Cursor != 2 || !first.HasMore {
		t.Fatalf("page 1 = %v cursor %d has_more %v", msgIDs(first.Messages), first.Cursor, first.HasMore)
	}
	second := decode[page](t, e.do("u1", http.MethodGet, "/messages?since="+strconv.FormatInt(first.Cursor, 10)+"&limit=2", ""))
	if !reflect.DeepEqual(msgIDs(second.Messages), []string{"m3", "m4"}) || second.Cursor != 4 || !second.HasMore {
		t.Fatalf("page 2 = %v cursor %d has_more %v", msgIDs(second.Messages), second.Cursor, second.HasMore)
	}
	// A page that fills the limit says there may be more, even when it is exactly
	// the end; the next one comes back short and says no.
	third := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=4&limit=2", ""))
	if !reflect.DeepEqual(msgIDs(third.Messages), []string{"m5"}) || third.Cursor != 5 || third.HasMore {
		t.Fatalf("page 3 = %v cursor %d has_more %v", msgIDs(third.Messages), third.Cursor, third.HasMore)
	}
}

func TestCatchUpPastTheEndKeepsTheRequestedCursor(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "x")

	rec := e.do("u1", http.MethodGet, "/messages?since=99", "")
	if !strings.Contains(rec.Body.String(), `"messages":[]`) {
		t.Errorf("an empty page must be [] and not null: %s", rec.Body)
	}
	got := decode[page](t, rec)
	if got.Cursor != 99 || got.HasMore || len(got.Messages) != 0 {
		t.Errorf("empty page = %+v, want cursor 99, no more", got)
	}
}

func TestCatchUpSinceThatMakesNoSenseMeansFromTheStart(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "x")
	e.db.seed("m2", "u2", "u1", "y")

	for _, q := range []string{"", "?since=-3", "?since=abc", "?since=1.5", "?since=99999999999999999999"} {
		got := decode[page](t, e.do("u1", http.MethodGet, "/messages"+q, ""))
		if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m1", "m2"}) || got.Cursor != 2 {
			t.Errorf("GET /messages%s = %v cursor %d, want everything", q, msgIDs(got.Messages), got.Cursor)
		}
	}
}

func TestCatchUpLimitIsClamped(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "x")

	for _, tc := range []struct {
		q    string
		want int
	}{
		{"", 200}, {"?limit=0", 200}, {"?limit=-4", 200}, {"?limit=abc", 200}, {"?limit=7", 7}, {"?limit=500", 500}, {"?limit=9000", 500},
	} {
		e.db.calls = nil
		e.do("u1", http.MethodGet, "/messages"+tc.q, "")
		calls := e.db.callsTo(listUpdatesSQL)
		if len(calls) != 1 || calls[0].args[2] != tc.want {
			t.Errorf("GET /messages%s asked the database for %v, want limit %d", tc.q, calls, tc.want)
		}
	}
}

// Everyone sees only their own side: the feed is "everything involving me".
func TestCatchUpIsScopedToTheCaller(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u2", "u3")
	e.db.seed("m1", "u1", "u2", "x")
	e.db.seed("m2", "u2", "u3", "y")

	if got := decode[page](t, e.do("u1", http.MethodGet, "/messages", "")); !reflect.DeepEqual(msgIDs(got.Messages), []string{"m1"}) {
		t.Errorf("u1 saw %v", msgIDs(got.Messages))
	}
	if got := decode[page](t, e.do("u3", http.MethodGet, "/messages", "")); !reflect.DeepEqual(msgIDs(got.Messages), []string{"m2"}) {
		t.Errorf("u3 saw %v", msgIDs(got.Messages))
	}
}

// The feed is friends-only like history and the conversation list: asking it from
// since=0 must not rebuild a thread the history endpoint refuses to serve. A
// current friend's thread, tombstones included, is still there.
func TestCatchUpOmitsFormerFriendsButKeepsCurrentFriendsAndTheirTombstones(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "with a friend")
	e.db.seed("x1", "u3", "u1", "from someone who is no longer a friend")
	e.db.seed("x2", "u1", "u3", "to someone who is no longer a friend")
	e.db.seed("m2", "u2", "u1", "from a friend, deleted")
	if rec := e.do("u2", http.MethodDelete, "/messages/m2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	feed := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=0", ""))
	if want := []string{"m1", "m2"}; !reflect.DeepEqual(msgIDs(feed.Messages), want) {
		t.Fatalf("feed = %v, want %v: the former friend's thread absent, the current friend's tombstone present", msgIDs(feed.Messages), want)
	}
	if last := feed.Messages[1]; last.DeletedAt == nil || last.Body != "" {
		t.Errorf("the tombstone from a current friend must be carried: %+v", last)
	}

	// The same thread is refused by history, so the feed was a way around it.
	if rec := e.do("u1", http.MethodGet, "/messages/u3", ""); rec.Code != http.StatusNotFound {
		t.Errorf("history with the former friend = %d, want 404", rec.Code)
	}
	// And from the other side: u3 sees nothing of the thread either.
	if got := decode[page](t, e.do("u3", http.MethodGet, "/messages?since=0", "")); len(got.Messages) != 0 {
		t.Errorf("u3 saw %v, want nothing: they have no friends", msgIDs(got.Messages))
	}
}

// The friends-only filter must not disturb what the feed is: ordered by seq alone
// (an edit moves a message to the end), with has_more true only when a page fills
// and a cursor that walks every page.
func TestFriendsOnlyCatchUpStillOrdersBySeqAndPaginates(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "first")
	e.db.seed("x1", "u3", "u1", "former friend")
	e.db.seed("m2", "u2", "u1", "second")
	e.db.seed("x2", "u1", "u3", "former friend")
	e.db.seed("m3", "u1", "u2", "third")
	if rec := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "first, fixed"})); rec.Code != 200 {
		t.Fatalf("edit = %d", rec.Code)
	}

	first := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=0&limit=2", ""))
	if !reflect.DeepEqual(msgIDs(first.Messages), []string{"m2", "m3"}) || !first.HasMore || first.Cursor != first.Messages[1].Seq {
		t.Fatalf("page 1 = %v cursor %d has_more %v, want [m2 m3], a full page", msgIDs(first.Messages), first.Cursor, first.HasMore)
	}
	second := decode[page](t, e.do("u1", http.MethodGet, "/messages?since="+strconv.FormatInt(first.Cursor, 10)+"&limit=2", ""))
	if !reflect.DeepEqual(msgIDs(second.Messages), []string{"m1"}) || second.HasMore {
		t.Fatalf("page 2 = %v has_more %v, want [m1] (the edit, last by seq) and a short page", msgIDs(second.Messages), second.HasMore)
	}
	if second.Messages[0].EditedAt == nil || second.Cursor != second.Messages[0].Seq {
		t.Errorf("page 2 = %+v", second)
	}
	// A page that fills the limit says there may be more, even at the exact end.
	exact := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=0&limit=3", ""))
	if !exact.HasMore || len(exact.Messages) != 3 {
		t.Errorf("an exactly full page = %d messages has_more %v, want 3 and true", len(exact.Messages), exact.HasMore)
	}
	for i := 1; i < len(exact.Messages); i++ {
		if exact.Messages[i].Seq <= exact.Messages[i-1].Seq {
			t.Errorf("feed is not strictly ascending by seq: %v", exact.Messages)
		}
	}
}

func TestUnreadSummaryIgnoresDeletedMessages(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "one")
	e.db.seed("m2", "u1", "u2", "two")

	type summary struct {
		Conversations []unreadConversation `json:"conversations"`
		Total         int64                `json:"total"`
	}
	if got := decode[summary](t, e.do("u2", http.MethodGet, "/messages/unread", "")); got.Total != 2 {
		t.Fatalf("before: %+v", got)
	}
	e.do("u1", http.MethodDelete, "/messages/m1", "")
	if got := decode[summary](t, e.do("u2", http.MethodGet, "/messages/unread", "")); got.Total != 1 {
		t.Errorf("after one delete: %+v, want 1", got)
	}
	e.do("u1", http.MethodDelete, "/messages/m2", "")
	rec := e.do("u2", http.MethodGet, "/messages/unread", "")
	if got := decode[summary](t, rec); got.Total != 0 || len(got.Conversations) != 0 {
		t.Errorf("after both: %+v, want nothing unread", got)
	}
	if !strings.Contains(rec.Body.String(), `"conversations":[]`) {
		t.Errorf("an empty summary must be [] and not null: %s", rec.Body)
	}
}

// One shape everywhere: these are the only keys a message ever has.
func TestMessageShape(t *testing.T) {
	raw := mustJSON(t, toResponse(&messageRecord{ID: "m1", Seq: 4, SenderID: "a", RecipientID: "b", Body: "hi"}))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"body", "created_at", "deleted_at", "edited_at", "id", "kind", "mentions", "pinned_at", "reactions", "recipient_id", "reply_to", "sender_id", "seq"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("message keys = %v, want %v", keys, want)
	}
	if got["edited_at"] != nil || got["deleted_at"] != nil || got["pinned_at"] != nil || got["reply_to"] != nil {
		t.Errorf("unset fields must be null: %v", got)
	}
	if r, ok := got["reactions"].([]any); !ok || len(r) != 0 {
		t.Errorf("reactions = %v, want []", got["reactions"])
	}
	if m, ok := got["mentions"].([]any); !ok || len(m) != 0 {
		t.Errorf("mentions = %v, want []", got["mentions"])
	}
}

// ---- history pagination -----------------------------------------------------

type historyPage struct {
	Messages []messageResponse `json:"messages"`
	HasMore  bool              `json:"has_more"`
}

// seedThread stores n messages u1<->u2 named m1..mn, alternating sender, oldest
// first.
func seedThread(e *env, n int) {
	for i := 1; i <= n; i++ {
		from, to := "u1", "u2"
		if i%2 == 0 {
			from, to = to, from
		}
		e.db.seed("m"+strconv.Itoa(i), from, to, "x")
	}
}

func TestHistoryLimitIsClamped(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 1)

	for _, tc := range []struct {
		q    string
		want int
	}{
		{"", 100}, {"?limit=0", 100}, {"?limit=-3", 100}, {"?limit=abc", 100}, {"?limit=7", 7}, {"?limit=200", 200}, {"?limit=201", 200}, {"?limit=9000", 200},
	} {
		e.db.calls = nil
		if rec := e.do("u1", http.MethodGet, "/messages/u2"+tc.q, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET /messages/u2%s = %d", tc.q, rec.Code)
		}
		calls := e.db.callsTo(listConversationSQL)
		if len(calls) != 1 || calls[0].args[2] != tc.want {
			t.Errorf("GET /messages/u2%s asked the database for %v, want limit %d", tc.q, calls, tc.want)
		}
	}

	// The same clamp on a page with a cursor.
	e.db.calls = nil
	e.do("u1", http.MethodGet, "/messages/u2?limit=9000&before=m1", "")
	if calls := e.db.callsTo(listConversationBeforeSQL); len(calls) != 1 || calls[0].args[4] != 200 {
		t.Errorf("a paged request asked the database for %v, want limit 200", calls)
	}
}

// Scrolling back: each page is the strictly older messages, ascending inside the
// page, and has_more says whether to ask again.
func TestHistoryPagesBackwardsWithBefore(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 5)

	first := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2?limit=2", ""))
	if !reflect.DeepEqual(msgIDs(first.Messages), []string{"m4", "m5"}) || !first.HasMore {
		t.Fatalf("newest page = %v has_more %v, want [m4 m5] and true", msgIDs(first.Messages), first.HasMore)
	}
	second := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2?limit=2&before="+first.Messages[0].ID, ""))
	if !reflect.DeepEqual(msgIDs(second.Messages), []string{"m2", "m3"}) || !second.HasMore {
		t.Fatalf("page before m4 = %v has_more %v, want [m2 m3] and true", msgIDs(second.Messages), second.HasMore)
	}
	third := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2?limit=2&before="+second.Messages[0].ID, ""))
	if !reflect.DeepEqual(msgIDs(third.Messages), []string{"m1"}) || third.HasMore {
		t.Fatalf("page before m2 = %v has_more %v, want [m1] and false", msgIDs(third.Messages), third.HasMore)
	}
	// Nothing is older than the first message: an empty page, and an array.
	rec := e.do("u1", http.MethodGet, "/messages/u2?limit=2&before=m1", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"messages":[]`) || decode[historyPage](t, rec).HasMore {
		t.Errorf("before the first message = %d %s, want an empty page", rec.Code, rec.Body)
	}
}

// has_more is "the page filled", which is all a page can know without reading
// further: an exact fit says true and the next page comes back empty and false.
func TestHistoryHasMoreIsTrueOnlyWhenThePageFills(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	if got := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2", "")); got.HasMore || len(got.Messages) != 0 {
		t.Errorf("an empty conversation = %+v", got)
	}
	seedThread(e, 3)
	for _, tc := range []struct {
		limit   string
		hasMore bool
	}{{"2", true}, {"3", true}, {"4", false}, {"100", false}} {
		got := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2?limit="+tc.limit, ""))
		if got.HasMore != tc.hasMore {
			t.Errorf("limit %s over 3 messages: has_more = %v, want %v", tc.limit, got.HasMore, tc.hasMore)
		}
	}
}

// A cursor that names nothing the caller can page from is an error, never an
// empty page, and it is the same error whichever way it is wrong.
func TestHistoryBeforeThatNamesNothingIs400(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u2", "u3")
	seedThread(e, 3)
	e.db.seed("other", "u2", "u3", "not u1's conversation")

	var bodies []string
	for _, before := range []string{"ghost", "other", "%20nope"} {
		e.db.calls = nil
		rec := e.do("u1", http.MethodGet, "/messages/u2?before="+before, "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_cursor" {
			t.Errorf("before=%s: got %d %s, want 400 bad_cursor", before, rec.Code, rec.Body)
		}
		if len(e.db.callsTo(listConversationSQL))+len(e.db.callsTo(listConversationBeforeSQL)) != 0 {
			t.Errorf("before=%s: a page was read for a cursor that was refused", before)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("responses differ, so one leaks: %q vs %q", b, bodies[0])
		}
	}

	// An empty ?before= is no cursor at all.
	if rec := e.do("u1", http.MethodGet, "/messages/u2?before=", ""); rec.Code != http.StatusOK {
		t.Errorf("an empty before = %d, want the newest page", rec.Code)
	}
}

// The friend gate answers first, so a cursor can't be used to find out whether a
// message id exists in a conversation the caller has no right to.
func TestHistoryChecksFriendshipBeforeTheCursor(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	seedThread(e, 2)

	rec := e.do("u1", http.MethodGet, "/messages/u3?before=m1", "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
		t.Errorf("got %d %s, want the opaque 404", rec.Code, rec.Body)
	}
	if len(e.db.callsTo(historyCursorSQL)) != 0 {
		t.Error("the cursor was looked up for a conversation the caller may not read")
	}
}

// Tombstones are not in history, and a deleted message is still somewhere to page
// from: the oldest message a client holds may be deleted after it was loaded.
func TestHistoryOmitsTombstonesButAcceptsOneAsACursor(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 5)
	e.do("u1", http.MethodDelete, "/messages/m1", "")
	e.do("u1", http.MethodDelete, "/messages/m3", "")
	e.do("u1", http.MethodDelete, "/messages/m5", "")

	all := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2", ""))
	if !reflect.DeepEqual(msgIDs(all.Messages), []string{"m2", "m4"}) {
		t.Errorf("history = %v, want [m2 m4]: the three tombstones are absent", msgIDs(all.Messages))
	}

	rec := e.do("u1", http.MethodGet, "/messages/u2?before=m3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a deleted message as a cursor = %d %s", rec.Code, rec.Body)
	}
	if got := decode[historyPage](t, rec); !reflect.DeepEqual(msgIDs(got.Messages), []string{"m2"}) {
		t.Errorf("page before the deleted m3 = %v, want [m2] (m1 is a tombstone too)", msgIDs(got.Messages))
	}

	// An edit moves seq but not the place in the conversation.
	e.do("u2", http.MethodPatch, "/messages/m2", jsonBody(t, map[string]any{"body": "fixed"}))
	got := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2?before=m4", ""))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m2"}) || got.Messages[0].Body != "fixed" {
		t.Errorf("page before m4 after editing m2 = %+v", got.Messages)
	}
}

// ---- the conversation list --------------------------------------------------

type conversationsPage struct {
	Conversations []conversationResponse `json:"conversations"`
	HasMore       bool                   `json:"has_more"`
}

func peerIDs(cs []conversationResponse) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.UserID)
	}
	return out
}

func TestConversationsListsOneEntryPerFriendNewestFirst(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4", "u5")
	for _, p := range []string{"u2", "u3", "u4"} {
		e.db.befriend("u1", p)
	}
	e.db.seed("m1", "u2", "u1", "hello from u2")
	e.db.seed("m2", "u1", "u3", "hello to u3")
	e.db.seed("m3", "u4", "u1", "hello from u4")
	e.db.seed("m4", "u5", "u1", "a stranger")
	e.db.seed("m5", "u2", "u1", "u2 again")
	// u1 has read u2 up to between m1 and m5, by a marker from before positions
	// existed (a time and no id); u3 has read u1's side up to m2.
	e.db.reads[[2]string{"u1", "u2"}] = readMark{at: e.db.clock.Add(3 * time.Minute)}
	e.db.reads[[2]string{"u3", "u1"}] = readMark{at: e.db.clock.Add(2 * time.Minute), id: "m2"}

	rec := e.do("u1", http.MethodGet, "/conversations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	got := decode[conversationsPage](t, rec)
	if want := []string{"u2", "u4", "u3"}; !reflect.DeepEqual(peerIDs(got.Conversations), want) {
		t.Fatalf("conversations = %v, want %v: one per friend, by last message, the stranger absent", peerIDs(got.Conversations), want)
	}
	if got.HasMore {
		t.Error("three conversations under a limit of 50 must not say has_more")
	}

	u2, u4, u3 := got.Conversations[0], got.Conversations[1], got.Conversations[2]
	if u2.LastMessage.ID != "m5" || u2.LastMessage.Body != "u2 again" || u2.Unread != 1 {
		t.Errorf("u2 = %+v, want last m5 and 1 unread (m1 is at or before the marker)", u2)
	}
	if u4.LastMessage.ID != "m3" || u4.Unread != 1 {
		t.Errorf("u4 = %+v", u4)
	}
	if u3.LastMessage.ID != "m2" || u3.Unread != 0 {
		t.Errorf("u3 = %+v, want last m2 (sent by the caller) and nothing unread", u3)
	}
	if u2.MyReadAt == nil || !u2.MyReadAt.Equal(e.db.clock.Add(3*time.Minute)) || u2.MyReadID != nil || u2.PeerReadAt != nil || u2.PeerReadID != nil {
		t.Errorf("u2 markers = my (%v, %v) peer (%v, %v), want a timestamp-only marker of mine and none of theirs", u2.MyReadAt, u2.MyReadID, u2.PeerReadAt, u2.PeerReadID)
	}
	if u3.PeerReadAt == nil || !u3.PeerReadAt.Equal(e.db.clock.Add(2*time.Minute)) || u3.PeerReadID == nil || *u3.PeerReadID != "m2" || u3.MyReadAt != nil || u3.MyReadID != nil {
		t.Errorf("u3 markers = my (%v, %v) peer (%v, %v), want their full position and none of mine", u3.MyReadAt, u3.MyReadID, u3.PeerReadAt, u3.PeerReadID)
	}

	// The unread tally here is the same number GET /messages/unread reports.
	type summary struct {
		Conversations []unreadConversation `json:"conversations"`
	}
	for _, c := range decode[summary](t, e.do("u1", http.MethodGet, "/messages/unread", "")).Conversations {
		for _, l := range got.Conversations {
			if l.UserID == c.UserID && l.Unread != c.Count {
				t.Errorf("%s: the list says %d unread, /messages/unread says %d", c.UserID, l.Unread, c.Count)
			}
		}
	}
}

// The wire shape of one entry: exactly these keys, the four marker fields present
// as null when unset.
func TestConversationEntryShape(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "hi")

	var raw struct {
		Conversations []map[string]any `json:"conversations"`
		HasMore       *bool            `json:"has_more"`
	}
	if err := json.Unmarshal(e.do("u1", http.MethodGet, "/conversations", "").Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.HasMore == nil || len(raw.Conversations) != 1 {
		t.Fatalf("response = %+v", raw)
	}
	var keys []string
	for k := range raw.Conversations[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"last_message", "my_read_at", "my_read_id", "peer_read_at", "peer_read_id", "unread", "user_id"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("entry keys = %v, want %v", keys, want)
	}
	for _, k := range []string{"my_read_at", "my_read_id", "peer_read_at", "peer_read_id"} {
		if v, ok := raw.Conversations[0][k]; !ok || v != nil {
			t.Errorf("%s = %v (present: %v), want present and null", k, v, ok)
		}
	}
	if lm, _ := raw.Conversations[0]["last_message"].(map[string]any); lm["id"] != "m1" || lm["seq"] == nil {
		t.Errorf("last_message = %v, want the full message shape", lm)
	}
}

func TestConversationsLeavesOutTombstonesAndDeadPairs(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	for _, p := range []string{"u2", "u3", "u4"} {
		e.db.befriend("u1", p)
	}
	e.db.seed("a1", "u2", "u1", "kept")
	e.db.seed("a2", "u2", "u1", "newest, then deleted")
	e.db.seed("b1", "u1", "u3", "every message of this pair")
	e.db.seed("b2", "u3", "u1", "gets deleted")
	e.db.seed("c1", "u4", "u1", "untouched")
	e.do("u2", http.MethodDelete, "/messages/a2", "")
	e.do("u1", http.MethodDelete, "/messages/b1", "")
	e.do("u3", http.MethodDelete, "/messages/b2", "")

	got := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", ""))
	if want := []string{"u4", "u2"}; !reflect.DeepEqual(peerIDs(got.Conversations), want) {
		t.Fatalf("conversations = %v, want %v: u3's pair is all tombstones and must not appear", peerIDs(got.Conversations), want)
	}
	u2 := got.Conversations[1]
	if u2.LastMessage.ID != "a1" || u2.LastMessage.DeletedAt != nil || u2.Unread != 1 {
		t.Errorf("u2 = %+v, want the newest LIVE message a1 and 1 unread", u2)
	}
}

// Only people the caller is still friends with: a stranger and an ex-friend are
// not listed, however many messages they exchanged.
func TestConversationsIsFriendsOnly(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "friend")
	e.db.seed("m2", "u3", "u1", "not a friend (any more)")

	got := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", ""))
	if !reflect.DeepEqual(peerIDs(got.Conversations), []string{"u2"}) {
		t.Errorf("conversations = %v, want only u2", peerIDs(got.Conversations))
	}
	// And nobody sees someone else's list.
	if other := decode[conversationsPage](t, e.do("u3", http.MethodGet, "/conversations", "")); len(other.Conversations) != 0 {
		t.Errorf("u3 has no friends and saw %v", peerIDs(other.Conversations))
	}
}

func TestConversationsEmptyIsAnArray(t *testing.T) {
	e := newEnv("u1")
	rec := e.do("u1", http.MethodGet, "/conversations", "")
	if !strings.Contains(rec.Body.String(), `"conversations":[]`) || decode[conversationsPage](t, rec).HasMore {
		t.Errorf("an empty list = %s, want [] and no more", rec.Body)
	}
}

func TestConversationsLimitIsClamped(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "x")

	for _, tc := range []struct {
		q    string
		want int
	}{
		{"", 50}, {"?limit=0", 50}, {"?limit=-1", 50}, {"?limit=abc", 50}, {"?limit=1", 1}, {"?limit=120", 120}, {"?limit=200", 200}, {"?limit=201", 200}, {"?limit=99999", 200},
	} {
		e.db.calls = nil
		if rec := e.do("u1", http.MethodGet, "/conversations"+tc.q, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET /conversations%s = %d", tc.q, rec.Code)
		}
		calls := e.db.callsTo(listSummariesSQL)
		if len(calls) != 1 || calls[0].args[3] != tc.want {
			t.Errorf("GET /conversations%s asked the database for %v, want limit %d", tc.q, calls, tc.want)
		}
	}
}

// Walking the whole list a page at a time with each page's last message id: every
// conversation once, in order, and has_more false only on the short last page.
func TestConversationsPageThroughWithBefore(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4", "u5", "u6")
	for i, p := range []string{"u2", "u3", "u4", "u5", "u6"} {
		e.db.befriend("u1", p)
		e.db.seed("m"+strconv.Itoa(i+1), p, "u1", "x")
	}

	var seen []string
	var hasMore []bool
	target := "/conversations?limit=2"
	for range 6 {
		page := decode[conversationsPage](t, e.do("u1", http.MethodGet, target, ""))
		seen = append(seen, peerIDs(page.Conversations)...)
		hasMore = append(hasMore, page.HasMore)
		if !page.HasMore {
			break
		}
		last := page.Conversations[len(page.Conversations)-1]
		target = "/conversations?limit=2&before=" + last.LastMessage.ID
	}
	if want := []string{"u6", "u5", "u4", "u3", "u2"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("paged through %v, want %v", seen, want)
	}
	if want := []bool{true, true, false}; !reflect.DeepEqual(hasMore, want) {
		t.Errorf("has_more per page = %v, want %v", hasMore, want)
	}
}

// A conversation that gets a new message moves to the top; paging on a cursor
// that was its old last message still works and never repeats an entry.
func TestConversationsCursorSurvivesNewMessagesAndDeletes(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	for _, p := range []string{"u2", "u3", "u4"} {
		e.db.befriend("u1", p)
	}
	e.db.seed("m2", "u2", "u1", "x")
	e.db.seed("m3", "u3", "u1", "x")
	e.db.seed("m4", "u4", "u1", "x")

	first := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations?limit=1", ""))
	anchorID := first.Conversations[0].LastMessage.ID // u4 m4
	e.do("u4", http.MethodDelete, "/messages/"+anchorID, "")
	e.db.seed("m5", "u2", "u1", "u2 jumps to the top")

	rec := e.do("u1", http.MethodGet, "/conversations?before="+anchorID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a deleted message as a cursor = %d %s", rec.Code, rec.Body)
	}
	// m4's place in the ordering is still after m3 and m5; u2's new message is
	// newer than the cursor so it is not on this page, and u3 is.
	if got := peerIDs(decode[conversationsPage](t, rec).Conversations); !reflect.DeepEqual(got, []string{"u3"}) {
		t.Errorf("page before the deleted m4 = %v, want [u3]", got)
	}
}

func TestConversationsBeforeThatNamesNothingIs400(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u2", "u3")
	e.db.seed("mine", "u2", "u1", "x")
	e.db.seed("theirs", "u2", "u3", "between two other people")

	var bodies []string
	for _, before := range []string{"ghost", "theirs"} {
		e.db.calls = nil
		rec := e.do("u1", http.MethodGet, "/conversations?before="+before, "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_cursor" {
			t.Errorf("before=%s: got %d %s, want 400 bad_cursor", before, rec.Code, rec.Body)
		}
		if len(e.db.callsTo(listSummariesSQL)) != 0 {
			t.Errorf("before=%s: the list was read for a cursor that was refused", before)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Errorf("a missing message and someone else's message answer differently: %q vs %q", bodies[0], bodies[1])
	}
	if rec := e.do("u1", http.MethodGet, "/conversations?before=mine", ""); rec.Code != http.StatusOK {
		t.Errorf("one of the caller's own messages = %d, want a page", rec.Code)
	}
}

// The reason the list is one statement: the number of round trips must not grow
// with the number of friends. Completing the messages it carries costs one more
// batched read for the whole page (the reactions), whatever the page holds.
func TestConversationsIsOneStatementWhateverTheFriendCount(t *testing.T) {
	for _, friends := range []int{1, 8, 40} {
		users := []string{"u0"}
		for i := 1; i <= friends; i++ {
			users = append(users, "f"+strconv.Itoa(i))
		}
		e := newEnv(users...)
		for i := 1; i <= friends; i++ {
			p := "f" + strconv.Itoa(i)
			e.db.befriend("u0", p)
			e.db.seed("m"+strconv.Itoa(i), p, "u0", "x")
		}

		rec := e.do("u0", http.MethodGet, "/conversations?limit=200", "")
		if rec.Code != http.StatusOK || len(decode[conversationsPage](t, rec).Conversations) != friends {
			t.Fatalf("%d friends: %d %s", friends, rec.Code, rec.Body)
		}
		if len(e.db.calls) != 2 || e.db.calls[0].sql != listSummariesSQL || e.db.calls[1].sql != reactionsSQL {
			t.Errorf("%d friends took %d statements, want the one list query and one batched read of reactions", friends, len(e.db.calls))
		}
	}
}

func TestConversationsFailsCleanly(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "x")

	e.db.failOnQuery = listSummariesSQL
	if rec := e.do("u1", http.MethodGet, "/conversations", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing list = %d, want 500", rec.Code)
	}
	e.db.failOnQuery = inboxCursorSQL
	if rec := e.do("u1", http.MethodGet, "/conversations?before=m1", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing cursor lookup = %d, want 500, not bad_cursor", rec.Code)
	}
	e.db.failOnQuery = historyCursorSQL
	e.db.befriend("u1", "u2")
	if rec := e.do("u1", http.MethodGet, "/messages/u2?before=m1", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing history cursor lookup = %d, want 500, not bad_cursor", rec.Code)
	}
}

// ---- typing -----------------------------------------------------------------

func TestTypingTellsThePeerAndNobodyElseAndStoresNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	rec := e.do("u1", http.MethodPost, "/messages/u2/typing", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("got %d %q, want an empty 204", rec.Code, rec.Body)
	}

	p, frame := e.ln.one(t)
	if p.kind != "typing" || !reflect.DeepEqual(p.users, []string{"u2"}) {
		t.Errorf("push = %q to %v, want typing to u2 alone (not echoed to the typist)", p.kind, p.users)
	}
	if !reflect.DeepEqual(frame, map[string]any{"from": "u1"}) {
		t.Errorf("frame = %v, want exactly {from: u1}", frame)
	}

	for _, c := range e.db.calls {
		if c.sql != userExistsSQL && c.sql != areFriendsSQL {
			t.Errorf("typing ran %q; it may only check who is asking", c.sql)
		}
	}
	if len(e.db.msgs) != 0 || len(e.db.reads) != 0 || e.db.commits != 0 {
		t.Error("typing stored something")
	}
}

// Friend-gated with the same opaque 404 as sending, whatever the reason.
func TestTypingIsFriendGated(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")

	var bodies []string
	for _, target := range []string{"u3", "ghost", "u1"} {
		rec := e.do("u1", http.MethodPost, "/messages/"+target+"/typing", "")
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
			t.Errorf("typing to %s: got %d %s, want 404 recipient_not_found", target, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("responses differ, so one leaks: %q vs %q", b, bodies[0])
		}
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a refused typing pushed %d frames", len(e.ln.pushes))
	}

	// A database failure while checking is the same opaque refusal, not a 500.
	e.db.failOnQuery = areFriendsSQL
	if rec := e.do("u1", http.MethodPost, "/messages/u2/typing", ""); rec.Code != http.StatusNotFound || len(e.ln.pushes) != 0 {
		t.Errorf("a failing friend check = %d with %d pushes, want the opaque 404 and none", rec.Code, len(e.ln.pushes))
	}
}

func TestTypingWithNoListenerIsStillA204(t *testing.T) {
	db := newFakeDB("u1", "u2")
	db.befriend("u1", "u2")
	e := &env{db: db, ln: &listener{}, h: newHandler(db, db.begin, nil)}
	if rec := e.do("u1", http.MethodPost, "/messages/u2/typing", ""); rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- read receipts ----------------------------------------------------------

// readFrameOf returns the one frame a test expects, with its time parsed.
func readFrameOf(t *testing.T, e *env) (push, time.Time, string) {
	t.Helper()
	p, frame := e.ln.one(t)
	if len(frame) != 3 {
		t.Errorf("frame = %v, want only from, at and id", frame)
	}
	raw, ok := frame["at"].(string)
	if !ok || !strings.HasSuffix(raw, "Z") {
		t.Fatalf("at = %v, want an RFC 3339 UTC string", frame["at"])
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("at %q does not parse: %v", raw, err)
	}
	id, _ := frame["id"].(string)
	if frame["from"] != "u1" {
		t.Errorf("frame = %v, want from u1", frame)
	}
	return p, at, id
}

// A read moves the marker to the newest message the peer sent, and tells the peer
// that exact position, to the peer only.
func TestReadAdvancesToTheNewestMessageAndPushesThatPosition(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")
	m2 := e.db.seed("m2", "u2", "u1", "two")
	e.db.seed("m3", "u1", "u2", "mine: not part of what u1 reads")

	rec := e.do("u1", http.MethodPost, "/messages/u2/read", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("got %d %q, want an empty 204", rec.Code, rec.Body)
	}

	p, at, id := readFrameOf(t, e)
	if p.kind != "read" || !reflect.DeepEqual(p.users, []string{"u2"}) {
		t.Errorf("push = %q to %v, want read to u2 alone", p.kind, p.users)
	}
	if id != "m2" || !at.Equal(m2.CreatedAt) {
		t.Errorf("frame says (%v, %q), want the newest message from u2: (%v, m2)", at, id, m2.CreatedAt)
	}
	if stored := e.db.reads[[2]string{"u1", "u2"}]; stored != (readMark{at: m2.CreatedAt, id: "m2"}) {
		t.Errorf("stored = %+v, but the peer must be told exactly what was saved", stored)
	}

	// And the badge is cleared.
	type summary struct {
		Total int64 `json:"total"`
	}
	if got := decode[summary](t, e.do("u1", http.MethodGet, "/messages/unread", "")); got.Total != 0 {
		t.Errorf("unread after reading = %d", got.Total)
	}
}

// The marker is a message's position, never a clock. The database's wall clock is
// set to a year no message is in, and neither the stored marker nor the frame
// follows it; the request hands the database nothing but the two ids.
func TestTheReadMarkerIsAMessagePositionAndNeverAClock(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.now = time.Date(2031, 3, 4, 5, 6, 7, 123456789, time.UTC)
	m1 := e.db.seed("m1", "u2", "u1", "one")

	if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	calls := e.db.callsTo(markReadSQL)
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, []any{"u1", "u2"}) {
		t.Fatalf("markReadSQL ran as %+v, want only the two ids: no time and no id from the request", calls)
	}
	if stored := e.db.reads[[2]string{"u1", "u2"}]; stored.at.Equal(e.db.now) || !stored.at.Equal(m1.CreatedAt) {
		t.Errorf("stored marker = %v, want m1's created_at %v and not the clock %v", stored.at, m1.CreatedAt, e.db.now)
	}
	if _, at, _ := readFrameOf(t, e); at.Equal(e.db.now) || !at.Equal(m1.CreatedAt) {
		t.Errorf("the frame says %v, want m1's created_at %v", at, m1.CreatedAt)
	}
}

// The race that a clock marker lost: a message stamped before the read, committed
// after it. The position was copied from a message that was already visible, so the
// late one still sorts after it, and it is unread in the badge and in the list.
func TestAMessageThatCommitsAfterTheReadIsStillUnread(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	m1 := e.db.seed("m1", "u2", "u1", "one")
	if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}

	// Stamped just after m1, visible only now; the wall clock is hours later than it.
	late := e.db.seedAt("m2", "u2", "u1", "late", m1.CreatedAt.Add(time.Microsecond))
	if !late.CreatedAt.Before(e.db.now) {
		t.Fatal("the test needs the wall clock past the late message, or it proves nothing")
	}

	type summary struct {
		Conversations []unreadConversation `json:"conversations"`
		Total         int64                `json:"total"`
	}
	if got := decode[summary](t, e.do("u1", http.MethodGet, "/messages/unread", "")); got.Total != 1 {
		t.Errorf("unread = %+v, want the late message", got)
	}
	list := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", ""))
	if len(list.Conversations) != 1 || list.Conversations[0].Unread != 1 {
		t.Errorf("conversations = %+v, want 1 unread", list.Conversations)
	}

	// Reading again picks it up and says so.
	e.ln.pushes = nil
	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	if _, _, id := readFrameOf(t, e); id != "m2" {
		t.Errorf("the second read's frame says %q, want m2", id)
	}
}

// Opening a conversation with nothing from the peer in it is not an event: no
// marker is written, the peer is told nothing, and the friend check is not even
// run. The answer is the same 204.
func TestReadingAConversationWithNothingNewWritesAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	check := func(name string) {
		t.Helper()
		e.ln.pushes = nil
		if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("%s: got %d %q, want an empty 204", name, rec.Code, rec.Body)
		}
		if len(e.ln.pushes) != 0 {
			t.Errorf("%s: pushed %d frames: %+v", name, len(e.ln.pushes), e.ln.pushes)
		}
		if len(e.db.reads) != 0 {
			t.Errorf("%s: wrote a marker: %+v", name, e.db.reads)
		}
		if len(e.db.callsTo(areFriendsSQL)) != 0 {
			t.Errorf("%s: the friend check ran for a read that was not going to be announced", name)
		}
	}
	check("an empty conversation")

	e.db.seed("m1", "u1", "u2", "only mine")
	check("only the reader's own messages")

	e.db.seed("m2", "u2", "u1", "gone")
	e.do("u2", http.MethodDelete, "/messages/m2", "")
	e.ln.pushes = nil
	check("only deleted messages")
}

// Reading twice with nothing new in between is one event, not two.
func TestAReadThatMovedNothingTellsNobody(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")

	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	if len(e.ln.pushes) != 1 {
		t.Fatalf("the first read pushed %d frames, want 1", len(e.ln.pushes))
	}
	e.ln.pushes = nil
	if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a repeat read pushed %d frames: %+v", len(e.ln.pushes), e.ln.pushes)
	}
}

// The marker never rewinds: when the message it points at is deleted the newest
// live message is older than the marker, and a marker another device already moved
// further on is held. Neither tells the peer anything.
func TestTheReadMarkerNeverRewinds(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")
	m2 := e.db.seed("m2", "u2", "u1", "two")
	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	e.ln.pushes = nil

	e.do("u2", http.MethodDelete, "/messages/m2", "")
	e.ln.pushes = nil
	if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a read behind the marker pushed %d frames", len(e.ln.pushes))
	}
	if got := e.db.reads[[2]string{"u1", "u2"}]; got != (readMark{at: m2.CreatedAt, id: "m2"}) {
		t.Errorf("the marker is %+v after the marked message was deleted, want it held at m2", got)
	}

	ahead := readMark{at: m2.CreatedAt.Add(time.Hour), id: "m9"}
	e.db.reads[[2]string{"u1", "u2"}] = ahead // another device already read further
	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	if len(e.ln.pushes) != 0 || e.db.reads[[2]string{"u1", "u2"}] != ahead {
		t.Errorf("a read behind the marker moved it to %+v or pushed %d frames", e.db.reads[[2]string{"u1", "u2"}], len(e.ln.pushes))
	}
}

// A marker written before positions existed has a time and no id. It still means
// everything up to that instant is read, and it is not listed with an id it never
// had.
func TestATimestampOnlyMarkerStillBehavesAsItDid(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")
	m2 := e.db.seed("m2", "u2", "u1", "two")
	e.db.reads[[2]string{"u1", "u2"}] = readMark{at: m2.CreatedAt.Add(time.Second)} // no id

	list := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", ""))
	if len(list.Conversations) != 1 {
		t.Fatalf("conversations = %+v", list.Conversations)
	}
	c := list.Conversations[0]
	if c.Unread != 0 {
		t.Errorf("unread = %d, want everything before the instant read", c.Unread)
	}
	if c.MyReadAt == nil || !c.MyReadAt.Equal(m2.CreatedAt.Add(time.Second)) || c.MyReadID != nil {
		t.Errorf("my marker = (%v, %v), want the time and a null id", c.MyReadAt, c.MyReadID)
	}

	// Reading with nothing past the instant is not an event.
	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	if len(e.ln.pushes) != 0 {
		t.Errorf("pushed %d frames for a read behind the marker", len(e.ln.pushes))
	}

	// A newer message is unread; reading it writes the full position and pushes it.
	m3 := e.db.seed("m3", "u2", "u1", "three")
	if c := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", "")).Conversations[0]; c.Unread != 1 {
		t.Errorf("unread = %d, want m3", c.Unread)
	}
	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	if _, at, id := readFrameOf(t, e); id != "m3" || !at.Equal(m3.CreatedAt) {
		t.Errorf("frame = (%v, %q), want m3's position", at, id)
	}
}

// No marker at all: everything the peer sent is unread, and both markers are null.
func TestNoMarkerAtAllMeansEverythingIsUnread(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")
	e.db.seed("m2", "u2", "u1", "two")

	c := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", "")).Conversations[0]
	if c.Unread != 2 || c.MyReadAt != nil || c.MyReadID != nil || c.PeerReadAt != nil || c.PeerReadID != nil {
		t.Errorf("entry = %+v, want 2 unread and four null markers", c)
	}
}

// The conversation list carries each side's whole position, so a client can compare
// it with a message it holds.
func TestConversationsCarryBothPositions(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	m1 := e.db.seed("m1", "u2", "u1", "from u2")
	m2 := e.db.seed("m2", "u1", "u2", "from u1")
	e.do("u1", http.MethodPost, "/messages/u2/read", "")
	e.do("u2", http.MethodPost, "/messages/u1/read", "")

	c := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", "")).Conversations[0]
	if c.MyReadAt == nil || !c.MyReadAt.Equal(m1.CreatedAt) || c.MyReadID == nil || *c.MyReadID != "m1" {
		t.Errorf("my position = (%v, %v), want m1's", c.MyReadAt, c.MyReadID)
	}
	if c.PeerReadAt == nil || !c.PeerReadAt.Equal(m2.CreatedAt) || c.PeerReadID == nil || *c.PeerReadID != "m2" {
		t.Errorf("peer position = (%v, %v), want m2's", c.PeerReadAt, c.PeerReadID)
	}
}

// The marker is a personal bookmark and is not friend-gated; telling somebody is.
func TestReadTellsOnlyFriends(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u3", "u1", "from a non-friend")

	if rec := e.do("u1", http.MethodPost, "/messages/u3/read", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, ok := e.db.reads[[2]string{"u1", "u3"}]; !ok {
		t.Error("the bookmark was not saved for a non-friend")
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a read aimed at a non-friend pushed %d frames", len(e.ln.pushes))
	}
}

func TestReadStillSucceedsWhenTheFriendCheckFails(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")
	e.db.failOnQuery = areFriendsSQL

	if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204: the marker was saved", rec.Code)
	}
	if _, ok := e.db.reads[[2]string{"u1", "u2"}]; !ok || len(e.ln.pushes) != 0 {
		t.Errorf("saved=%v pushes=%d, want the marker saved and no frame", ok, len(e.ln.pushes))
	}
}

func TestReadRefusals(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "one")

	if rec := e.do("u1", http.MethodPost, "/messages/u1/read", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("reading yourself = %d, want 400", rec.Code)
	}
	e.db.failOnQuery = markReadSQL
	if rec := e.do("u1", http.MethodPost, "/messages/u2/read", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing write = %d, want 500", rec.Code)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a refused or failed read pushed %d frames", len(e.ln.pushes))
	}
}

// ---- replies ----------------------------------------------------------------

// sendAs sends a message with whatever extra request fields a test needs.
func sendAs(t *testing.T, e *env, user, to, body string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	fields := map[string]any{"recipient_id": to, "body": body}
	for k, v := range extra {
		fields[k] = v
	}
	return e.do(user, http.MethodPost, "/messages", jsonBody(t, fields))
}

func replyOf(t *testing.T, rec *httptest.ResponseRecorder) *replyPreview {
	t.Helper()
	return decode[messageResponse](t, rec).ReplyTo
}

func TestAReplyCarriesAPreviewOfTheOriginalInBothDirections(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "my own first")
	e.db.seed("m2", "u2", "u1", "their second")

	for _, orig := range []struct{ id, sender, body string }{
		{"m1", "u1", "my own first"},
		{"m2", "u2", "their second"},
	} {
		rec := sendAs(t, e, "u1", "u2", "answering "+orig.id, map[string]any{"reply_to_id": orig.id})
		if rec.Code != http.StatusCreated {
			t.Fatalf("reply to %s = %d (%s)", orig.id, rec.Code, rec.Body)
		}
		got := replyOf(t, rec)
		if got == nil || got.ID != orig.id || got.SenderID != orig.sender || got.Body != orig.body || got.Deleted {
			t.Errorf("reply_to = %+v, want the preview of %s", got, orig.id)
		}
		if stored := e.db.msgs[len(e.db.msgs)-1]; stored.ReplyToID == nil || *stored.ReplyToID != orig.id {
			t.Errorf("stored reply_to_id = %v, want %s", stored.ReplyToID, orig.id)
		}
	}
}

// Without this check a reply is a way to read one line out of a conversation you
// are not in, through the preview. A message of another pair and a message that does
// not exist are the same 400, and nothing is stored or pushed.
func TestAReplyToAnotherConversationsMessageIsBadReply(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	e.db.seed("secret", "u2", "u3", "something u1 must never read")
	e.db.seed("mine", "u1", "u3", "u1's own thread with u3")

	for name, target := range map[string]string{
		"another pair's message":             "secret",
		"a message of mine, other recipient": "mine",
		"a message that does not exist":      "nope",
	} {
		rec := sendAs(t, e, "u1", "u2", "hi", map[string]any{"reply_to_id": target})
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_reply" {
			t.Errorf("%s: got %d %s, want 400 bad_reply", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "must never read") {
			t.Errorf("%s: the refusal leaked the original", name)
		}
	}
	if len(e.db.msgs) != 2 || len(e.ln.pushes) != 0 || e.db.commits != 0 {
		t.Errorf("a refused reply stored or pushed something: rows=%d pushes=%d commits=%d", len(e.db.msgs), len(e.ln.pushes), e.db.commits)
	}
}

// A stranger gets the friendship answer, not a reply answer: the reply check must
// not become a way to learn whether a message id exists.
func TestTheFriendshipGateComesBeforeTheReplyCheck(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")

	rec := sendAs(t, e, "u1", "u3", "hi", map[string]any{"reply_to_id": "m1"})
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
		t.Errorf("got %d %s, want the opaque recipient 404", rec.Code, rec.Body)
	}
	if n := len(e.db.callsTo(replyTargetSQL)); n != 0 {
		t.Errorf("the reply target was checked %d times for someone who may not send at all", n)
	}
}

func TestAReplyToADeletedMessageIsAllowedAndReportsItDeleted(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "words that go")
	if rec := e.do("u2", http.MethodDelete, "/messages/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	rec := sendAs(t, e, "u1", "u2", "sorry, missed that", map[string]any{"reply_to_id": "m1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want a reply to a tombstone to be allowed", rec.Code, rec.Body)
	}
	got := replyOf(t, rec)
	if got == nil || got.ID != "m1" || got.SenderID != "u2" || got.Body != "" || !got.Deleted {
		t.Errorf("reply_to = %+v, want m1 reported deleted with an empty body", got)
	}
	if strings.Contains(rec.Body.String(), "words that go") {
		t.Error("the deleted words came back through the preview")
	}
}

// The preview is resolved when the reply is READ, so it follows the original, and a
// deletion of the original shows through too.
func TestTheReplyPreviewFollowsTheOriginalThroughAnEditAndADelete(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "teh original")
	reply := decode[messageResponse](t, sendAs(t, e, "u1", "u2", "re", map[string]any{"reply_to_id": "m1"}))

	history := func() *replyPreview {
		t.Helper()
		page := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2", ""))
		for _, m := range page.Messages {
			if m.ID == reply.ID {
				return m.ReplyTo
			}
		}
		t.Fatalf("reply %s is not in history", reply.ID)
		return nil
	}

	if got := history(); got == nil || got.Body != "teh original" {
		t.Fatalf("before the edit: %+v", got)
	}
	if rec := e.do("u2", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "the original"})); rec.Code != http.StatusOK {
		t.Fatalf("edit = %d", rec.Code)
	}
	if got := history(); got == nil || got.Body != "the original" || got.Deleted {
		t.Errorf("after the edit: %+v, want the new words to show through", got)
	}
	if rec := e.do("u2", http.MethodDelete, "/messages/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if got := history(); got == nil || got.Body != "" || !got.Deleted || got.ID != "m1" {
		t.Errorf("after the delete: %+v, want a deleted preview with no words", got)
	}
}

func TestTheReplyPreviewIsTheFirst120CharactersNotBytes(t *testing.T) {
	long := strings.Repeat("é", 130) // two bytes each: a byte cut at 120 would land mid-character... and be short
	cases := []struct {
		name, body string
		want       int
	}{
		{"short", "hello", 5},
		{"exactly 120", strings.Repeat("é", 120), 120},
		{"121", strings.Repeat("é", 121), 120},
		{"long", long, 120},
	}
	for _, tc := range cases {
		got := toReplyPreview(replyRecord{OriginalID: "m", SenderID: "u", Body: tc.body})
		if n := len([]rune(got.Body)); n != tc.want {
			t.Errorf("%s: preview has %d characters, want %d", tc.name, n, tc.want)
		}
		if !strings.HasPrefix(tc.body, got.Body) {
			t.Errorf("%s: preview is not a prefix of the body", tc.name)
		}
	}
	if got := toReplyPreview(replyRecord{Body: "stale words", Deleted: true}); got.Body != "" || !got.Deleted {
		t.Errorf("a deleted original = %+v, want no words", got)
	}
}

// A cut that lands inside a mention drops the half token; a mention the cut leaves
// whole stays; and a body that was never cut ends as its author wrote it.
func TestTheReplyPreviewNeverEndsInAHalfMention(t *testing.T) {
	const id = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	token := "<@" + id + ">"
	pad := func(n int) string { return strings.Repeat("a", n) }

	for _, tc := range []struct{ name, body, want string }{
		{"cut after the < of a mention", pad(119) + token + " tail", pad(119)},
		{"cut after the <@", pad(118) + token + " tail", pad(118)},
		{"cut one character into the id", pad(117) + token + " tail", pad(117)},
		{"cut in the middle of the id", pad(110) + token + " tail", pad(110)},
		{"cut one short of the closing >", pad(120-len(token)+1) + token + " tail", pad(120 - len(token) + 1)},
		{"cut exactly after the closing >", pad(120-len(token)) + token + " tail", pad(120-len(token)) + token},
		{"a whole mention earlier survives", "hi " + token + " " + pad(60) + " " + token, "hi " + token + " " + pad(60) + " "},
		{"a mention that was never cut", "hi " + token, "hi " + token},
		{"a short body ending in half a mention is the author's own", "see <@01JAB", "see <@01JAB"},
		{"a short body ending in a <", "a <", "a <"},
		{"multi-byte text cut next to a mention", strings.Repeat("é", 119) + token + " tail", strings.Repeat("é", 119)},
		{"a < in the middle is only text", "1 < 2 " + pad(130), "1 < 2 " + pad(114)},
	} {
		got := toReplyPreview(replyRecord{OriginalID: "m", SenderID: "u", Body: tc.body})
		if got.Body != tc.want {
			t.Errorf("%s: preview = %q, want %q", tc.name, got.Body, tc.want)
		}
		if !utf8.ValidString(got.Body) {
			t.Errorf("%s: preview is not valid UTF-8", tc.name)
		}
		if n := len([]rune(got.Body)); n > replyPreviewChars {
			t.Errorf("%s: preview has %d characters, want at most %d", tc.name, n, replyPreviewChars)
		}
	}

	// Through the endpoint, so the repair is in the response and not just the function.
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("orig", "u2", "u1", pad(110)+token+" tail")
	rec := sendAs(t, e, "u1", "u2", "answering", map[string]any{"reply_to_id": "orig"})
	if got := replyOf(t, rec); got == nil || got.Body != pad(110) {
		t.Errorf("reply_to over HTTP = %+v, want the body cut before the half mention", got)
	}
}

// reply_to_id is immutable: PATCH neither accepts it nor changes it.
func TestPatchCannotChangeReplyToID(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "first")
	e.db.seed("m2", "u2", "u1", "second")
	reply := decode[messageResponse](t, sendAs(t, e, "u1", "u2", "re first", map[string]any{"reply_to_id": "m1"}))

	rec := e.do("u1", http.MethodPatch, "/messages/"+reply.ID, jsonBody(t, map[string]any{"body": "re first, fixed", "reply_to_id": "m2"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if got.Body != "re first, fixed" || got.ReplyTo == nil || got.ReplyTo.ID != "m1" {
		t.Errorf("edited = %+v, want the body changed and the reply still pointing at m1", got)
	}
	if stored := e.db.find(reply.ID); stored.ReplyToID == nil || *stored.ReplyToID != "m1" {
		t.Errorf("stored reply_to_id = %v, want m1", stored.ReplyToID)
	}
	// And the UPDATE could not have set it: it is never given the field.
	if args := e.db.callsTo(editMessageSQL)[0].args; len(args) != 3 {
		t.Errorf("the edit statement got %d arguments %v, want id, sender and body only", len(args), args)
	}
}

// A tombstone says nothing, including what it answered.
func TestDeletingAReplyClearsItsLink(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "first")
	reply := decode[messageResponse](t, sendAs(t, e, "u1", "u2", "re", map[string]any{"reply_to_id": "m1"}))
	e.ln.pushes = nil

	if rec := e.do("u1", http.MethodDelete, "/messages/"+reply.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if stored := e.db.find(reply.ID); stored.ReplyToID != nil {
		t.Errorf("a tombstone still points at %s", *stored.ReplyToID)
	}
	_, frame := e.ln.frame(t)
	if msg := frame["message"].(map[string]any); msg["reply_to"] != nil {
		t.Errorf("the delete frame still carries reply_to: %v", msg["reply_to"])
	}
}

// Even if some row held a reply_to_id into another conversation, the read-time
// join would not resolve it: the preview is scoped to the reply's own pair.
func TestAReplyPointingOutsideItsConversationResolvesToNothing(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("secret", "u2", "u3", "private")
	e.db.seed("m1", "u1", "u2", "hi")
	elsewhere := "secret"
	e.db.find("m1").ReplyToID = &elsewhere

	page := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2", ""))
	if len(page.Messages) != 1 || page.Messages[0].ReplyTo != nil {
		t.Errorf("messages = %+v, want reply_to null", page.Messages)
	}
	if strings.Contains(string(mustJSON(t, page)), "private") {
		t.Error("a line of another conversation came through a preview")
	}
}

// A repeat send of a reply is still the original and still pushes nothing.
func TestARepeatReplyReturnsTheOriginal(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "first")
	first := sendAs(t, e, "u1", "u2", "re", map[string]any{"reply_to_id": "m1", "nonce": "n"})
	e.ln.pushes = nil

	again := sendAs(t, e, "u1", "u2", "re", map[string]any{"reply_to_id": "m1", "nonce": "n"})
	if again.Code != http.StatusOK || decode[messageResponse](t, again).ID != decode[messageResponse](t, first).ID {
		t.Errorf("repeat = %d %s", again.Code, again.Body)
	}
	if replyOf(t, again) == nil || len(e.ln.pushes) != 0 {
		t.Errorf("the repeat lost its reply_to or pushed")
	}
}

// ---- pins -------------------------------------------------------------------

func (e *env) pin(user, id string) *httptest.ResponseRecorder {
	return e.do(user, http.MethodPut, "/messages/"+id+"/pin", "")
}

func (e *env) unpin(user, id string) *httptest.ResponseRecorder {
	return e.do(user, http.MethodDelete, "/messages/"+id+"/pin", "")
}

// A pin belongs to the conversation, not to whoever wrote the message: either
// person may pin either person's message. (Edit and delete are the sender's alone.)
func TestEitherParticipantMayPinEitherMessage(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "u1's")
	e.db.seed("m2", "u2", "u1", "u2's")
	e.db.seed("m3", "u1", "u2", "u1's again")
	e.db.seed("m4", "u2", "u1", "u2's again")

	for _, tc := range []struct{ pinner, id, author string }{
		{"u1", "m1", "u1"}, // their own
		{"u1", "m2", "u2"}, // the other person's
		{"u2", "m3", "u1"}, // the other person's
		{"u2", "m4", "u2"}, // their own
	} {
		before := e.db.seq
		e.ln.pushes = nil
		rec := e.pin(tc.pinner, tc.id)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s pinning %s = %d (%s)", tc.pinner, tc.id, rec.Code, rec.Body)
		}
		got := decode[messageResponse](t, rec)
		if got.ID != tc.id || got.PinnedAt == nil || got.Seq <= before {
			t.Errorf("%s pinning %s = %+v, want it pinned with a fresh seq past %d", tc.pinner, tc.id, got, before)
		}

		p, frame := e.ln.frame(t)
		if p.kind != "dm" || !reflect.DeepEqual(sorted(p.users), []string{"u1", "u2"}) {
			t.Errorf("push = %q to %v, want dm to both", p.kind, p.users)
		}
		// `from` stays the author: it never changes meaning from one op to the next.
		if frame["op"] != "pin" || frame["from"] != tc.author {
			t.Errorf("frame = %v, want op pin from the message's author %s", frame, tc.author)
		}
		if msg := frame["message"].(map[string]any); msg["id"] != tc.id || msg["pinned_at"] == nil {
			t.Errorf("frame message = %v", msg)
		}
	}
}

// A repeat is not news: no seq is drawn, no frame is pushed, and the pin is
// unchanged — whoever repeats it.
func TestARepeatPinDrawsNoSeqAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")

	first := decode[messageResponse](t, e.pin("u1", "m1"))
	seq, pinnedAt := e.db.seq, e.db.find("m1").PinnedAt
	e.ln.pushes = nil
	e.db.calls = nil

	for _, who := range []string{"u1", "u2"} {
		rec := e.pin(who, "m1")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s repeating the pin = %d (%s)", who, rec.Code, rec.Body)
		}
		got := decode[messageResponse](t, rec)
		if got.Seq != first.Seq || got.PinnedAt == nil || !got.PinnedAt.Equal(*first.PinnedAt) {
			t.Errorf("repeat = %+v, want the pin as it was: %+v", got, first)
		}
	}
	if e.db.seq != seq || e.db.find("m1").PinnedAt != pinnedAt || e.db.find("m1").Seq != first.Seq {
		t.Errorf("a repeat moved seq %d -> %d or the pin", seq, e.db.seq)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a repeat pushed %d frames: %+v", len(e.ln.pushes), e.ln.pushes)
	}
	if n := len(e.db.callsTo(pinMessageSQL)); n != 0 {
		t.Errorf("a repeat reached the UPDATE %d times", n)
	}
}

func TestUnpinningDrawsASeqAndPushesAndARepeatDoesNot(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	e.pin("u1", "m1")
	pinSeq := e.db.find("m1").Seq
	e.ln.pushes = nil

	rec := e.unpin("u2", "m1") // the other person may take it down
	if rec.Code != http.StatusOK {
		t.Fatalf("unpin = %d (%s)", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if got.PinnedAt != nil || got.Seq <= pinSeq {
		t.Errorf("unpinned = %+v, want pinned_at null and a seq past %d", got, pinSeq)
	}
	if e.db.find("m1").PinnedAt != nil {
		t.Error("the message is still pinned")
	}
	_, frame := e.ln.frame(t)
	if msg := frame["message"].(map[string]any); frame["op"] != "pin" || msg["pinned_at"] != nil {
		t.Errorf("frame = %v, want op pin carrying pinned_at null", frame)
	}

	// Repeats: taking down what is down, and taking down what was never up.
	e.db.seed("m2", "u2", "u1", "never pinned")
	seq := e.db.seq
	e.ln.pushes = nil
	for _, c := range []struct{ user, id string }{{"u1", "m1"}, {"u2", "m1"}, {"u1", "m2"}} {
		if rec := e.unpin(c.user, c.id); rec.Code != http.StatusOK {
			t.Errorf("%s unpinning %s = %d, want a no-op success", c.user, c.id, rec.Code)
		}
	}
	if e.db.seq != seq || len(e.ln.pushes) != 0 || len(e.db.callsTo(unpinMessageSQL)) != 1 {
		t.Errorf("repeats moved seq %d -> %d, pushed %d, or re-ran the UPDATE", seq, e.db.seq, len(e.ln.pushes))
	}
}

// A conversation keeps at most 50 pins. The 51st is a 400 that writes and pushes
// nothing; a repeat at the cap is still just a success; another conversation is
// unaffected; and taking one down makes room.
func TestAConversationKeepsAtMostFiftyPins(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	for i := range 51 {
		id := "p" + strconv.Itoa(1000+i)
		sender, recipient := "u1", "u2"
		if i%2 == 1 {
			sender, recipient = "u2", "u1"
		}
		e.db.seed(id, sender, recipient, "message "+id)
		if i < 49 {
			at := e.db.clock.Add(time.Duration(i) * time.Second)
			e.db.find(id).PinnedAt = &at
		}
	}
	e.db.seed("other", "u3", "u1", "a different conversation")

	// 49 pinned: the 50th fits...
	if rec := e.pin("u1", "p1049"); rec.Code != http.StatusOK {
		t.Fatalf("the 50th pin = %d (%s)", rec.Code, rec.Body)
	}
	if got := e.db.pinned("u1", "u2"); got != 50 {
		t.Fatalf("pins = %d, want 50", got)
	}

	// ...and the 51st does not.
	seq := e.db.seq
	e.ln.pushes = nil
	rec := e.pin("u2", "p1050")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "too_many_pins" {
		t.Errorf("the 51st pin = %d %s, want 400 too_many_pins", rec.Code, rec.Body)
	}
	if e.db.find("p1050").PinnedAt != nil || e.db.seq != seq || len(e.ln.pushes) != 0 {
		t.Error("a refused pin changed or pushed something")
	}

	// A repeat of a pin that is already in is not a new pin, so the cap does not apply.
	if rec := e.pin("u1", "p1000"); rec.Code != http.StatusOK {
		t.Errorf("repeating a pin at the cap = %d, want 200", rec.Code)
	}
	// The cap is per conversation.
	if rec := e.pin("u1", "other"); rec.Code != http.StatusOK {
		t.Errorf("a pin in another conversation = %d, want 200", rec.Code)
	}
	// Room appears when one comes down.
	if rec := e.unpin("u1", "p1000"); rec.Code != http.StatusOK {
		t.Fatalf("unpin = %d", rec.Code)
	}
	if rec := e.pin("u2", "p1050"); rec.Code != http.StatusOK {
		t.Errorf("a pin after making room = %d (%s)", rec.Code, rec.Body)
	}
}

func TestADeletedMessageCannotBePinnedAndDeletingAPinnedOneClearsTheMark(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "pin me then lose me")
	e.db.seed("m2", "u1", "u2", "gone before the pin")
	e.pin("u2", "m1")
	if e.db.find("m1").PinnedAt == nil {
		t.Fatal("setup: m1 should be pinned")
	}
	e.ln.pushes = nil

	if rec := e.do("u1", http.MethodDelete, "/messages/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if stored := e.db.find("m1"); stored.PinnedAt != nil || stored.DeletedAt == nil {
		t.Errorf("stored = %+v, want a tombstone that is no longer pinned", stored)
	}
	_, frame := e.ln.frame(t)
	if msg := frame["message"].(map[string]any); frame["op"] != "delete" || msg["pinned_at"] != nil {
		t.Errorf("the delete frame = %v, want pinned_at null", frame)
	}
	if pins := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2/pins", "")); len(pins.Messages) != 0 {
		t.Errorf("pins = %v, want none", msgIDs(pins.Messages))
	}

	e.do("u1", http.MethodDelete, "/messages/m2", "")
	e.ln.pushes = nil
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"pin of a deleted message":   e.pin("u2", "m2"),
		"unpin of a deleted message": e.unpin("u2", "m2"),
	} {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s = %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
	}
	if e.db.find("m2").PinnedAt != nil || len(e.ln.pushes) != 0 {
		t.Error("a deleted message was pinned or a frame was pushed")
	}
}

// Missing, deleted, someone else's conversation and a conversation whose other
// person is no longer a friend are one answer, byte for byte, and nothing is written.
func TestPinRefusalsAreOpaque(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	e.db.seed("m1", "u1", "u2", "between u1 and u2")
	e.db.seed("gone", "u1", "u2", "deleted")
	e.db.seed("cold", "u1", "u4", "no longer friends")
	e.do("u1", http.MethodDelete, "/messages/gone", "")
	e.ln.pushes = nil
	e.db.commits = 0

	var bodies []string
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a third person, who is a friend of both": e.pin("u3", "m1"),
		"a message that does not exist":           e.pin("u1", "nope"),
		"a deleted message":                       e.pin("u1", "gone"),
		"a former friend":                         e.pin("u1", "cold"),
		"unpin by a third person":                 e.unpin("u3", "m1"),
	} {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s: got %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("refusals differ, so one of them leaks: %q vs %q", b, bodies[0])
		}
	}
	if e.db.commits != 0 || len(e.ln.pushes) != 0 || e.db.find("m1").PinnedAt != nil || e.db.find("cold").PinnedAt != nil {
		t.Errorf("a refused pin wrote or pushed: commits=%d pushes=%d", e.db.commits, len(e.ln.pushes))
	}
}

// Pinning runs the same way every seq-drawing write does: lock first, inside the
// transaction, and a failure pushes nothing.
func TestPinRunsInsideTheSeqLockAndAFailedPinPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")

	e.db.calls = nil
	if rec := e.pin("u2", "m1"); rec.Code != http.StatusOK {
		t.Fatalf("pin = %d", rec.Code)
	}
	var inTx []string
	for _, c := range e.db.calls {
		if c.inTx {
			inTx = append(inTx, c.sql)
		}
	}
	if want := []string{lockSeqSQL, participantMessageSQL, countPinsSQL, pinMessageSQL}; !reflect.DeepEqual(inTx, want) {
		t.Errorf("in the transaction: %d statements in the wrong order, want lock, lookup, count, update", len(inTx))
	}
	if e.db.commits != 1 {
		t.Errorf("commits = %d, want 1", e.db.commits)
	}

	e.db.seed("m2", "u1", "u2", "second")
	e.ln.pushes = nil
	e.db.commits = 0
	e.db.failOnQuery = pinMessageSQL
	if rec := e.pin("u1", "m2"); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing pin = %d, want 500", rec.Code)
	}
	if e.db.commits != 0 || len(e.ln.pushes) != 0 || e.db.find("m2").PinnedAt != nil {
		t.Error("a failed pin committed or pushed")
	}
}

// A pin and an unpin each draw a seq, so a client that was away learns of them from
// the same feed as everything else — nothing separate.
func TestAPinAndAnUnpinReachAnOfflineClientThroughTheCatchUpFeed(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "first")
	e.db.seed("m2", "u1", "u2", "second")
	cursor := decode[page](t, e.do("u2", http.MethodGet, "/messages?since=0", "")).Cursor

	e.pin("u1", "m1")
	feed := decode[page](t, e.do("u2", http.MethodGet, "/messages?since="+strconv.FormatInt(cursor, 10), ""))
	if !reflect.DeepEqual(msgIDs(feed.Messages), []string{"m1"}) || feed.Messages[0].PinnedAt == nil {
		t.Fatalf("after a pin the feed = %+v, want m1 carrying its pin", feed.Messages)
	}
	cursor = feed.Cursor

	e.unpin("u2", "m1")
	feed = decode[page](t, e.do("u2", http.MethodGet, "/messages?since="+strconv.FormatInt(cursor, 10), ""))
	if !reflect.DeepEqual(msgIDs(feed.Messages), []string{"m1"}) || feed.Messages[0].PinnedAt != nil {
		t.Errorf("after an unpin the feed = %+v, want m1 with the pin gone", feed.Messages)
	}
}

func TestPinsListsTheConversationsPinsNewestPinnedFirst(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	for _, m := range []struct{ id, from, to string }{
		{"m1", "u1", "u2"}, {"m2", "u2", "u1"}, {"m3", "u1", "u2"}, {"m4", "u2", "u1"}, {"x1", "u1", "u3"},
	} {
		e.db.seed(m.id, m.from, m.to, "body "+m.id)
	}
	// Pinned out of message order, by both people, in two conversations.
	e.pin("u1", "m3")
	e.pin("u2", "m1")
	e.pin("u1", "x1")
	e.pin("u2", "m4")

	for _, user := range []string{"u1", "u2"} {
		path := "/messages/u2/pins"
		if user == "u2" {
			path = "/messages/u1/pins"
		}
		rec := e.do(user, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s pins = %d (%s)", user, rec.Code, rec.Body)
		}
		got := decode[historyPage](t, rec)
		if want := []string{"m4", "m1", "m3"}; !reflect.DeepEqual(msgIDs(got.Messages), want) {
			t.Errorf("%s sees pins %v, want %v: newest pinned first, this conversation only", user, msgIDs(got.Messages), want)
		}
		for _, m := range got.Messages {
			if m.PinnedAt == nil {
				t.Errorf("%s listed %s without its pin", user, m.ID)
			}
		}
	}

	if rec := e.do("u1", http.MethodGet, "/messages/u4/pins", ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
		t.Errorf("pins with a non-friend = %d %s, want the opaque recipient 404", rec.Code, rec.Body)
	}
	if rec := e.do("u1", http.MethodGet, "/messages/u1/pins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("pins with yourself = %d, want 404", rec.Code)
	}

	// No pins is an empty list, not null.
	e.db.befriend("u1", "u4")
	rec := e.do("u1", http.MethodGet, "/messages/u4/pins", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"messages":[]`) {
		t.Errorf("no pins = %d %s, want an empty list", rec.Code, rec.Body)
	}
}

// ---- mentions ---------------------------------------------------------------

func TestMentionCandidates(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
	}{
		{"none", "no mentions here", nil},
		{"one", "hi <@u2>", []string{"u2"}},
		{"first appearance order", "<@c> <@a> <@b>", []string{"c", "a", "b"}},
		{"duplicates collapse to the first", "<@a> <@b> <@a> <@b> <@c>", []string{"a", "b", "c"}},
		{"alphanumerics only", "<@01ARZ3NDEKTSV4RRFFQ69G5FAV>", []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}},
		{"empty", "<@>", nil},
		{"a space inside", "<@a b>", nil},
		{"the bang form is not ours", "<@!u2>", nil},
		{"unclosed", "<@u2", nil},
		{"no at", "<u2>", nil},
		{"punctuation inside", "<@u-2> <@u_2>", nil},
		{"in the middle of text", "see<@u2>now", []string{"u2"}},
	}
	for _, tc := range cases {
		if got := mentionCandidates(tc.body); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: mentionCandidates(%q) = %v, want %v", tc.name, tc.body, got, tc.want)
		}
	}
}

func TestReportedMentionsKeepsOnlyTheReaderAndTheReadersFriends(t *testing.T) {
	friends := map[string]bool{"f1": true, "f2": true, "f3": true}

	got := reportedMentions([]string{"stranger", "f2", "me", "ghost", "f1"}, "me", friends)
	if want := []string{"f2", "me", "f1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v: friends and self, in order, strangers dropped", got, want)
	}

	// Never nil: a client gets [] and not null.
	if got := reportedMentions(nil, "me", friends); got == nil || len(got) != 0 {
		t.Errorf("no candidates = %#v, want an empty non-nil slice", got)
	}
	if got := reportedMentions([]string{"nobody"}, "me", nil); got == nil || len(got) != 0 {
		t.Errorf("no friends = %#v, want an empty non-nil slice", got)
	}
}

func TestReportedMentionsAreCappedAtSixteen(t *testing.T) {
	friends := map[string]bool{}
	var candidates, strangers []string
	for i := range 30 {
		id := "f" + strconv.Itoa(100+i)
		friends[id] = true
		candidates = append(candidates, id)
		strangers = append(strangers, "s"+strconv.Itoa(100+i))
	}

	got := reportedMentions(candidates, "me", friends)
	if len(got) != 16 || !reflect.DeepEqual(got, candidates[:16]) {
		t.Errorf("got %d mentions %v, want the first 16 in order", len(got), got)
	}
	// What is dropped does not use up a slot.
	got = reportedMentions(append(append([]string{}, strangers...), candidates...), "me", friends)
	if !reflect.DeepEqual(got, candidates[:16]) {
		t.Errorf("with 30 strangers first, got %v, want the first 16 friends", got)
	}
}

// What the server reports is what the reader can resolve: friends and themselves.
// An id that is neither stays in the text and is not reported — and a send is never
// refused over one.
func TestMentionsAreReportedForFriendsAndSelfAndUnknownIDsStayText(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	body := "hey <@u2> and <@u1> and <@u3> and <@ghost> and <@u2> again"

	rec := sendAs(t, e, "u1", "u2", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a body with an unknown mention was refused: %d %s", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if want := []string{"u2", "u1", "u3"}; !reflect.DeepEqual(got.Mentions, want) {
		t.Errorf("sender sees mentions %v, want %v (first appearance, no duplicate, ghost dropped)", got.Mentions, want)
	}
	if got.Body != body {
		t.Errorf("body = %q, want the text untouched, ghost included", got.Body)
	}

	// Relative to the reader: u3 is u1's friend and not u2's, so for u2 it is plain text.
	frames := e.ln.frames(t)
	read := func(user string) []any { return frames[user]["message"].(map[string]any)["mentions"].([]any) }
	if want := []any{"u2", "u1", "u3"}; !reflect.DeepEqual(read("u1"), want) {
		t.Errorf("u1's frame mentions %v, want %v", read("u1"), want)
	}
	if want := []any{"u2", "u1"}; !reflect.DeepEqual(read("u2"), want) {
		t.Errorf("u2's frame mentions %v, want %v: u3 is not u2's friend", read("u2"), want)
	}

	// History says the same to each reader.
	for user, want := range map[string][]string{"u1": {"u2", "u1", "u3"}, "u2": {"u2", "u1"}} {
		path := "/messages/u2"
		if user == "u2" {
			path = "/messages/u1"
		}
		page := decode[historyPage](t, e.do(user, http.MethodGet, path, ""))
		if len(page.Messages) != 1 || !reflect.DeepEqual(page.Messages[0].Mentions, want) {
			t.Errorf("%s's history mentions %+v, want %v", user, page.Messages, want)
		}
	}
}

// Mentions are derived from the body on every read and never stored, so an edit that
// removes one removes it everywhere, and one that adds one adds it.
func TestAnEditChangesTheMentions(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	sent := decode[messageResponse](t, sendAs(t, e, "u1", "u2", "ping <@u2>", nil))
	if !reflect.DeepEqual(sent.Mentions, []string{"u2"}) {
		t.Fatalf("mentions = %v", sent.Mentions)
	}

	edited := decode[messageResponse](t, e.do("u1", http.MethodPatch, "/messages/"+sent.ID, jsonBody(t, map[string]any{"body": "never mind"})))
	if edited.Mentions == nil || len(edited.Mentions) != 0 {
		t.Errorf("after removing it, mentions = %#v, want []", edited.Mentions)
	}
	feed := decode[page](t, e.do("u2", http.MethodGet, "/messages?since=0", ""))
	if len(feed.Messages) != 1 || len(feed.Messages[0].Mentions) != 0 {
		t.Errorf("the feed still reports a mention that was edited out: %+v", feed.Messages)
	}

	again := decode[messageResponse](t, e.do("u1", http.MethodPatch, "/messages/"+sent.ID, jsonBody(t, map[string]any{"body": "<@u1> and <@u2>"})))
	if !reflect.DeepEqual(again.Mentions, []string{"u1", "u2"}) {
		t.Errorf("after adding them, mentions = %v", again.Mentions)
	}
}

// A page with nothing that looks like a mention asks nothing about friends.
func TestNoMentionsMeansNoFriendLookup(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u2", "u1", "plain text, and an email a@b.c")

	e.db.calls = nil
	e.do("u1", http.MethodGet, "/messages/u2", "")
	if n := len(e.db.callsTo(mentionableSQL)); n != 0 {
		t.Errorf("a page without mentions ran %d friend lookups", n)
	}

	e.db.seed("m2", "u2", "u1", "now <@u1>")
	e.db.seed("m3", "u2", "u1", "and <@u2>")
	e.db.calls = nil
	e.do("u1", http.MethodGet, "/messages/u2", "")
	if got := e.db.callsTo(mentionableSQL); len(got) != 1 {
		t.Errorf("a page with mentions ran %d friend lookups, want one for the whole page", len(got))
	} else if cands := got[0].args[1].([]string); !reflect.DeepEqual(cands, []string{"u2"}) {
		t.Errorf("candidates = %v, want only ids that are not the reader", cands)
	}
}

// ---- reactions: read out of the table --------------------------------------

// Every message carries what the table holds, per reader, in the order the emoji
// were first used.
func TestReactionsAreReadFromTheTablePerReader(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "react to me")
	e.db.seed("m2", "u2", "u1", "and nothing on me")
	e.db.react("m1", "u2", "❤️")
	e.db.react("m1", "u1", "👍")
	e.db.react("m1", "u2", "👍")
	e.db.react("m1", "u2", "👍") // the same row again: still one

	byReader := map[string][]reactionSummary{
		"u1": {{Emoji: "❤️", Count: 1, Me: false}, {Emoji: "👍", Count: 2, Me: true}},
		"u2": {{Emoji: "❤️", Count: 1, Me: true}, {Emoji: "👍", Count: 2, Me: true}},
	}
	for reader, want := range byReader {
		path := "/messages/u2"
		if reader == "u2" {
			path = "/messages/u1"
		}
		page := decode[historyPage](t, e.do(reader, http.MethodGet, path, ""))
		got := map[string][]reactionSummary{}
		for _, m := range page.Messages {
			got[m.ID] = m.Reactions
		}
		if !reflect.DeepEqual(got["m1"], want) {
			t.Errorf("%s sees %+v on m1, want %+v (first-reacted order, me per reader)", reader, got["m1"], want)
		}
		if got["m2"] == nil || len(got["m2"]) != 0 {
			t.Errorf("%s sees %#v on m2, want []", reader, got["m2"])
		}
	}

	// The catch-up feed and the conversation list carry them too.
	feed := decode[page](t, e.do("u1", http.MethodGet, "/messages?since=0", ""))
	for _, m := range feed.Messages {
		if m.ID == "m1" && !reflect.DeepEqual(m.Reactions, byReader["u1"]) {
			t.Errorf("the feed carries %+v on m1", m.Reactions)
		}
	}
	e.db.seed("m3", "u1", "u2", "last")
	e.db.react("m3", "u2", "🎉")
	list := decode[conversationsPage](t, e.do("u1", http.MethodGet, "/conversations", ""))
	if len(list.Conversations) != 1 || !reflect.DeepEqual(list.Conversations[0].LastMessage.Reactions, []reactionSummary{{Emoji: "🎉", Count: 1, Me: false}}) {
		t.Errorf("the list's last_message = %+v, want its reaction", list.Conversations)
	}
}

// Each person's frame has THEIR `me`: one shared frame would highlight the wrong
// reaction for somebody.
func TestEachPersonsFrameHasTheirOwnMe(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "teh")
	e.db.react("m1", "u1", "👍")
	e.db.react("m1", "u2", "👍")
	e.db.react("m1", "u2", "❤️")

	if rec := e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "the"})); rec.Code != http.StatusOK {
		t.Fatalf("edit = %d", rec.Code)
	}
	frames := e.ln.frames(t)
	reactions := func(user string) []any { return frames[user]["message"].(map[string]any)["reactions"].([]any) }
	want := map[string][]any{
		"u1": {map[string]any{"emoji": "👍", "count": float64(2), "me": true}, map[string]any{"emoji": "❤️", "count": float64(1), "me": false}},
		"u2": {map[string]any{"emoji": "👍", "count": float64(2), "me": true}, map[string]any{"emoji": "❤️", "count": float64(1), "me": true}},
	}
	for user, w := range want {
		if !reflect.DeepEqual(reactions(user), w) {
			t.Errorf("%s's frame reactions = %v, want %v", user, reactions(user), w)
		}
	}
}

// Deleting a message deletes what was said about it.
func TestDeletingAMessageClearsItsReactions(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "bye")
	e.db.seed("m2", "u1", "u2", "stays")
	e.db.react("m1", "u2", "👍")
	e.db.react("m2", "u2", "👍")

	if rec := e.do("u1", http.MethodDelete, "/messages/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if len(e.db.reactions) != 1 || e.db.reactions[0].messageID != "m2" {
		t.Errorf("reactions left = %+v, want only m2's", e.db.reactions)
	}
	if _, frame := e.ln.frame(t); len(frame["message"].(map[string]any)["reactions"].([]any)) != 0 {
		t.Errorf("the tombstone's frame still has reactions: %v", frame["message"])
	}
}

// The batched reads cost the same number of statements whatever the page holds.
func TestAPageOfMessagesCostsAFixedNumberOfDerivedReads(t *testing.T) {
	for _, n := range []int{1, 30} {
		e := newEnv("u1", "u2")
		e.db.befriend("u1", "u2")
		for i := range n {
			id := "m" + strconv.Itoa(1000+i)
			e.db.seed(id, "u2", "u1", "hi <@u1>")
			e.db.react(id, "u1", "👍")
		}
		e.db.calls = nil
		if rec := e.do("u1", http.MethodGet, "/messages/u2", ""); rec.Code != http.StatusOK {
			t.Fatalf("history = %d", rec.Code)
		}
		var derived int
		for _, c := range e.db.calls {
			switch c.sql {
			case replyPreviewsSQL, reactionsSQL, mentionableSQL:
				derived++
			}
		}
		if derived != 1 { // reactions only: nothing replies, and the only mention is the reader
			t.Errorf("%d messages ran %d derived reads, want 1", n, derived)
		}
	}
}

// ---- the derived parts failing ---------------------------------------------

// A message is whole or it is an error: when a read it depends on fails, the
// endpoint answers 500 and never a message with its reactions quietly missing.
func TestAFailedDerivedReadIsA500AndNeverAHalfMessage(t *testing.T) {
	for _, failing := range []string{reactionsSQL, replyPreviewsSQL, mentionableSQL} {
		e := newEnv("u1", "u2")
		e.db.befriend("u1", "u2")
		e.db.seed("orig", "u2", "u1", "x")
		reply := decode[messageResponse](t, sendAs(t, e, "u1", "u2", "re <@u2>", map[string]any{"reply_to_id": "orig"}))
		e.db.failOnQuery = failing

		for name, rec := range map[string]*httptest.ResponseRecorder{
			"history":       e.do("u1", http.MethodGet, "/messages/u2", ""),
			"catch-up":      e.do("u1", http.MethodGet, "/messages?since=0", ""),
			"conversations": e.do("u1", http.MethodGet, "/conversations", ""),
			"edit":          e.do("u1", http.MethodPatch, "/messages/"+reply.ID, jsonBody(t, map[string]any{"body": "re again <@u2>"})),
			"pin":           e.pin("u1", reply.ID),
		} {
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("%s with a failing %.30q = %d, want 500", name, strings.Join(strings.Fields(failing), " "), rec.Code)
			}
		}
		// The pin above was stored before its response failed, so there is one to list.
		if rec := e.do("u1", http.MethodGet, "/messages/u2/pins", ""); rec.Code != http.StatusInternalServerError {
			t.Errorf("pins with a failing %.30q = %d, want 500", strings.Join(strings.Fields(failing), " "), rec.Code)
		}
	}
}

// A new message that was stored but whose frame or response could not be built is a
// 500 with nothing pushed; the retry (the nonce makes it safe) finds it already there.
func TestASendWhoseResponseCannotBeBuiltIsRetrySafe(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.failOnQuery = reactionsSQL

	rec := sendAs(t, e, "u1", "u2", "hi", map[string]any{"nonce": "n-1"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(e.db.msgs) != 1 || len(e.ln.pushes) != 0 {
		t.Fatalf("rows=%d pushes=%d, want the message stored and no frame built from a failed read", len(e.db.msgs), len(e.ln.pushes))
	}

	e.db.failOnQuery = ""
	again := sendAs(t, e, "u1", "u2", "hi", map[string]any{"nonce": "n-1"})
	if again.Code != http.StatusOK || len(e.db.msgs) != 1 {
		t.Errorf("retry = %d with %d rows, want 200 and still one message", again.Code, len(e.db.msgs))
	}
}

// With nobody listening no frame is built, so a handler with a nil Notify does not
// pay for two more rounds of derived reads on every write.
func TestNoListenerMeansNoFrameIsBuilt(t *testing.T) {
	db := newFakeDB("u1", "u2")
	db.befriend("u1", "u2")
	e := &env{db: db, ln: &listener{}, h: newHandler(db, db.begin, nil)}

	if rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u2", "body": "hi"})); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := len(db.callsTo(reactionsSQL)); n != 1 {
		t.Errorf("reactions were read %d times, want once, for the response alone", n)
	}
}

// ---- reactions: the write side ----------------------------------------------

func reactAs(t *testing.T, e *env, user, id, emoji string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(user, http.MethodPut, "/messages/"+id+"/reactions", jsonBody(t, map[string]any{"emoji": emoji}))
}

func (e *env) unreact(user, id, emoji string) *httptest.ResponseRecorder {
	return e.do(user, http.MethodDelete, "/messages/"+id+"/reactions/"+url.PathEscape(emoji), "")
}

// reactionsOn is how many reaction rows a message has.
func reactionsOn(e *env, id string) int {
	n := 0
	for _, r := range e.db.reactions {
		if r.messageID == id {
			n++
		}
	}
	return n
}

// manyEmoji is n distinct emoji: the smileys block, one code point each.
func manyEmoji(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune(0x1F600 + i))
	}
	return out
}

// reactionsOf is the reactions in a response, as a client reads them.
func reactionsOf(t *testing.T, rec *httptest.ResponseRecorder) []reactionSummary {
	t.Helper()
	return decode[messageResponse](t, rec).Reactions
}

// The emoji is a SHAPE check and not an emoji test: nothing here knows what an emoji
// is, only what is certainly not one. Each rejected case is a way the field could
// become a second message body.
var acceptedEmoji = []struct{ name, in, want string }{
	{"a thumbs up", "👍", "👍"},
	{"a heart with its variation selector", "❤\U0000FE0F", "❤\U0000FE0F"},
	{"a flag, two regional indicators", "🇯🇵", "🇯🇵"},
	{"a skin tone", "👍🏽", "👍🏽"},
	{"the rainbow flag, a ZWJ sequence", "🏳\U0000FE0F\U0000200D🌈", "🏳\U0000FE0F\U0000200D🌈"},
	{"a family of three, joined by ZWJ", "👨\U0000200D👩\U0000200D👧", "👨\U0000200D👩\U0000200D👧"},
	{"a family of four", "👨\U0000200D👩\U0000200D👧\U0000200D👦", "👨\U0000200D👩\U0000200D👧\U0000200D👦"},
	{"a family of four with skin tones, 11 runes", "👨🏽\U0000200D👩🏽\U0000200D👧🏽\U0000200D👦🏽", "👨🏽\U0000200D👩🏽\U0000200D👧🏽\U0000200D👦🏽"},
	{"the Scotland flag, a base, tag characters and the cancel tag", "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F", "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"},
	{"a keycap with the emoji selector", "1\U0000FE0F\U000020E3", "1\U0000FE0F\U000020E3"},
	{"a keycap without it", "9\U000020E3", "9\U000020E3"},
	{"the keycap zero", "0\U0000FE0F\U000020E3", "0\U0000FE0F\U000020E3"},
	{"the keycap number sign", "#\U0000FE0F\U000020E3", "#\U0000FE0F\U000020E3"},
	{"the keycap asterisk", "*\U0000FE0F\U000020E3", "*\U0000FE0F\U000020E3"},
	{"the longest allowed sequence, a ZWJ chain of 16 runes", zwjChain(8), zwjChain(8)},
	{"a copyright sign, a symbol", "©\U0000FE0F", "©\U0000FE0F"},
	{"information source, filed as a letter", "\U00002139\U0000FE0F", "\U00002139\U0000FE0F"},
	{"double exclamation mark, filed as punctuation", "\U0000203C\U0000FE0F", "\U0000203C\U0000FE0F"},
	{"exclamation question mark, filed as punctuation", "\U00002049\U0000FE0F", "\U00002049\U0000FE0F"},
	{"part alternation mark, filed as punctuation", "\U0000303D\U0000FE0F", "\U0000303D\U0000FE0F"},
	{"wavy dash, filed as punctuation", "\U00003030\U0000FE0F", "\U00003030\U0000FE0F"},
	{"left right arrow, filed as a math symbol", "\U00002194\U0000FE0F", "\U00002194\U0000FE0F"},
	{"arrow curving up, filed as a math symbol", "\U00002934\U0000FE0F", "\U00002934\U0000FE0F"},
	{"arrow curving down, filed as a math symbol", "\U00002935\U0000FE0F", "\U00002935\U0000FE0F"},
	{"surrounding whitespace is trimmed", " \t👍\n", "👍"},
}

// Each rejected case is a way the field could become a second message body, or is
// not made of the runes an emoji is made of.
var rejectedEmoji = []struct{ name, in string }{
	{"empty", ""},
	{"only whitespace", "  \t \n"},
	{"a letter", "a"},
	{"a word", "hi"},
	{"a short sentence", "ok lol"},
	{"a sentence ending in an emoji", "nice 👍"},
	{"a letter after an emoji", "👍a"},
	{"a digit", "1"},
	{"a number sign", "#"},
	{"an asterisk", "*"},
	{"an emoji and a digit", "👍1"},
	{"a digit before an emoji", "1👍"},
	{"a digit with the selector but no keycap", "1\U0000FE0F"},
	{"a digit with the wrong selector before the keycap", "1\U0000FE0E\U000020E3"},
	{"two digits under one keycap", "12\U000020E3"},
	{"a letter under a keycap", "a\U000020E3"},
	{"a lone keycap", "\U000020E3"},
	{"two emoji with a space between", "👍 👍"},
	{"punctuation", "!"},
	{"an emoji and punctuation", "👍!"},
	{"non-ASCII punctuation", "…"},
	{"a heart made of ASCII", "<3"},
	{"NUL", "\x00"},
	{"a bell", "\a"},
	{"DEL", "\x7f"},
	{"a newline between", "👍\n👍"},
	{"a tab between", "👍\t👍"},
	{"a C1 control between", "👍\u0085👍"},
	{"a no-break space between", "👍\U000000A0👍"},
	{"an ideographic space between", "👍\U00003000👍"},
	{"an em space between", "👍\U00002003👍"},
	{"17 emoji, many clusters", strings.Repeat("👍", 17)},
	{"a ZWJ chain over the rune cap, one cluster", zwjChain(9)},
	{"an accented word", "été"},
	{"CJK text", "日本語"},
	{"kana", "ありがと"},
	{"Cyrillic", "да"},
	{"an Arabic-Indic digit", "٣"},
	{"a circled digit", "①"},
	{"a superscript", "²"},
	{"the replacement character", "\U0000FFFD"},
	{"an emoji and the replacement character", "👍\U0000FFFD"},
	{"a lone ZWJ, which draws nothing", "\U0000200D"},
	{"a lone variation selector, which draws nothing", "\U0000FE0F"},
	{"a lone text selector", "\U0000FE0E"},
	{"only tag characters", "\U000E0067\U000E0062\U000E007F"},
	{"ZWJ and selectors only", "\U0000200D\U0000FE0F\U0000200D"},
	{"a lone combining mark", "\U00000301"},
	{"a combining mark on an emoji", "👍\U00000301"},
	{"a zero-width space", "👍\U0000200B"},
	{"a caret, an ASCII accent filed as Sk", "^"},
	{"four carets", "^^^^"},
	{"a backtick", "`"},
	{"a degree sign, low So", "\U000000B0"},
	{"a macron, Sk", "\U000000AF"},
	{"invalid UTF-8", "\xff\xfe"},
	{"an emoji and invalid UTF-8", "👍\xff"},
}

func TestCleanEmoji(t *testing.T) {
	for _, tc := range acceptedEmoji {
		if got, ok := cleanEmoji(tc.in); !ok || got != tc.want {
			t.Errorf("%s: cleanEmoji(%q) = %q, %v, want %q accepted", tc.name, tc.in, got, ok, tc.want)
		}
	}
	for _, tc := range rejectedEmoji {
		if got, ok := cleanEmoji(tc.in); ok {
			t.Errorf("%s: cleanEmoji(%q) = %q accepted, want it refused", tc.name, tc.in, got)
		}
	}
}

// Every rejected shape is a 400 bad_emoji whichever way it arrives — in the body to
// add, in the path to remove — and it is refused before anything is read or written:
// not a statement, not a frame.
func TestEveryBadEmojiShapeIs400AndTouchesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	e.db.calls = nil

	for _, tc := range rejectedEmoji {
		rec := reactAs(t, e, "u2", "m1", tc.in)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_emoji" {
			t.Errorf("PUT %s: %d %s, want 400 bad_emoji", tc.name, rec.Code, rec.Body)
		}
		if tc.in == "" {
			continue // an empty path segment is a different route, and a 404
		}
		rec = e.unreact("u2", "m1", tc.in)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_emoji" {
			t.Errorf("DELETE %s: %d %s, want 400 bad_emoji", tc.name, rec.Code, rec.Body)
		}
	}
	if rec := e.do("u2", http.MethodPut, "/messages/m1/reactions", "{}"); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_emoji" {
		t.Errorf("a body with no emoji: %d %s, want 400 bad_emoji", rec.Code, rec.Body)
	}
	if len(e.db.calls) != 0 || len(e.ln.pushes) != 0 || len(e.db.reactions) != 0 || e.db.commits != 0 {
		t.Errorf("a refused emoji reached the database (%d statements), pushed %d frames or stored %d rows",
			len(e.db.calls), len(e.ln.pushes), len(e.db.reactions))
	}

	// Not JSON, or the wrong type for the field: a 400, never a 500 or a reaction.
	for _, body := range []string{"", "nope", `{"emoji": 5}`, `{"emoji": ["👍"]}`} {
		if rec := e.do("u2", http.MethodPut, "/messages/m1/reactions", body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %q = %d, want 400", body, rec.Code)
		}
	}

	// A percent-escape that does not decode is as bad an emoji as any other. Routing
	// would never hand one over, so the handler is called directly.
	req := httptest.NewRequest(http.MethodDelete, "/messages/m1/reactions/x", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("messageID", "m1")
	rctx.URLParams.Add("emoji", "%zz")
	req = req.WithContext(auth.WithUserID(context.WithValue(req.Context(), chi.RouteCtxKey, rctx), "u2"))
	rec := httptest.NewRecorder()
	e.h.RemoveReaction(rec, req)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_emoji" {
		t.Errorf("an undecodable escape: %d %s, want 400 bad_emoji", rec.Code, rec.Body)
	}
}

// The emoji in the DELETE path may reach the handler decoded, raw, or encoded in
// either case of hex: all of them name the same emoji.
func TestRemovingAReactionAcceptsTheEmojiInEveryPathSpelling(t *testing.T) {
	for name, tc := range map[string]struct{ segment, emoji string }{
		"literal":            {"👍", "👍"},
		"upper-case hex":     {"%F0%9F%91%8D", "👍"},
		"lower-case hex":     {"%f0%9f%91%8d", "👍"},
		"a skin tone":        {"%F0%9F%91%8D%F0%9F%8F%BD", "👍🏽"},
		"with its selector":  {"%E2%9D%A4%EF%B8%8F", "❤️"},
		"padded with spaces": {"%20%F0%9F%91%8D%20", "👍"},
	} {
		e := newEnv("u1", "u2")
		e.db.befriend("u1", "u2")
		e.db.seed("m1", "u1", "u2", "hi")
		e.db.react("m1", "u2", tc.emoji)

		rec := e.do("u2", http.MethodDelete, "/messages/m1/reactions/"+tc.segment, "")
		if rec.Code != http.StatusOK || reactionsOn(e, "m1") != 0 {
			t.Errorf("%s: DELETE = %d (%s), %d reactions left, want it removed", name, rec.Code, rec.Body, reactionsOn(e, "m1"))
		}
	}
}

// A reaction bumps the MESSAGE's seq — that is the only way an offline client learns
// of it — and tells both people, each with their own `me`.
func TestAReactionDrawsASeqAndPushesToBothSidesEachWithTheirOwnMe(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "react to me")
	e.db.react("m1", "u1", "👍")
	before := e.db.seq
	stored := *e.db.find("m1")

	rec := reactAs(t, e, "u2", "m1", "❤️")
	if rec.Code != http.StatusOK {
		t.Fatalf("react = %d (%s)", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if got.ID != "m1" || got.Seq != before+1 || e.db.seq != before+1 || e.db.find("m1").Seq != before+1 {
		t.Errorf("response seq %d, db seq %d: want exactly one fresh seq, %d", got.Seq, e.db.seq, before+1)
	}
	// Nothing about the words or the place moved.
	if got.Body != stored.Body || !got.CreatedAt.Equal(stored.CreatedAt) || got.EditedAt != nil || got.DeletedAt != nil {
		t.Errorf("a reaction changed the message itself: %+v", got)
	}
	// u2 is the caller, so u2's own is `me`.
	if want := []reactionSummary{{Emoji: "👍", Count: 1, Me: false}, {Emoji: "❤️", Count: 1, Me: true}}; !reflect.DeepEqual(got.Reactions, want) {
		t.Errorf("the response's reactions = %+v, want %+v", got.Reactions, want)
	}

	frames := e.ln.frames(t)
	for user, want := range map[string][]any{
		"u1": {map[string]any{"emoji": "👍", "count": float64(1), "me": true}, map[string]any{"emoji": "❤️", "count": float64(1), "me": false}},
		"u2": {map[string]any{"emoji": "👍", "count": float64(1), "me": false}, map[string]any{"emoji": "❤️", "count": float64(1), "me": true}},
	} {
		f := frames[user]
		if e.ln.pushes[0].kind != "dm" || f["op"] != "react" {
			t.Errorf("%s's frame = %v, want a dm frame with op react", user, f)
		}
		// `from` is the message's author, as for a pin: a reaction is not the author's act.
		if f["from"] != "u1" {
			t.Errorf("%s's frame has from %v, want the author u1", user, f["from"])
		}
		if _, has := f["nonce"]; has {
			t.Errorf("%s's frame carries a nonce: %v", user, f)
		}
		msg := f["message"].(map[string]any)
		if msg["id"] != "m1" || msg["seq"] != float64(before+1) || !reflect.DeepEqual(msg["reactions"], want) {
			t.Errorf("%s's frame message = %v, want seq %d and reactions %v", user, msg, before+1, want)
		}
	}
}

// Either person may react to either person's message, their own included.
func TestEitherParticipantMayReactToEitherMessage(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "u1's")
	e.db.seed("m2", "u2", "u1", "u2's")

	for _, tc := range []struct{ user, id, author string }{
		{"u1", "m1", "u1"}, {"u1", "m2", "u2"}, {"u2", "m1", "u1"}, {"u2", "m2", "u2"},
	} {
		e.ln.pushes = nil
		before := e.db.seq
		rec := reactAs(t, e, tc.user, tc.id, "👍")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s reacting to %s = %d (%s)", tc.user, tc.id, rec.Code, rec.Body)
		}
		if got := reactionsOf(t, rec); len(got) != 1 || got[0].Emoji != "👍" || !got[0].Me {
			t.Errorf("%s reacting to %s: reactions = %+v, want their own", tc.user, tc.id, got)
		}
		p, frame := e.ln.frame(t)
		if !reflect.DeepEqual(sorted(p.users), []string{"u1", "u2"}) || frame["op"] != "react" || frame["from"] != tc.author || e.db.seq != before+1 {
			t.Errorf("%s reacting to %s: push to %v %v, seq %d -> %d", tc.user, tc.id, p.users, frame, before, e.db.seq)
		}
	}
	if got := len(e.db.reactions); got != 4 {
		t.Errorf("rows = %d, want 4", got)
	}
}

// A repeat is not news. Adding what you already added succeeds, draws no seq, pushes
// nothing and never reaches an INSERT — which is what makes a double-tap and a retry
// safe, and also what keeps the table's primary key from ever being tripped.
func TestAReactionYouAlreadyAddedIsASuccessThatWritesAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	first := decode[messageResponse](t, reactAs(t, e, "u2", "m1", "👍"))
	seq, rows := e.db.seq, len(e.db.reactions)
	e.ln.pushes = nil
	e.db.calls = nil
	e.db.commits = 0

	for i := range 3 {
		rec := reactAs(t, e, "u2", "m1", "👍")
		if rec.Code != http.StatusOK {
			t.Fatalf("repeat %d = %d (%s)", i, rec.Code, rec.Body)
		}
		got := decode[messageResponse](t, rec)
		if got.Seq != first.Seq || !reflect.DeepEqual(got.Reactions, []reactionSummary{{Emoji: "👍", Count: 1, Me: true}}) {
			t.Errorf("repeat %d = seq %d %+v, want the message as it was", i, got.Seq, got.Reactions)
		}
	}
	if e.db.seq != seq || e.db.find("m1").Seq != first.Seq || len(e.db.reactions) != rows || len(e.ln.pushes) != 0 {
		t.Errorf("repeats moved seq %d -> %d, rows %d -> %d, or pushed %d frames", seq, e.db.seq, rows, len(e.db.reactions), len(e.ln.pushes))
	}
	if n := len(e.db.callsTo(insertReactionSQL)) + len(e.db.callsTo(touchMessageSQL)); n != 0 {
		t.Errorf("repeats reached a write %d times", n)
	}

	// The same emoji from the OTHER person is not a repeat: it is a second reaction.
	if rec := reactAs(t, e, "u1", "m1", "👍"); rec.Code != http.StatusOK || e.db.seq != seq+1 || len(e.ln.pushes) != 2 {
		t.Errorf("the other person's 👍 = %d, seq %d -> %d, %d pushes, want a new reaction", rec.Code, seq, e.db.seq, len(e.ln.pushes))
	}
	if got := reactionsOf(t, reactAs(t, e, "u1", "m1", "👍")); !reflect.DeepEqual(got, []reactionSummary{{Emoji: "👍", Count: 2, Me: true}}) {
		t.Errorf("two people's 👍 = %+v, want count 2", got)
	}
}

// Removing is the same: it draws a seq and pushes when it removes something, and
// removing what is not there is a quiet success. It can only ever remove YOUR reaction.
func TestRemovingAReactionDrawsASeqAndARepeatDoesNot(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	e.db.react("m1", "u1", "👍")
	e.db.react("m1", "u2", "👍")
	e.db.react("m1", "u2", "❤️")
	before := e.db.seq

	rec := e.unreact("u2", "m1", "👍")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d (%s)", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if got.Seq != before+1 || e.db.seq != before+1 {
		t.Errorf("seq = %d / %d, want one fresh seq past %d", got.Seq, e.db.seq, before)
	}
	// u1's own 👍 survived: you can only remove your own.
	if want := []reactionSummary{{Emoji: "👍", Count: 1, Me: false}, {Emoji: "❤️", Count: 1, Me: true}}; !reflect.DeepEqual(got.Reactions, want) {
		t.Errorf("reactions = %+v, want %+v", got.Reactions, want)
	}
	frames := e.ln.frames(t)
	if frames["u1"]["op"] != "react" || frames["u2"]["op"] != "react" {
		t.Errorf("frames = %v, want op react for both", frames)
	}
	if r := frames["u1"]["message"].(map[string]any)["reactions"].([]any); len(r) != 2 || r[0].(map[string]any)["me"] != true {
		t.Errorf("u1's frame reactions = %v, want their 👍 still theirs", r)
	}

	// Repeats: the one just removed, one never added, one only the other person added.
	e.db.seed("m2", "u2", "u1", "x")
	seq, rows := e.db.seq, len(e.db.reactions)
	e.ln.pushes = nil
	e.db.calls = nil
	for _, c := range []struct{ user, id, emoji string }{
		{"u2", "m1", "👍"}, {"u2", "m1", "🎉"}, {"u2", "m2", "👍"}, {"u1", "m1", "❤️"},
	} {
		if rec := e.unreact(c.user, c.id, c.emoji); rec.Code != http.StatusOK {
			t.Errorf("%s removing %s from %s = %d, want a no-op success", c.user, c.emoji, c.id, rec.Code)
		}
	}
	if e.db.seq != seq || len(e.db.reactions) != rows || len(e.ln.pushes) != 0 {
		t.Errorf("repeats moved seq %d -> %d, rows %d -> %d, or pushed %d", seq, e.db.seq, rows, len(e.db.reactions), len(e.ln.pushes))
	}
	if n := len(e.db.callsTo(removeReactionSQL)) + len(e.db.callsTo(touchMessageSQL)); n != 0 {
		t.Errorf("repeats reached a write %d times", n)
	}
}

// A message holds at most 20 distinct emoji. The 21st is refused, but more people
// using an emoji already there is not a new emoji, and a repeat at the cap is still
// only a success.
func TestAMessageHoldsAtMostTwentyDistinctEmoji(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	emoji := manyEmoji(25)
	for _, em := range emoji[:19] {
		e.db.react("m1", "u1", em)
	}

	if rec := reactAs(t, e, "u2", "m1", emoji[19]); rec.Code != http.StatusOK {
		t.Fatalf("the 20th distinct emoji = %d (%s)", rec.Code, rec.Body)
	}
	seq, rows := e.db.seq, len(e.db.reactions)
	e.ln.pushes = nil
	rec := reactAs(t, e, "u2", "m1", emoji[20])
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "too_many_reactions" {
		t.Errorf("the 21st distinct emoji = %d %s, want 400 too_many_reactions", rec.Code, rec.Body)
	}
	if rec := reactAs(t, e, "u1", "m1", emoji[21]); rec.Code != http.StatusBadRequest {
		t.Errorf("the 21st from the other person = %d, want 400", rec.Code)
	}
	if e.db.seq != seq || len(e.db.reactions) != rows || len(e.ln.pushes) != 0 {
		t.Error("a refused reaction wrote or pushed something")
	}

	// Not a new emoji: someone else using one that is already there.
	if rec := reactAs(t, e, "u2", "m1", emoji[0]); rec.Code != http.StatusOK {
		t.Errorf("a 20-emoji message taking a second person's %s = %d, want 200", emoji[0], rec.Code)
	}
	// A repeat at the cap.
	e.ln.pushes = nil
	if rec := reactAs(t, e, "u2", "m1", emoji[19]); rec.Code != http.StatusOK || len(e.ln.pushes) != 0 {
		t.Errorf("a repeat at the cap = %d with %d pushes, want a quiet 200", rec.Code, len(e.ln.pushes))
	}
	// Taking the only use of one away makes room for a new one.
	if rec := e.unreact("u2", "m1", emoji[19]); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d", rec.Code)
	}
	if rec := reactAs(t, e, "u1", "m1", emoji[20]); rec.Code != http.StatusOK {
		t.Errorf("a new emoji after making room = %d (%s), want 200", rec.Code, rec.Body)
	}
	// The cap is per message.
	e.db.seed("m2", "u2", "u1", "elsewhere")
	if rec := reactAs(t, e, "u1", "m2", emoji[22]); rec.Code != http.StatusOK {
		t.Errorf("a different message = %d, want 200", rec.Code)
	}
}

// A message holds at most 50 reactions across everyone, whatever the emoji.
func TestAMessageHoldsAtMostFiftyReactions(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "popular")
	for i := range 49 {
		e.db.react("m1", "fan"+strconv.Itoa(i), "👍")
	}

	if rec := reactAs(t, e, "u2", "m1", "👍"); rec.Code != http.StatusOK {
		t.Fatalf("the 50th reaction = %d (%s)", rec.Code, rec.Body)
	}
	seq := e.db.seq
	e.ln.pushes = nil
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"the 51st, the same emoji":       reactAs(t, e, "u1", "m1", "👍"),
		"the 51st, a new emoji":          reactAs(t, e, "u1", "m1", "❤️"),
		"the 51st, from the 50th person": reactAs(t, e, "u2", "m1", "🎉"),
	} {
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "too_many_reactions" {
			t.Errorf("%s = %d %s, want 400 too_many_reactions", name, rec.Code, rec.Body)
		}
	}
	if e.db.seq != seq || reactionsOn(e, "m1") != 50 || len(e.ln.pushes) != 0 {
		t.Error("a refused reaction wrote or pushed something")
	}
	// A repeat at the cap is still a success: it is not a new reaction.
	if rec := reactAs(t, e, "u2", "m1", "👍"); rec.Code != http.StatusOK || len(e.ln.pushes) != 0 {
		t.Errorf("a repeat at 50 = %d with %d pushes, want a quiet 200", rec.Code, len(e.ln.pushes))
	}
	// Removing one makes room for exactly one.
	if rec := e.unreact("u2", "m1", "👍"); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d", rec.Code)
	}
	if rec := reactAs(t, e, "u1", "m1", "❤️"); rec.Code != http.StatusOK {
		t.Errorf("a reaction after making room = %d (%s)", rec.Code, rec.Body)
	}
	if rec := reactAs(t, e, "u2", "m1", "🎉"); rec.Code != http.StatusBadRequest {
		t.Errorf("a second one after making room for one = %d, want 400", rec.Code)
	}
	// The cap is per message.
	e.db.seed("m2", "u2", "u1", "quiet")
	if rec := reactAs(t, e, "u1", "m2", "👍"); rec.Code != http.StatusOK {
		t.Errorf("another message = %d, want 200", rec.Code)
	}
}

// You cannot react to a deleted message; and deleting a message takes its
// reactions with it, so there is nothing left to remove either.
func TestAReactionToADeletedMessageIs404(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "bye")
	e.db.react("m1", "u2", "👍")
	e.do("u1", http.MethodDelete, "/messages/m1", "")
	e.ln.pushes = nil
	seq := e.db.seq

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"add":    reactAs(t, e, "u2", "m1", "❤️"),
		"remove": e.unreact("u2", "m1", "👍"),
	} {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s on a deleted message = %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
	}
	if e.db.seq != seq || len(e.db.reactions) != 0 || len(e.ln.pushes) != 0 {
		t.Error("a reaction to a tombstone wrote or pushed something")
	}
}

// Missing, deleted, someone else's conversation, and a conversation whose other
// person is no longer a friend are one answer, byte for byte, and nothing is written.
func TestReactionRefusalsAreOpaque(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	e.db.seed("m1", "u1", "u2", "between u1 and u2")
	e.db.seed("gone", "u1", "u2", "deleted")
	e.db.seed("cold", "u1", "u4", "no longer friends")
	e.db.react("m1", "u1", "👍")
	e.do("u1", http.MethodDelete, "/messages/gone", "")
	e.ln.pushes = nil
	e.db.commits = 0
	seq, rows := e.db.seq, len(e.db.reactions)

	var bodies []string
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a third person who is a friend of both":  reactAs(t, e, "u3", "m1", "👍"),
		"a message that does not exist":           reactAs(t, e, "u1", "nope", "👍"),
		"a deleted message":                       reactAs(t, e, "u1", "gone", "👍"),
		"a former friend":                         reactAs(t, e, "u1", "cold", "👍"),
		"the former friend's side":                reactAs(t, e, "u4", "cold", "👍"),
		"remove by a third person":                e.unreact("u3", "m1", "👍"),
		"remove from a message that is not there": e.unreact("u1", "nope", "👍"),
	} {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s: got %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("refusals differ, so one of them leaks: %q vs %q", b, bodies[0])
		}
	}
	if e.db.commits != 0 || e.db.seq != seq || len(e.db.reactions) != rows || len(e.ln.pushes) != 0 {
		t.Errorf("a refused reaction wrote or pushed: commits=%d seq=%d rows=%d pushes=%d", e.db.commits, e.db.seq, len(e.db.reactions), len(e.ln.pushes))
	}
}

// It runs the way every seq-drawing write does: the lock first, then inside one
// transaction the lookup, the state, the row and the seq; and a failure at any
// statement commits and pushes nothing.
func TestAReactionRunsInsideTheSeqLockAndAFailedOneCommitsAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")

	inTx := func() []string {
		var out []string
		for _, c := range e.db.calls {
			if c.inTx {
				out = append(out, c.sql)
			}
		}
		return out
	}
	e.db.calls = nil
	if rec := reactAs(t, e, "u2", "m1", "👍"); rec.Code != http.StatusOK {
		t.Fatalf("add = %d", rec.Code)
	}
	if want := []string{lockSeqSQL, participantMessageSQL, reactionStateSQL, insertReactionSQL, touchMessageSQL}; !reflect.DeepEqual(inTx(), want) {
		t.Errorf("adding ran %d statements in the transaction in the wrong order, want lock, lookup, state, insert, touch", len(inTx()))
	}
	if e.db.commits != 1 {
		t.Errorf("commits = %d, want 1", e.db.commits)
	}

	e.db.calls = nil
	if rec := e.unreact("u2", "m1", "👍"); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d", rec.Code)
	}
	if want := []string{lockSeqSQL, participantMessageSQL, reactionStateSQL, removeReactionSQL, touchMessageSQL}; !reflect.DeepEqual(inTx(), want) {
		t.Errorf("removing ran %d statements in the transaction in the wrong order, want lock, lookup, state, delete, touch", len(inTx()))
	}

	for _, failing := range []string{reactionStateSQL, insertReactionSQL, touchMessageSQL} {
		e.ln.pushes = nil
		e.db.commits = 0
		e.db.failOnQuery = failing
		if rec := reactAs(t, e, "u1", "m1", "🎉"); rec.Code != http.StatusInternalServerError {
			t.Errorf("add with a failing %.20q = %d, want 500", strings.Join(strings.Fields(failing), " "), rec.Code)
		}
		if e.db.commits != 0 || len(e.ln.pushes) != 0 {
			t.Errorf("a failed add committed (%d) or pushed (%d)", e.db.commits, len(e.ln.pushes))
		}
	}
	e.db.failOnQuery = ""
	e.db.react("m1", "u1", "😀")
	for _, failing := range []string{removeReactionSQL, touchMessageSQL} {
		e.ln.pushes = nil
		e.db.commits = 0
		e.db.failOnQuery = failing
		if rec := e.unreact("u1", "m1", "😀"); rec.Code != http.StatusInternalServerError {
			t.Errorf("remove with a failing %.20q = %d, want 500", strings.Join(strings.Fields(failing), " "), rec.Code)
		}
		if e.db.commits != 0 || len(e.ln.pushes) != 0 {
			t.Errorf("a failed remove committed (%d) or pushed (%d)", e.db.commits, len(e.ln.pushes))
		}
	}
}

// The reaction is stored before its response is built, so a failure to build the
// response is a 500 and nothing is pushed; the retry finds it already there and is a
// quiet success, which is exactly what makes retrying safe.
func TestAReactionWhoseResponseCannotBeBuiltIsRetrySafe(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	e.db.failOnQuery = reactionsSQL

	if rec := reactAs(t, e, "u2", "m1", "👍"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(e.db.reactions) != 1 || len(e.ln.pushes) != 0 {
		t.Fatalf("rows=%d pushes=%d, want the reaction stored and no frame built from a failed read", len(e.db.reactions), len(e.ln.pushes))
	}
	e.db.failOnQuery = ""
	if rec := reactAs(t, e, "u2", "m1", "👍"); rec.Code != http.StatusOK || len(e.db.reactions) != 1 {
		t.Errorf("retry = %d with %d rows, want 200 and still one reaction", rec.Code, len(e.db.reactions))
	}
}

// A reaction reaches a client that was away through the same feed as everything else,
// carrying that reader's own `me` — nothing separate — and it does not move the
// message within the conversation: the feed puts it last, history leaves it first.
func TestAReactionAppearsInTheCatchUpFeedAndNeverMovesTheMessageInTheConversation(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "oldest")
	e.db.seed("m2", "u1", "u2", "newest")
	cursor := decode[page](t, e.do("u2", http.MethodGet, "/messages?since=0", "")).Cursor

	reactAs(t, e, "u1", "m1", "👍")
	feed := decode[page](t, e.do("u2", http.MethodGet, "/messages?since="+strconv.FormatInt(cursor, 10), ""))
	if !reflect.DeepEqual(msgIDs(feed.Messages), []string{"m1"}) ||
		!reflect.DeepEqual(feed.Messages[0].Reactions, []reactionSummary{{Emoji: "👍", Count: 1, Me: false}}) {
		t.Fatalf("after a reaction the feed = %+v, want m1 carrying it, not mine", feed.Messages)
	}
	cursor = feed.Cursor

	e.unreact("u1", "m1", "👍")
	feed = decode[page](t, e.do("u2", http.MethodGet, "/messages?since="+strconv.FormatInt(cursor, 10), ""))
	if !reflect.DeepEqual(msgIDs(feed.Messages), []string{"m1"}) || len(feed.Messages[0].Reactions) != 0 {
		t.Errorf("after the reaction is removed the feed = %+v, want m1 with none", feed.Messages)
	}

	// Conversation order is (created_at, id): m1 is still first.
	hist := decode[historyPage](t, e.do("u2", http.MethodGet, "/messages/u1", ""))
	if !reflect.DeepEqual(msgIDs(hist.Messages), []string{"m1", "m2"}) {
		t.Errorf("history = %v, want m1 still first", msgIDs(hist.Messages))
	}
}

// With nobody listening no frame is built, so a reaction costs one round of derived
// reads: the response's.
func TestAReactionWithNoListenerStillStoresAndBuildsNoFrame(t *testing.T) {
	db := newFakeDB("u1", "u2")
	db.befriend("u1", "u2")
	db.seed("m1", "u1", "u2", "hi")
	e := &env{db: db, ln: &listener{}, h: newHandler(db, db.begin, nil)}

	if rec := reactAs(t, e, "u2", "m1", "👍"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(db.reactions) != 1 || len(db.callsTo(reactionsSQL)) != 1 {
		t.Errorf("rows=%d, reactions read %d times, want the row stored and one read, for the response alone", len(db.reactions), len(db.callsTo(reactionsSQL)))
	}
}

// ---- search -----------------------------------------------------------------

// search asks for q; extra is more query string, already encoded ("&with=u2").
func (e *env) search(user, q, extra string) *httptest.ResponseRecorder {
	return e.do(user, http.MethodGet, "/messages/search?q="+url.QueryEscape(q)+extra, "")
}

func searchIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d (%s)", rec.Code, rec.Body)
	}
	return msgIDs(decode[historyPage](t, rec).Messages)
}

// Search reads across every conversation the caller is in, in both directions, and
// nobody else's; it matches words, not case; and newest comes first.
func TestSearchSpansEveryConversationOfTheCallerAndNobodyElses(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	e.db.seed("a1", "u1", "u2", "dentist on tuesday")
	e.db.seed("a2", "u2", "u1", "Dentist? moving it")
	e.db.seed("b1", "u3", "u1", "dentist again")
	e.db.seed("c1", "u2", "u3", "dentist, and not for u1's eyes")
	e.db.seed("d1", "u1", "u2", "nothing relevant")

	for user, want := range map[string][]string{
		"u1": {"b1", "a2", "a1"}, // two conversations, sent and received, newest first
		"u2": {"c1", "a2", "a1"},
		"u3": {"c1", "b1"},
	} {
		if got := searchIDs(t, e.search(user, "dentist", "")); !reflect.DeepEqual(got, want) {
			t.Errorf("%s found %v, want %v", user, got, want)
		}
	}
	if got := searchIDs(t, e.search("u1", "relevant dentist", "")); len(got) != 0 {
		t.Errorf("a query needing both words found %v, want none", got)
	}

	// The route is not the history route: /messages/search is not a conversation with
	// somebody called "search", so the body is a search result and has no cursor.
	rec := e.search("u1", "dentist", "")
	if strings.Contains(rec.Body.String(), `"cursor"`) || !strings.Contains(rec.Body.String(), `"has_more"`) {
		t.Errorf("body = %s, want {messages, has_more}", rec.Body)
	}
	// Nothing found is an empty list, not null.
	rec = e.search("u1", "zeppelin", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"messages":[]`) {
		t.Errorf("no matches = %d %s, want an empty list", rec.Code, rec.Body)
	}
}

// A search that can reach a thread history refuses to serve is a hole, and the
// catch-up feed already had exactly this bug once.
func TestSearchFindsAFormerFriendsWordsNeverAndAFriendsAlways(t *testing.T) {
	e := newEnv("u1", "u2", "u4")
	e.db.befriend("u1", "u2")
	e.db.seed("ok", "u2", "u1", "a perfectly ordinary message")
	e.db.seed("x1", "u4", "u1", "the xylophone recital")
	e.db.seed("x2", "u1", "u4", "xylophone lessons start friday")

	// Never friends, or no longer: the word exists only in their thread, and is not found.
	if got := searchIDs(t, e.search("u1", "xylophone", "")); len(got) != 0 {
		t.Errorf("a stranger's thread was searched: %v", got)
	}
	// History refuses the same thread, so search must agree.
	if rec := e.do("u1", http.MethodGet, "/messages/u4", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("history with a non-friend = %d, want 404", rec.Code)
	}

	e.db.befriend("u1", "u4")
	if got := searchIDs(t, e.search("u1", "xylophone", "")); !reflect.DeepEqual(got, []string{"x2", "x1"}) {
		t.Errorf("a friend's thread found %v, want [x2 x1]", got)
	}

	// Unfriended again: the thread leaves the results, and returns with the friendship.
	delete(e.db.friends, pair("u1", "u4"))
	if got := searchIDs(t, e.search("u1", "xylophone", "")); len(got) != 0 {
		t.Errorf("a former friend's thread was searched: %v", got)
	}
	// The other side is held to the same rule.
	if got := searchIDs(t, e.search("u4", "xylophone", "")); len(got) != 0 {
		t.Errorf("the former friend searched their own old thread: %v", got)
	}
	e.db.befriend("u1", "u4")
	if got := searchIDs(t, e.search("u1", "xylophone", "")); len(got) != 2 {
		t.Errorf("a restored friendship found %v, want the thread back", got)
	}
}

// A tombstone never matches. Its body is empty in the database, so its vector is too;
// that is checked here through delete, and the query ALSO filters deleted_at, so the
// guarantee does not rest on the body having been cleared.
func TestATombstoneNeverMatchesASearch(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "the secret ingredient is flapjack")
	e.db.seed("m3", "u2", "u1", "flapjack is still here")

	if got := searchIDs(t, e.search("u2", "flapjack", "")); !reflect.DeepEqual(got, []string{"m3", "m1"}) {
		t.Fatalf("before the delete = %v", got)
	}
	if rec := e.do("u1", http.MethodDelete, "/messages/m1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if stored := e.db.find("m1"); stored.Body != "" || stored.DeletedAt == nil {
		t.Fatalf("stored = %+v, want a tombstone with an empty body", stored)
	}
	for _, user := range []string{"u1", "u2"} {
		if got := searchIDs(t, e.search(user, "flapjack", "")); !reflect.DeepEqual(got, []string{"m3"}) {
			t.Errorf("%s after the delete found %v, want only the live message", user, got)
		}
	}

	// A row marked deleted whose words were somehow still there must not match either.
	e.db.seed("m4", "u1", "u2", "flapjack that should have been cleared")
	at := e.db.clock
	e.db.find("m4").DeletedAt = &at
	if got := searchIDs(t, e.search("u2", "flapjack", "")); !reflect.DeepEqual(got, []string{"m3"}) {
		t.Errorf("a deleted row with words found %v, want only the live message", got)
	}
	mustContain(t, "searchMessagesSQL", searchMessagesSQL, "deleted_at IS NULL")
	if n := strings.Count(strings.Join(strings.Fields(searchMessagesSQL), " "), "deleted_at IS NULL"); n != 2 {
		t.Errorf("searchMessagesSQL filters deleted_at %d times, want once in each branch", n)
	}
}

// ?with= narrows to one conversation, and a `with` who is not an accepted friend is
// the opaque 404 history gives.
func TestSearchWithNarrowsToOnePeerAndIsFriendGated(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.seed("a1", "u1", "u2", "lunch with u2")
	e.db.seed("a2", "u2", "u1", "lunch? yes")
	e.db.seed("b1", "u3", "u1", "lunch with u3")
	e.db.seed("z1", "u4", "u1", "lunch from a stranger")

	for with, want := range map[string][]string{
		"u2": {"a2", "a1"},
		"u3": {"b1"},
	} {
		if got := searchIDs(t, e.search("u1", "lunch", "&with="+with)); !reflect.DeepEqual(got, want) {
			t.Errorf("with=%s found %v, want %v", with, got, want)
		}
	}
	if got := searchIDs(t, e.search("u1", "lunch", "")); !reflect.DeepEqual(got, []string{"b1", "a2", "a1"}) {
		t.Errorf("without with= found %v, want both friends and no stranger", got)
	}
	// An empty `with` is no `with`.
	if got := searchIDs(t, e.search("u1", "lunch", "&with=")); len(got) != 3 {
		t.Errorf("with= (empty) found %v, want every conversation", got)
	}

	var bodies []string
	for name, with := range map[string]string{
		"a stranger who has written to you": "u4",
		"a user who does not exist":         "nobody",
		"yourself":                          "u1",
	} {
		rec := e.search("u1", "lunch", "&with="+with)
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
			t.Errorf("with %s = %d %s, want the opaque 404 recipient_not_found", name, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("refusals differ, so one of them leaks: %q vs %q", b, bodies[0])
		}
	}
	// The gate comes before the query is run.
	e.db.calls = nil
	e.search("u1", "lunch", "&with=u4")
	if n := len(e.db.callsTo(searchMessagesSQL)); n != 0 {
		t.Errorf("a refused `with` still ran the search %d times", n)
	}
}

// The keyset: newest first, a page ends where the next begins, nothing repeats or
// skips, and has_more is true exactly when the page filled.
func TestSearchPagesNewestFirstOnCreatedAtAndID(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	for i := 1; i <= 5; i++ {
		from, to := "u1", "u2"
		if i%2 == 0 {
			from, to = to, from
		}
		e.db.seed("m"+strconv.Itoa(i), from, to, "ticket number "+strconv.Itoa(i))
	}
	e.db.seed("noise", "u1", "u2", "unrelated")

	rec := e.search("u1", "ticket", "&limit=2")
	p := decode[historyPage](t, rec)
	if !reflect.DeepEqual(msgIDs(p.Messages), []string{"m5", "m4"}) || !p.HasMore {
		t.Fatalf("page 1 = %v has_more=%v, want [m5 m4] and more", msgIDs(p.Messages), p.HasMore)
	}
	p = decode[historyPage](t, e.search("u1", "ticket", "&limit=2&before=m4"))
	if !reflect.DeepEqual(msgIDs(p.Messages), []string{"m3", "m2"}) || !p.HasMore {
		t.Fatalf("page 2 = %v has_more=%v, want [m3 m2] and more", msgIDs(p.Messages), p.HasMore)
	}
	p = decode[historyPage](t, e.search("u1", "ticket", "&limit=2&before=m2"))
	if !reflect.DeepEqual(msgIDs(p.Messages), []string{"m1"}) || p.HasMore {
		t.Fatalf("page 3 = %v has_more=%v, want [m1] and no more", msgIDs(p.Messages), p.HasMore)
	}
	// A page that fills exactly is "more" — the same rule history follows.
	p = decode[historyPage](t, e.search("u1", "ticket", "&limit=5"))
	if len(p.Messages) != 5 || !p.HasMore {
		t.Errorf("a page that filled = %d messages has_more=%v", len(p.Messages), p.HasMore)
	}
	p = decode[historyPage](t, e.search("u1", "ticket", "&limit=6"))
	if len(p.Messages) != 5 || p.HasMore {
		t.Errorf("a page that did not fill = %d messages has_more=%v", len(p.Messages), p.HasMore)
	}

	// The order is the conversation's, never seq's: editing, reacting to and pinning
	// the OLDEST match gives it the newest seq, and it stays at the bottom.
	e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "ticket number 1, edited"}))
	reactAs(t, e, "u2", "m1", "👍")
	e.pin("u2", "m1")
	if e.db.find("m1").Seq <= e.db.find("m5").Seq {
		t.Fatal("setup: m1 should now have the newest seq")
	}
	if got := searchIDs(t, e.search("u1", "ticket", "")); !reflect.DeepEqual(got, []string{"m5", "m4", "m3", "m2", "m1"}) {
		t.Errorf("after bumping m1's seq the results = %v, want creation order, newest first", got)
	}
	// ...and a page cursor is a place in that order, not in seq.
	if got := searchIDs(t, e.search("u1", "ticket", "&before=m3")); !reflect.DeepEqual(got, []string{"m2", "m1"}) {
		t.Errorf("before=m3 found %v, want [m2 m1]", got)
	}
}

// Ties on created_at are told apart by id, so a page boundary never loses a message.
func TestSearchBreaksTiesOnID(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	at := e.db.clock
	for _, id := range []string{"a", "b", "c", "d"} {
		e.db.seedAt(id, "u1", "u2", "same moment", at)
	}
	var seen []string
	before := ""
	for range 4 {
		extra := "&limit=1"
		if before != "" {
			extra += "&before=" + before
		}
		got := searchIDs(t, e.search("u1", "moment", extra))
		if len(got) != 1 {
			t.Fatalf("page after %q = %v, want one", before, got)
		}
		seen = append(seen, got[0])
		before = got[0]
	}
	if !reflect.DeepEqual(seen, []string{"d", "c", "b", "a"}) {
		t.Errorf("paged %v, want every message once, id descending", seen)
	}
}

// before= names a place in the caller's own messages. Anything else is a 400
// bad_cursor — not an empty page, which would read as "there is nothing older".
func TestSearchBeforeThatNamesNothingIs400(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	e.db.seed("m1", "u1", "u2", "findme")
	e.db.seed("m2", "u1", "u3", "findme too")
	e.db.seed("theirs", "u2", "u3", "findme, not for u1")
	e.db.seed("gone", "u1", "u2", "findme once")
	e.do("u1", http.MethodDelete, "/messages/gone", "")

	for name, extra := range map[string]string{
		"a message that does not exist":            "&before=nope",
		"a message between two other people":       "&before=theirs",
		"a message of another conversation, with=": "&before=m2&with=u2",
		"a message between two others, with=":      "&before=theirs&with=u3",
	} {
		rec := e.search("u1", "findme", extra)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_cursor" {
			t.Errorf("%s: %d %s, want 400 bad_cursor", name, rec.Code, rec.Body)
		}
	}
	// A tombstone is still a place to page from, and so is a message of a former friend.
	if rec := e.search("u1", "findme", "&before=gone"); rec.Code != http.StatusOK {
		t.Errorf("before a tombstone = %d, want 200", rec.Code)
	}
	if got := searchIDs(t, e.search("u1", "findme", "&before=m2&with=u3")); len(got) != 0 {
		t.Errorf("before the only message of the conversation found %v, want none", got)
	}
	// The friendship gate on `with` comes first.
	if rec := e.search("u1", "findme", "&before=nope&with=stranger"); rec.Code != http.StatusNotFound {
		t.Errorf("a bad cursor with a bad `with` = %d, want the 404 first", rec.Code)
	}
}

func TestSearchQueryLengthAndEmptiness(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hello")
	e.db.calls = nil

	for name, target := range map[string]string{
		"no q at all":     "/messages/search",
		"an empty q":      "/messages/search?q=",
		"only spaces":     "/messages/search?q=%20%20%09",
		"only a NUL":      "/messages/search?q=%00",
		"only bad UTF-8":  "/messages/search?q=%FF%FE",
		"spaces and NULs": "/messages/search?q=%20%00%20",
	} {
		rec := e.do("u1", http.MethodGet, target, "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "empty_query" {
			t.Errorf("%s: %d %s, want 400 empty_query", name, rec.Code, rec.Body)
		}
	}
	if n := len(e.db.callsTo(searchMessagesSQL)); n != 0 {
		t.Errorf("an empty query ran the search %d times", n)
	}

	// 1 to 200 characters: counted in characters, not bytes.
	for name, q := range map[string]string{
		"one character":              "h",
		"exactly 200 characters":     strings.Repeat("a", 200),
		"200 multi-byte characters":  strings.Repeat("日", 200),
		"200 after trimming padding": "  " + strings.Repeat("a", 200) + "  ",
	} {
		if rec := e.search("u1", q, ""); rec.Code != http.StatusOK {
			t.Errorf("%s = %d (%s), want 200", name, rec.Code, rec.Body)
		}
	}
	for name, q := range map[string]string{
		"201 characters":            strings.Repeat("a", 201),
		"201 multi-byte characters": strings.Repeat("日", 201),
	} {
		rec := e.search("u1", q, "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "query_too_long" {
			t.Errorf("%s = %d %s, want 400 query_too_long", name, rec.Code, rec.Body)
		}
	}
}

// A person's search box is never a 500. Postgres refuses a NUL byte and invalid UTF-8
// in a text parameter, so those are dropped before it sees them; everything else —
// quotes, dashes, brackets, operators — goes through exactly as typed, to
// websearch_to_tsquery, which is the function that does not throw on them.
func TestAPunctuationHeavyQueryIsAnOrdinarySearch(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "see the dentist appointment")
	e.db.seed("m2", "u2", "u1", "the dentist cancelled")

	for _, q := range []string{
		`!!! ??? ((( ))) -- ' " ~~~ :* & | <-> \`,
		`"unterminated quote`,
		`-`,
		`- - -`,
		`dentist OR`,
		`<@u1>`,
		`'; DROP TABLE messages; --`,
		`dentist:* & !appointment`,
		"emoji 👍 only",
	} {
		rec := e.search("u1", q, "")
		if rec.Code != http.StatusOK {
			t.Errorf("q=%q = %d (%s), want 200", q, rec.Code, rec.Body)
		}
	}

	// What reaches the database is the query as typed, as a parameter and never spliced
	// into the statement; NULs and bad UTF-8 are gone.
	e.db.calls = nil
	e.search("u1", `  "dentist appointment" -cancelled  `, "")
	e.do("u1", http.MethodGet, "/messages/search?q=den%00tist%FF", "")
	calls := e.db.callsTo(searchMessagesSQL)
	if len(calls) != 2 || calls[0].args[1] != `"dentist appointment" -cancelled` || calls[1].args[1] != "dentist" {
		t.Fatalf("the search was run with %+v, want the trimmed query, then the cleaned one", calls)
	}

	// Phrases and exclusions work end to end (as far as the fake models them).
	if got := searchIDs(t, e.search("u1", `"dentist appointment"`, "")); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("a quoted phrase found %v, want [m1]", got)
	}
	if got := searchIDs(t, e.search("u1", `dentist -cancelled`, "")); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("an exclusion found %v, want [m1]", got)
	}
	// A query with no words in it is an empty search, not an error.
	if got := searchIDs(t, e.search("u1", `!!! ???`, "")); len(got) != 0 {
		t.Errorf("a query of punctuation found %v, want none", got)
	}
}

func TestSearchLimitIsClamped(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 50}, {"0", 50}, {"-3", 50}, {"abc", 50}, {"7", 7}, {"100", 100}, {"101", 100}, {"9999", 100},
	} {
		e.db.calls = nil
		target := "/messages/search?q=x"
		if tc.raw != "" {
			target += "&limit=" + tc.raw
		}
		if rec := e.do("u1", http.MethodGet, target, ""); rec.Code != http.StatusOK {
			t.Fatalf("limit=%q = %d", tc.raw, rec.Code)
		}
		calls := e.db.callsTo(searchMessagesSQL)
		if len(calls) != 1 || calls[0].args[5] != tc.want {
			t.Errorf("limit=%q ran with %v, want %d", tc.raw, calls, tc.want)
		}
	}
}

// Each result is a whole message: its reply, its reactions as THIS reader sees them,
// its mentions and its pin, so a result row can show them.
func TestSearchResultsAreFullMessages(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.seed("orig", "u2", "u1", "the original thing")
	reply := decode[messageResponse](t, sendAs(t, e, "u1", "u2", "answering with a pomegranate <@u2> and <@u3> and <@stranger>", map[string]any{"reply_to_id": "orig"}))
	e.db.react(reply.ID, "u2", "👍")
	e.db.react(reply.ID, "u1", "❤️")
	e.pin("u2", reply.ID)

	for user, wantReactions := range map[string][]reactionSummary{
		"u1": {{Emoji: "👍", Count: 1, Me: false}, {Emoji: "❤️", Count: 1, Me: true}},
		"u2": {{Emoji: "👍", Count: 1, Me: true}, {Emoji: "❤️", Count: 1, Me: false}},
	} {
		rec := e.search(user, "pomegranate", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s search = %d (%s)", user, rec.Code, rec.Body)
		}
		got := decode[historyPage](t, rec).Messages
		if len(got) != 1 {
			t.Fatalf("%s found %d, want 1", user, len(got))
		}
		m := got[0]
		if m.ReplyTo == nil || m.ReplyTo.ID != "orig" || m.ReplyTo.Body != "the original thing" || m.ReplyTo.Deleted {
			t.Errorf("%s: reply_to = %+v, want the original", user, m.ReplyTo)
		}
		if !reflect.DeepEqual(m.Reactions, wantReactions) {
			t.Errorf("%s: reactions = %+v, want %+v", user, m.Reactions, wantReactions)
		}
		if m.PinnedAt == nil || m.Seq == 0 {
			t.Errorf("%s: pinned_at=%v seq=%d, want the pin and a seq", user, m.PinnedAt, m.Seq)
		}
		// u1 reads as the reader, u2 and u3 as friends, the stranger as plain text.
		wantMentions := []string{"u2", "u3"}
		if user == "u2" {
			// u2 is not friends with u3 or the stranger; only themself counts.
			wantMentions = []string{"u2"}
		}
		if !reflect.DeepEqual(m.Mentions, wantMentions) {
			t.Errorf("%s: mentions = %v, want %v", user, m.Mentions, wantMentions)
		}
	}
}

// Search only reads: no lock, no transaction, no seq, no frame.
func TestSearchWritesNothingAndPushesNothing(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hello there")
	seq := e.db.seq
	e.db.calls = nil

	if rec := e.search("u1", "hello", ""); rec.Code != http.StatusOK {
		t.Fatalf("search = %d", rec.Code)
	}
	for _, c := range e.db.calls {
		if c.inTx || c.sql == lockSeqSQL {
			t.Errorf("search ran %.40q inside a transaction", strings.Join(strings.Fields(c.sql), " "))
		}
	}
	if e.db.seq != seq || e.db.commits != 0 || len(e.ln.pushes) != 0 {
		t.Errorf("search moved seq %d -> %d, committed %d, pushed %d", seq, e.db.seq, e.db.commits, len(e.ln.pushes))
	}
}

// Every statement a search leans on can fail, and each failure is a 500 and never a
// half-built result.
func TestSearchFailsCleanly(t *testing.T) {
	for _, tc := range []struct {
		name, failing, extra string
	}{
		{"the search", searchMessagesSQL, ""},
		{"the search, narrowed", searchMessagesSQL, "&with=u2"},
		{"the derived reads", reactionsSQL, ""},
		{"the cursor of every conversation", inboxCursorSQL, "&before=m1"},
		{"the cursor of one conversation", historyCursorSQL, "&before=m1&with=u2"},
	} {
		e := newEnv("u1", "u2")
		e.db.befriend("u1", "u2")
		e.db.seed("m1", "u1", "u2", "hello")
		e.db.seed("m2", "u2", "u1", "hello again")
		e.db.failOnQuery = tc.failing
		if rec := e.search("u1", "hello", tc.extra); rec.Code != http.StatusInternalServerError {
			t.Errorf("a failing %s = %d (%s), want 500", tc.name, rec.Code, rec.Body)
		}
	}
}

// ---- reactions: unfriending, and every accepted shape on the wire -------------

// Adding is aimed at somebody and needs the friendship; removing is taking back your
// own mark and does not, for the reason Delete does not. Either way the caller must
// be a party to the message, and the refusals stay one opaque answer.
func TestAFormerFriendCanRemoveTheirOwnReactionButNotAddOne(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "from u1")
	e.db.react("m1", "u1", "👍")
	e.db.react("m1", "u2", "👍")
	e.db.react("m1", "u2", "❤️")
	delete(e.db.friends, pair("u1", "u2")) // unfriended, or blocked
	e.ln.pushes = nil
	seq := e.db.seq

	// Adding is refused, opaquely, and writes nothing.
	rec := reactAs(t, e, "u2", "m1", "🎉")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
		t.Errorf("adding after the friendship ended = %d %s, want 404 message_not_found", rec.Code, rec.Body)
	}
	if e.db.seq != seq || len(e.db.reactions) != 3 || len(e.ln.pushes) != 0 {
		t.Fatal("a refused add wrote or pushed something")
	}

	// Removing their own works, from either side, and only their own is removed.
	rec = e.unreact("u2", "m1", "👍")
	if rec.Code != http.StatusOK {
		t.Fatalf("removing after the friendship ended = %d (%s), want 200", rec.Code, rec.Body)
	}
	if want := []reactionSummary{{Emoji: "👍", Count: 1, Me: false}, {Emoji: "❤️", Count: 1, Me: true}}; !reflect.DeepEqual(reactionsOf(t, rec), want) {
		t.Errorf("reactions = %+v, want only u2's 👍 gone: %+v", reactionsOf(t, rec), want)
	}
	if e.db.seq != seq+1 {
		t.Errorf("seq = %d, want one fresh seq past %d", e.db.seq, seq)
	}
	// Both people are told: the other has no other way to see it go, because a thread
	// with a former friend is not in their catch-up feed.
	frames := e.ln.frames(t)
	if frames["u1"]["op"] != "react" || frames["u2"]["op"] != "react" {
		t.Errorf("frames = %v, want op react for both", frames)
	}
	if rec := e.unreact("u1", "m1", "👍"); rec.Code != http.StatusOK || reactionsOn(e, "m1") != 1 {
		t.Errorf("the other side removing theirs = %d, %d reactions left, want 1", rec.Code, reactionsOn(e, "m1"))
	}
	// A repeat is still a quiet success.
	e.ln.pushes = nil
	seq = e.db.seq
	if rec := e.unreact("u2", "m1", "👍"); rec.Code != http.StatusOK || e.db.seq != seq || len(e.ln.pushes) != 0 {
		t.Errorf("repeat removal = %d, seq %d -> %d, %d pushes, want a quiet 200", rec.Code, seq, e.db.seq, len(e.ln.pushes))
	}

	// Not a party: still the opaque 404, friend of both or not, and no probing by id.
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	var bodies []string
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a third person":        e.unreact("u3", "m1", "❤️"),
		"a message that is not": e.unreact("u2", "nope", "❤️"),
		"a third person's add":  reactAs(t, e, "u3", "m1", "❤️"),
		"a former friend's add": reactAs(t, e, "u1", "m1", "❤️"),
		"a deleted message": func() *httptest.ResponseRecorder {
			e.do("u1", http.MethodDelete, "/messages/m1", "")
			return e.unreact("u2", "m1", "❤️")
		}(),
	} {
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
			t.Errorf("%s: %d %s, want 404 message_not_found", name, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("refusals differ, so one of them leaks: %q vs %q", b, bodies[0])
		}
	}
}

// The gate is one definition: the same statements, in the same order, for a pin, an
// add and a removal that needs the friendship, and the removal skips only the
// friendship check.
func TestPinsAndReactionsShareOneGate(t *testing.T) {
	statements := func(do func(e *env) *httptest.ResponseRecorder) []string {
		e := newEnv("u1", "u2")
		e.db.befriend("u1", "u2")
		e.db.seed("m1", "u1", "u2", "hi")
		e.db.react("m1", "u2", "👍")
		e.db.calls = nil
		if rec := do(e); rec.Code != http.StatusOK {
			t.Fatalf("setup request = %d (%s)", rec.Code, rec.Body)
		}
		var out []string
		for _, c := range e.db.calls {
			switch c.sql {
			case participantMessageSQL, userExistsSQL, areFriendsSQL:
				out = append(out, c.sql)
			}
		}
		return out
	}
	gated := []string{participantMessageSQL, userExistsSQL, areFriendsSQL, participantMessageSQL}
	for name, got := range map[string][]string{
		"pin": statements(func(e *env) *httptest.ResponseRecorder { return e.pin("u2", "m1") }),
		"add": statements(func(e *env) *httptest.ResponseRecorder { return reactAs(t, e, "u1", "m1", "👍") }),
	} {
		if !reflect.DeepEqual(got, gated) {
			t.Errorf("%s ran %d gate statements %v, want the participant lookup, then the friendship, then the write's own lookup", name, len(got), got)
		}
	}
	if got, want := statements(func(e *env) *httptest.ResponseRecorder { return e.unreact("u2", "m1", "👍") }),
		[]string{participantMessageSQL, participantMessageSQL}; !reflect.DeepEqual(got, want) {
		t.Errorf("removal ran %v, want the participant lookup and nothing about friendship", got)
	}
}

// Every accepted shape survives the whole request path: the JSON body, the trim, the
// stored row, and then the same emoji as a percent-encoded path segment (a `#` is
// a fragment and `*` is escaped by Go but not by browsers' encodeURIComponent, so
// both spellings are tried).
func TestEveryAcceptedEmojiShapeCanBeAddedAndRemovedOverHTTP(t *testing.T) {
	for _, tc := range acceptedEmoji {
		for _, spelling := range []string{url.PathEscape(tc.want), strings.ReplaceAll(url.PathEscape(tc.want), "%2A", "*")} {
			e := newEnv("u1", "u2")
			e.db.befriend("u1", "u2")
			e.db.seed("m1", "u1", "u2", "hi")

			rec := reactAs(t, e, "u2", "m1", tc.in)
			if rec.Code != http.StatusOK {
				t.Errorf("%s: add = %d (%s)", tc.name, rec.Code, rec.Body)
				continue
			}
			if got := reactionsOf(t, rec); len(got) != 1 || got[0].Emoji != tc.want || !got[0].Me {
				t.Errorf("%s: stored as %+v, want %q", tc.name, got, tc.want)
			}
			rec = e.do("u2", http.MethodDelete, "/messages/m1/reactions/"+spelling, "")
			if rec.Code != http.StatusOK || reactionsOn(e, "m1") != 0 {
				t.Errorf("%s: remove via %q = %d (%s), %d reactions left", tc.name, spelling, rec.Code, rec.Body, reactionsOn(e, "m1"))
			}
		}
	}
}

// ---- NUL in text -------------------------------------------------------------

// Postgres refuses NUL in a text column, so a body with one used to be a 500 on send
// and on edit. NUL is dropped before the trim and the length, so text made of nothing
// else is empty, and a NUL among real words is just the words.
func TestCleanBodyDropsNULBeforeTrimmingAndCounting(t *testing.T) {
	for _, tc := range []struct{ name, raw, wantBody, wantCode string }{
		{"only NULs", "\x00\x00\x00", "", errEmptyBody},
		{"NULs and spaces", " \x00 \x00 ", "", errEmptyBody},
		{"a NUL among words", "he\x00llo wor\x00ld", "hello world", ""},
		{"a NUL in the padding", "\x00  hi  \x00", "hi", ""},
		{"NULs do not count towards the length", strings.Repeat("a", maxBody) + strings.Repeat("\x00", 50), strings.Repeat("a", maxBody), ""},
		{"one past the limit is still too long", strings.Repeat("a", maxBody+1) + "\x00", "", errBodyTooLong},
	} {
		body, code := cleanBody(tc.raw)
		if body != tc.wantBody || code != tc.wantCode {
			t.Errorf("%s: cleanBody = (%.12q, %q), want (%.12q, %q)", tc.name, body, code, tc.wantBody, tc.wantCode)
		}
	}
}

func TestANULBodyIsNeverA500OnSendOrEdit(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "original")
	rows, seq := len(e.db.msgs), e.db.seq
	e.ln.pushes = nil

	// Nothing but NUL: empty_body, and nothing stored, pushed or sequenced.
	rec := sendAs(t, e, "u1", "u2", "\x00\x00", nil)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "empty_body" {
		t.Errorf("a NUL-only send = %d %s, want 400 empty_body", rec.Code, rec.Body)
	}
	rec = e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "\x00"}))
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "empty_body" {
		t.Errorf("a NUL-only edit = %d %s, want 400 empty_body", rec.Code, rec.Body)
	}
	if len(e.db.msgs) != rows || e.db.seq != seq || e.db.find("m1").Body != "original" || len(e.ln.pushes) != 0 {
		t.Error("a NUL-only body stored, edited or pushed something")
	}

	// A NUL among real words: the words are stored, and no NUL ever reaches a statement.
	rec = sendAs(t, e, "u1", "u2", "good\x00bye", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a send with a NUL in it = %d (%s), want 201", rec.Code, rec.Body)
	}
	if got := decode[messageResponse](t, rec).Body; got != "goodbye" {
		t.Errorf("stored body = %q, want %q", got, "goodbye")
	}
	rec = e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "orig\x00inal, edited"}))
	if rec.Code != http.StatusOK || decode[messageResponse](t, rec).Body != "original, edited" {
		t.Errorf("an edit with a NUL in it = %d (%s), want 200 and the words", rec.Code, rec.Body)
	}
	for _, c := range e.db.calls {
		for _, a := range c.args {
			if s, ok := a.(string); ok && strings.ContainsRune(s, 0) {
				t.Fatalf("a NUL reached %.40q as %q", strings.Join(strings.Fields(c.sql), " "), s)
			}
		}
	}

	// The other free-text fields of a send: a NUL in the nonce or the reply target.
	rec = sendAs(t, e, "u1", "u2", "with a nonce", map[string]any{"nonce": "n\x00-1", "reply_to_id": "m1\x00"})
	if rec.Code != http.StatusCreated {
		t.Errorf("a send with NULs in nonce and reply_to_id = %d (%s), want 201", rec.Code, rec.Body)
	}
	for _, c := range e.db.calls {
		for _, a := range c.args {
			if s, ok := a.(string); ok && strings.ContainsRune(s, 0) {
				t.Fatalf("a NUL reached %.40q as %q", strings.Join(strings.Fields(c.sql), " "), s)
			}
		}
	}
}

// ---- the emoji the categories refuse ------------------------------------------

// The ten real emoji the symbol class would refuse are allowed by name, with and
// without the emoji selector, and the allowance reaches no further: not Sm as a
// whole, which would admit the ordinary math symbols.
func TestTheEmojiOutsideTheSymbolRuleAreAllowedByNameAndNoFurther(t *testing.T) {
	if len(emojiOutsideSymbols) != 10 {
		t.Errorf("emojiOutsideSymbols has %d entries, want the 10 found so far", len(emojiOutsideSymbols))
	}
	for r, what := range emojiOutsideSymbols {
		if isEmojiSymbol(r) {
			t.Errorf("%U (%s) is already accepted by the symbol class, so it does not belong in the list", r, what)
		}
		bare, withSelector := string(r), string(r)+string(runeVS16)
		for _, in := range []string{bare, withSelector} {
			if got, ok := cleanEmoji(in); !ok || got != in {
				t.Errorf("%U %s: cleanEmoji(%q) = %q, %v, want it accepted", r, what, in, got, ok)
			}
		}
	}
	// The eight outside So, and the two below Latin-1.
	for _, r := range []rune{0x2139, 0x203C, 0x2049, 0x303D, 0x3030, 0x2194, 0x2934, 0x2935, 0xA9, 0xAE} {
		if _, ok := emojiOutsideSymbols[r]; !ok {
			t.Errorf("%U is missing from emojiOutsideSymbols", r)
		}
	}
	// Neighbours the list must not have let in: other math symbols, other punctuation,
	// other letters, and the ASCII symbols that are in the Sm category.
	for name, in := range map[string]string{
		"a plus-minus sign":         "\U000000B1",
		"a multiplication sign":     "\U000000D7",
		"a summation sign":          "\U00002211",
		"a not-equal sign":          "\U00002260",
		"an equals sign":            "=",
		"a plus sign":               "+",
		"a vertical bar":            "|",
		"a tilde":                   "~",
		"an ellipsis":               "\U00002026",
		"a different dash":          "\U00002014",
		"a modifier letter":         "\U000002B0",
		"a double question mark":    "\U00002047",
		"an allowed one and a word": "\U0000203Cok",
	} {
		if got, ok := cleanEmoji(in); ok {
			t.Errorf("%s: cleanEmoji(%q) = %q accepted, want it refused", name, in, got)
		}
	}
}

// Sk is not a class the rule accepts, and So counts only above Latin-1. ^ and ` are Sk
// (once ^^^^ was a valid reaction), ¨ ¯ ´ ¸ are Sk, and ° and ¦ are So but are
// typography. A lone one is a single cluster, so it is the character clause and the
// Latin-1 floor that refuse it; the repeated forms are also several clusters.
func TestAccentsAndLowTypographyAreNotEmoji(t *testing.T) {
	for name, in := range map[string]string{
		"a caret":                "^",
		"a backtick":             "`",
		"a degree sign":          "\U000000B0",
		"a diaeresis":            "\U000000A8",
		"a macron":               "\U000000AF",
		"an acute accent":        "\U000000B4",
		"a cedilla":              "\U000000B8",
		"a broken bar":           "\U000000A6",
		"four carets":            "^^^^",
		"carets and backticks":   "^`^`",
		"a caret after an emoji": "\U0001F44D^",
		"all the low symbols":    "\U000000A8\U000000AF\U000000B4\U000000B8",
	} {
		if got, ok := cleanEmoji(in); ok {
			t.Errorf("%s: cleanEmoji(%q) = %q accepted, want it refused", name, in, got)
		}
	}
}

// What stays: both signs below the floor by name, every skin tone, and a skin tone
// on an emoji.
func TestCopyrightRegisteredAndEverySkinToneStillPass(t *testing.T) {
	for name, in := range map[string]string{
		"copyright":            "\U000000A9",
		"copyright, selector":  "\U000000A9\U0000FE0F",
		"registered":           "\U000000AE",
		"registered, selector": "\U000000AE\U0000FE0F",
	} {
		if got, ok := cleanEmoji(in); !ok || got != in {
			t.Errorf("%s: cleanEmoji(%q) = %q, %v, want it accepted", name, in, got, ok)
		}
	}
	for tone := skinToneFirst; tone <= skinToneLast; tone++ {
		for _, in := range []string{string(tone), "\U0001F44D" + string(tone), "\U0001F9D1" + string(tone) + string(runeZWJ) + "\U0001F9B0"} {
			if got, ok := cleanEmoji(in); !ok || got != in {
				t.Errorf("skin tone %U: cleanEmoji(%q) = %q, %v, want it accepted", tone, in, got, ok)
			}
		}
	}
	if skinToneLast-skinToneFirst != 4 {
		t.Errorf("the skin-tone range spans %d code points, want 5", skinToneLast-skinToneFirst+1)
	}
}

// ---- NUL in the URL ------------------------------------------------------------

// nulFixture is a small conversation every case below runs against afresh, so a case
// that writes (a pin, an edit) can be compared with its clean spelling exactly.
func nulFixture() *env {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "first needle")
	e.db.seed("m2", "u2", "u1", "second needle")
	e.db.seed("m3", "u1", "u2", "third needle")
	return e
}

// reachedAStatement reports a NUL found in any argument a statement was run with.
func reachedAStatement(e *env) (string, bool) {
	for _, c := range e.db.calls {
		for _, a := range c.args {
			switch v := a.(type) {
			case string:
				if strings.ContainsRune(v, 0) {
					return strings.Join(strings.Fields(c.sql), " "), true
				}
			case []string:
				for _, s := range v {
					if strings.ContainsRune(s, 0) {
						return strings.Join(strings.Fields(c.sql), " "), true
					}
				}
			}
		}
	}
	return "", false
}

// An id or cursor with a NUL in it is that value without the NUL, and is judged by
// exactly the rules of the clean spelling: same status, same body, same effect.
func TestANULInsideAnIDOrCursorBehavesAsItsCleanSpelling(t *testing.T) {
	react := `{"emoji":"👍"}`
	edit := `{"body":"edited"}`
	thumb := url.PathEscape("👍")
	for _, tc := range []struct{ name, method, nul, clean, body string }{
		{"history: userID", "GET", "/messages/u%002", "/messages/u2", ""},
		{"pins: userID", "GET", "/messages/u%002/pins", "/messages/u2/pins", ""},
		{"read: userID", "POST", "/messages/u%002/read", "/messages/u2/read", ""},
		{"typing: userID", "POST", "/messages/u%002/typing", "/messages/u2/typing", ""},
		{"edit: messageID", "PATCH", "/messages/m%001", "/messages/m1", edit},
		{"delete: messageID", "DELETE", "/messages/m%001", "/messages/m1", ""},
		{"pin: messageID", "PUT", "/messages/m%001/pin", "/messages/m1/pin", ""},
		{"unpin: messageID", "DELETE", "/messages/m%001/pin", "/messages/m1/pin", ""},
		{"react: messageID", "PUT", "/messages/m%001/reactions", "/messages/m1/reactions", react},
		{"unreact: messageID", "DELETE", "/messages/m%001/reactions/" + thumb, "/messages/m1/reactions/" + thumb, ""},
		{"history: before", "GET", "/messages/u2?before=m%002", "/messages/u2?before=m2", ""},
		{"history: around", "GET", "/messages/u2?around=m%002&limit=2", "/messages/u2?around=m2&limit=2", ""},
		{"conversations: before", "GET", "/conversations?before=m%003", "/conversations?before=m3", ""},
		{"search: before", "GET", "/messages/search?q=needle&before=m%003", "/messages/search?q=needle&before=m3", ""},
		{"search: with", "GET", "/messages/search?q=needle&with=u%002", "/messages/search?q=needle&with=u2", ""},
		{"search: q", "GET", "/messages/search?q=nee%00dle", "/messages/search?q=needle", ""},
		{"catch-up: since", "GET", "/messages?since=%001", "/messages?since=1", ""},
		{"history: limit", "GET", "/messages/u2?limit=1%00", "/messages/u2?limit=1", ""},
		{"search: limit", "GET", "/messages/search?q=needle&limit=2%00", "/messages/search?q=needle&limit=2", ""},
	} {
		a, b := nulFixture(), nulFixture()
		withNUL, clean := a.do("u1", tc.method, tc.nul, tc.body), b.do("u1", tc.method, tc.clean, tc.body)
		if withNUL.Code >= 500 {
			t.Errorf("%s: a NUL in the URL is a %d (%s)", tc.name, withNUL.Code, withNUL.Body)
		}
		if withNUL.Code != clean.Code || withNUL.Body.String() != clean.Body.String() {
			t.Errorf("%s: with a NUL = %d %s, but the clean spelling = %d %s", tc.name, withNUL.Code, withNUL.Body, clean.Code, clean.Body)
		}
		if !reflect.DeepEqual(a.db.msgs, b.db.msgs) || !reflect.DeepEqual(a.db.reactions, b.db.reactions) || len(a.ln.pushes) != len(b.ln.pushes) {
			t.Errorf("%s: the NUL spelling left the database or the frames different from the clean one", tc.name)
		}
		if sql, found := reachedAStatement(a); found {
			t.Errorf("%s: a NUL reached %.60q", tc.name, sql)
		}
	}
}

// A parameter of nothing but NUL is as empty as a missing one: it is not a different
// error, and it is not a read of something else.
func TestAnIDOrCursorOfOnlyNULIsExactlyAsEmptyAsAMissingOne(t *testing.T) {
	react := `{"emoji":"👍"}`
	thumb := url.PathEscape("👍")
	for _, tc := range []struct {
		name, method, target, body string
		wantCode                   int
		wantError                  string
	}{
		{"history: userID", "GET", "/messages/%00", "", 404, "recipient_not_found"},
		{"pins: userID", "GET", "/messages/%00/pins", "", 404, "recipient_not_found"},
		{"read: userID", "POST", "/messages/%00/read", "", 400, "invalid conversation"},
		{"typing: userID", "POST", "/messages/%00/typing", "", 404, "recipient_not_found"},
		{"edit: messageID", "PATCH", "/messages/%00", `{"body":"x"}`, 404, "message_not_found"},
		{"delete: messageID", "DELETE", "/messages/%00", "", 404, "message_not_found"},
		{"pin: messageID", "PUT", "/messages/%00/pin", "", 404, "message_not_found"},
		{"unpin: messageID", "DELETE", "/messages/%00/pin", "", 404, "message_not_found"},
		{"react: messageID", "PUT", "/messages/%00/reactions", react, 404, "message_not_found"},
		{"unreact: messageID", "DELETE", "/messages/%00/reactions/" + thumb, "", 404, "message_not_found"},
		{"search: with, only a NUL, is no `with`", "GET", "/messages/search?q=needle&with=%00", "", 200, ""},
	} {
		e := nulFixture()
		rec := e.do("u1", tc.method, tc.target, tc.body)
		if rec.Code != tc.wantCode || (tc.wantError != "" && errorCode(t, rec) != tc.wantError) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, rec.Code, rec.Body, tc.wantCode, tc.wantError)
		}
		if tc.wantCode != 200 && (len(e.db.msgs) != 3 || len(e.db.reactions) != 0 || len(e.ln.pushes) != 0 || e.db.find("m1").PinnedAt != nil) {
			t.Errorf("%s: a refused request changed or pushed something", tc.name)
		}
		if sql, found := reachedAStatement(e); found {
			t.Errorf("%s: a NUL reached %.60q", tc.name, sql)
		}
	}

	// Cursors, `with`, since and limit that are only NUL are the parameter left out.
	for _, tc := range []struct{ name, nul, absent string }{
		{"history: before", "/messages/u2?before=%00", "/messages/u2"},
		{"history: limit", "/messages/u2?limit=%00", "/messages/u2"},
		{"conversations: before", "/conversations?before=%00", "/conversations"},
		{"search: before", "/messages/search?q=needle&before=%00", "/messages/search?q=needle"},
		{"search: with", "/messages/search?q=needle&with=%00", "/messages/search?q=needle"},
		{"search: limit", "/messages/search?q=needle&limit=%00", "/messages/search?q=needle"},
		{"catch-up: since", "/messages?since=%00", "/messages"},
		{"catch-up: limit", "/messages?limit=%00", "/messages"},
	} {
		a, b := nulFixture(), nulFixture()
		withNUL, absent := a.do("u1", "GET", tc.nul, ""), b.do("u1", "GET", tc.absent, "")
		if withNUL.Code != http.StatusOK || withNUL.Code != absent.Code || withNUL.Body.String() != absent.Body.String() {
			t.Errorf("%s: only a NUL = %d %s, but the parameter left out = %d %s", tc.name, withNUL.Code, withNUL.Body, absent.Code, absent.Body)
		}
		if sql, found := reachedAStatement(a); found {
			t.Errorf("%s: a NUL reached %.60q", tc.name, sql)
		}
	}
}

// A NUL cannot be used to get past a check that compares ids: yourself is yourself
// whatever is wedged into the id.
func TestANULCannotHideYourselfFromTheSelfChecks(t *testing.T) {
	e := nulFixture()
	for name, target := range map[string]string{
		"history": "/messages/u%001",
		"pins":    "/messages/u%001/pins",
		"typing":  "/messages/u%001/typing",
		"search":  "/messages/search?q=needle&with=u%001",
	} {
		method := "GET"
		if name == "typing" {
			method = "POST"
		}
		if rec := e.do("u1", method, target, ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
			t.Errorf("%s with yourself spelled with a NUL: %d %s, want the opaque 404", name, rec.Code, rec.Body)
		}
	}
	if rec := e.do("u1", "POST", "/messages/u%001/read", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("read with yourself spelled with a NUL = %d, want 400", rec.Code)
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("pushed %d frames", len(e.ln.pushes))
	}
}

// ---- history around a message ------------------------------------------------

type aroundPage struct {
	Messages []messageResponse `json:"messages"`
	Older    bool              `json:"has_more_older"`
	Newer    bool              `json:"has_more_newer"`
}

func (e *env) around(user, peer, id, extra string) *httptest.ResponseRecorder {
	return e.do(user, http.MethodGet, "/messages/"+peer+"?around="+id+extra, "")
}

func aroundOf(t *testing.T, rec *httptest.ResponseRecorder) aroundPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("around = %d (%s)", rec.Code, rec.Body)
	}
	return decode[aroundPage](t, rec)
}

// A page centred mid-conversation: the older half including the anchor, the newer
// half, ascending, each direction saying whether there is more.
func TestAroundGivesAPageCentredOnTheMessage(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 10)

	for _, tc := range []struct {
		name, extra  string
		want         []string
		older, newer bool
	}{
		{"an even limit splits evenly", "&limit=4", []string{"m4", "m5", "m6", "m7"}, true, true},
		{"an odd limit gives the extra place to the older side", "&limit=5", []string{"m3", "m4", "m5", "m6", "m7"}, true, true},
		{"a limit of one is the message alone", "&limit=1", []string{"m5"}, true, true},
		{"a limit of two is the message and the one after", "&limit=2", []string{"m5", "m6"}, true, true},
		{"the default limit holds the whole conversation", "", []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10"}, false, false},
		{"a limit that exactly fits the conversation is not more", "&limit=10", []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10"}, false, false},
		{"one short of fitting leaves the newest out", "&limit=9", []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9"}, false, true},
	} {
		got := aroundOf(t, e.around("u1", "u2", "m5", tc.extra))
		if !reflect.DeepEqual(msgIDs(got.Messages), tc.want) || got.Older != tc.older || got.Newer != tc.newer {
			t.Errorf("%s: got %v older=%v newer=%v, want %v older=%v newer=%v", tc.name, msgIDs(got.Messages), got.Older, got.Newer, tc.want, tc.older, tc.newer)
		}
	}
	// Either person asks and gets the same page.
	if got := aroundOf(t, e.around("u2", "u1", "m5", "&limit=4")); !reflect.DeepEqual(msgIDs(got.Messages), []string{"m4", "m5", "m6", "m7"}) {
		t.Errorf("the other person got %v", msgIDs(got.Messages))
	}
}

// The ends of the conversation: nothing newer past the newest, nothing older past the
// oldest, and the page is short on that side rather than refilled from the other.
func TestAroundAtEitherEndOfTheConversation(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 10)

	got := aroundOf(t, e.around("u1", "u2", "m10", "&limit=4"))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m9", "m10"}) || !got.Older || got.Newer {
		t.Errorf("around the newest: %v older=%v newer=%v, want [m9 m10], more older, nothing newer", msgIDs(got.Messages), got.Older, got.Newer)
	}
	got = aroundOf(t, e.around("u1", "u2", "m1", "&limit=4"))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m1", "m2", "m3"}) || got.Older || !got.Newer {
		t.Errorf("around the oldest: %v older=%v newer=%v, want [m1 m2 m3], nothing older, more newer", msgIDs(got.Messages), got.Older, got.Newer)
	}
	// Exactly enough room on a side is not "more": it is the extra row that says so.
	got = aroundOf(t, e.around("u1", "u2", "m2", "&limit=4"))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m1", "m2", "m3", "m4"}) || got.Older || !got.Newer {
		t.Errorf("around m2: %v older=%v newer=%v, want [m1 m2 m3 m4] with nothing older", msgIDs(got.Messages), got.Older, got.Newer)
	}
	// A conversation of one.
	e2 := newEnv("u1", "u2")
	e2.db.befriend("u1", "u2")
	e2.db.seed("only", "u1", "u2", "alone")
	got = aroundOf(t, e2.around("u1", "u2", "only", ""))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"only"}) || got.Older || got.Newer {
		t.Errorf("around the only message: %v older=%v newer=%v", msgIDs(got.Messages), got.Older, got.Newer)
	}
}

// A tombstone still positions the page — a deleted message is still a place in the
// conversation — and is never in it.
func TestAroundADeletedAnchorPositionsThePageAndIsNeverReturned(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 10)
	e.do("u1", http.MethodDelete, "/messages/m5", "")
	e.do("u2", http.MethodDelete, "/messages/m4", "")

	got := aroundOf(t, e.around("u1", "u2", "m5", "&limit=4"))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m2", "m3", "m6", "m7"}) || !got.Older || !got.Newer {
		t.Errorf("around a deleted message: %v older=%v newer=%v, want [m2 m3 m6 m7] with the two tombstones skipped", msgIDs(got.Messages), got.Older, got.Newer)
	}
	for _, m := range got.Messages {
		if m.DeletedAt != nil || m.ID == "m4" || m.ID == "m5" {
			t.Errorf("a tombstone %s is in the page", m.ID)
		}
	}
}

func TestAroundAndBeforeTogetherIs400AndNeitherWins(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 6)
	e.db.calls = nil

	rec := e.do("u1", http.MethodGet, "/messages/u2?around=m3&before=m5", "")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "conflicting_cursors" {
		t.Errorf("both together = %d %s, want 400 conflicting_cursors", rec.Code, rec.Body)
	}
	if n := len(e.db.callsTo(listAroundOlderSQL)) + len(e.db.callsTo(listAroundNewerSQL)) + len(e.db.callsTo(listConversationBeforeSQL)) + len(e.db.callsTo(historyCursorSQL)); n != 0 {
		t.Errorf("a refused request still read %d times", n)
	}
	// Even when the cursors are not valid ones: it is the combination that is refused.
	if rec := e.do("u1", http.MethodGet, "/messages/u2?around=nope&before=nope", ""); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "conflicting_cursors" {
		t.Errorf("both invalid = %d %s, want 400 conflicting_cursors", rec.Code, rec.Body)
	}
	// An empty one of them is not asking for it.
	if rec := e.do("u1", http.MethodGet, "/messages/u2?around=m3&before=", ""); rec.Code != http.StatusOK {
		t.Errorf("around with an empty before = %d, want 200", rec.Code)
	}
	if rec := e.do("u1", http.MethodGet, "/messages/u2?around=&before=m3", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"has_more"`) {
		t.Errorf("before with an empty around = %d %s, want the ordinary page", rec.Code, rec.Body)
	}
}

// The anchor is judged like before's: a message of this pair, or bad_cursor — and
// the friendship gate comes first, with the same opaque 404.
func TestAroundAnchorsAreScopedToThePairAndFriendGated(t *testing.T) {
	e := newEnv("u1", "u2", "u3", "u4")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	e.db.befriend("u2", "u3")
	seedThread(e, 4)
	e.db.seed("x1", "u1", "u3", "another conversation")
	e.db.seed("y1", "u2", "u3", "between two others")
	e.db.seed("z1", "u4", "u1", "from a stranger")

	for name, id := range map[string]string{
		"a message that does not exist":      "nope",
		"a message of another conversation":  "x1",
		"a message between two other people": "y1",
	} {
		rec := e.around("u1", "u2", id, "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_cursor" {
			t.Errorf("%s: %d %s, want 400 bad_cursor", name, rec.Code, rec.Body)
		}
	}
	// Not friends: the opaque 404 whatever the anchor is, even a real one of theirs.
	for _, id := range []string{"z1", "m1", "nope"} {
		rec := e.around("u1", "u4", id, "")
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
			t.Errorf("around %s with a non-friend = %d %s, want the opaque 404", id, rec.Code, rec.Body)
		}
	}
	e.db.calls = nil
	e.around("u1", "u4", "z1", "")
	if n := len(e.db.callsTo(historyCursorSQL)) + len(e.db.callsTo(listAroundOlderSQL)); n != 0 {
		t.Errorf("a refused friendship still read the anchor or the page %d times", n)
	}
	if rec := e.do("u1", http.MethodGet, "/messages/u1?around=m1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("around with yourself = %d, want 404", rec.Code)
	}
}

// The property that matters: for every anchor and every limit the page is one
// contiguous run of the live conversation, with no gap and no duplicate, containing
// the anchor when it is live, no longer than the limit, and its two flags say exactly
// whether the run stops short of either end.
func TestAroundIsAContiguousRunWithTruthfulFlags(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 12)
	e.do("u1", http.MethodDelete, "/messages/m7", "")
	e.do("u2", http.MethodDelete, "/messages/m2", "")
	// Ties on created_at, told apart by id.
	at := e.db.find("m9").CreatedAt
	e.db.find("m10").CreatedAt, e.db.find("m11").CreatedAt = at, at

	var live []string
	hist := decode[historyPage](t, e.do("u1", http.MethodGet, "/messages/u2", ""))
	live = msgIDs(hist.Messages)
	if len(live) != 10 {
		t.Fatalf("setup: %d live messages, want 10", len(live))
	}
	position := map[string]int{}
	for i, id := range live {
		position[id] = i
	}

	for anchor := 1; anchor <= 12; anchor++ {
		id := "m" + strconv.Itoa(anchor)
		for limit := 1; limit <= 14; limit++ {
			got := aroundOf(t, e.around("u1", "u2", id, "&limit="+strconv.Itoa(limit)))
			ids := msgIDs(got.Messages)
			if len(ids) == 0 || len(ids) > limit {
				t.Errorf("around %s limit %d: %d messages", id, limit, len(ids))
				continue
			}
			first, ok := position[ids[0]]
			if !ok {
				t.Errorf("around %s limit %d: %s is not a live message", id, limit, ids[0])
				continue
			}
			for i, got := range ids {
				if position[got] != first+i {
					t.Errorf("around %s limit %d: %v is not a contiguous run of %v (gap or duplicate at %d)", id, limit, ids, live, i)
					break
				}
			}
			if p, live := position[id]; live && !slices.Contains(ids, id) {
				t.Errorf("around %s limit %d: the live anchor (position %d) is missing from %v", id, limit, p, ids)
			}
			if wantOlder := first > 0; got.Older != wantOlder {
				t.Errorf("around %s limit %d: has_more_older=%v but the page starts at %d", id, limit, got.Older, first)
			}
			if wantNewer := first+len(ids) < len(live); got.Newer != wantNewer {
				t.Errorf("around %s limit %d: has_more_newer=%v but the page ends at %d of %d", id, limit, got.Newer, first+len(ids), len(live))
			}
		}
	}
}

// An edit moves a message's seq and must not move its place; the shape of the older
// responses is untouched.
func TestAroundOrdersByCreationNeverSeqAndLeavesOtherShapesAlone(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 6)
	e.do("u1", http.MethodPatch, "/messages/m1", jsonBody(t, map[string]any{"body": "edited last"}))
	reactAs(t, e, "u2", "m3", "👍")
	if e.db.find("m1").Seq <= e.db.find("m6").Seq {
		t.Fatal("setup: m1 should have the newest seq")
	}

	got := aroundOf(t, e.around("u1", "u2", "m3", ""))
	if !reflect.DeepEqual(msgIDs(got.Messages), []string{"m1", "m2", "m3", "m4", "m5", "m6"}) {
		t.Errorf("page = %v, want creation order, not seq order", msgIDs(got.Messages))
	}
	// Each is the full message, as the asker sees it.
	for _, m := range got.Messages {
		if m.ID == "m3" && !reflect.DeepEqual(m.Reactions, []reactionSummary{{Emoji: "👍", Count: 1, Me: false}}) {
			t.Errorf("m3's reactions = %+v", m.Reactions)
		}
		if m.ID == "m1" && m.EditedAt == nil {
			t.Error("m1 lost its edited_at")
		}
	}

	body := e.around("u1", "u2", "m3", "").Body.String()
	if strings.Contains(body, `"has_more":`) || !strings.Contains(body, `"has_more_older":`) || !strings.Contains(body, `"has_more_newer":`) {
		t.Errorf("around body = %s, want has_more_older and has_more_newer and no has_more", body)
	}
	before := e.do("u1", http.MethodGet, "/messages/u2?before=m5", "").Body.String()
	if !strings.Contains(before, `"has_more":`) || strings.Contains(before, "has_more_") {
		t.Errorf("before body = %s, want the unchanged {messages, has_more}", before)
	}
}

func TestAroundLimitIsClampedLikeHistory(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 3)
	for _, tc := range []struct {
		raw          string
		older, newer int // rows asked for: the place count plus the one that says "more"
	}{
		{"", 51, 51}, {"0", 51, 51}, {"-5", 51, 51}, {"abc", 51, 51}, {"7", 5, 4}, {"200", 101, 101}, {"500", 101, 101}, {"9999", 101, 101},
	} {
		e.db.calls = nil
		extra := ""
		if tc.raw != "" {
			extra = "&limit=" + tc.raw
		}
		if rec := e.around("u1", "u2", "m2", extra); rec.Code != http.StatusOK {
			t.Fatalf("limit=%q = %d", tc.raw, rec.Code)
		}
		o, n := e.db.callsTo(listAroundOlderSQL), e.db.callsTo(listAroundNewerSQL)
		if len(o) != 1 || len(n) != 1 || o[0].args[4] != tc.older || n[0].args[4] != tc.newer {
			t.Errorf("limit=%q read %v older and %v newer, want %d and %d", tc.raw, o, n, tc.older, tc.newer)
		}
	}
}

func TestAroundFailsCleanlyAndReadsOnlyTwiceWithNoWrites(t *testing.T) {
	for _, failing := range []string{historyCursorSQL, listAroundOlderSQL, listAroundNewerSQL, reactionsSQL} {
		e := newEnv("u1", "u2")
		e.db.befriend("u1", "u2")
		seedThread(e, 4)
		e.db.failOnQuery = failing
		if rec := e.around("u1", "u2", "m2", ""); rec.Code != http.StatusInternalServerError {
			t.Errorf("a failing %.30q = %d, want 500", strings.Join(strings.Fields(failing), " "), rec.Code)
		}
	}
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	seedThread(e, 4)
	seq := e.db.seq
	e.db.calls = nil
	e.around("u1", "u2", "m2", "")
	if len(e.db.callsTo(listAroundOlderSQL)) != 1 || len(e.db.callsTo(listAroundNewerSQL)) != 1 || len(e.db.callsTo(historyCursorSQL)) != 1 {
		t.Error("an around page should be one anchor read and two page reads")
	}
	for _, c := range e.db.calls {
		if c.inTx {
			t.Errorf("around ran %.40q inside a transaction", strings.Join(strings.Fields(c.sql), " "))
		}
	}
	if e.db.seq != seq || e.db.commits != 0 || len(e.ln.pushes) != 0 {
		t.Error("around wrote or pushed something")
	}
}

// ---- one grapheme cluster -----------------------------------------------------

// zwjChain is n copies of one emoji joined by ZWJ, with the emoji selector at the end:
// 2n runes, and one grapheme cluster however long it is.
func zwjChain(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "\U0001F44D"
	}
	return strings.Join(parts, string(runeZWJ)) + string(runeVS16)
}

// A reaction is exactly one cluster. Everything that is several is refused whatever
// it is made of, which is what keeps WORDS out without a list of alphabets.
func TestAReactionIsExactlyOneGraphemeCluster(t *testing.T) {
	flag := func(a, b rune) string { return string(a) + string(b) }
	const (
		j = 0x1F1EF
		p = 0x1F1F5
		u = 0x1F1FA
		s = 0x1F1F8
	)
	for name, in := range map[string]string{
		"a flag":                       flag(j, p),
		"a ZWJ chain of sixteen runes": zwjChain(8),
		"a single regional indicator, one character": string(rune(j)),
	} {
		if got, ok := cleanEmoji(in); !ok || got != in {
			t.Errorf("%s: cleanEmoji(%q) = %q, %v, want it accepted", name, in, got, ok)
		}
	}
	if n := len([]rune(zwjChain(8))); n != maxEmojiRunes {
		t.Fatalf("setup: the chain is %d runes, want exactly %d", n, maxEmojiRunes)
	}
	for name, in := range map[string]string{
		"a flag and another emoji":           flag(j, p) + "\U0001F44D",
		"an emoji between two flags":         flag(j, p) + "\U0001F44D" + flag(u, s),
		"two flags in a row":                 flag(j, p) + flag(u, s),
		"three regional indicators":          string(rune(j)) + flag(p, u),
		"five, hello in flag letters":        "\U0001F1ED\U0001F1EA\U0001F1F1\U0001F1F1\U0001F1F4",
		"four, help in flag letters":         "\U0001F1ED\U0001F1EA\U0001F1F1\U0001F1F5",
		"a lone one between two emoji":       "\U0001F44D" + string(rune(j)) + "\U0001F44D",
		"a flag whose halves are split":      string(rune(j)) + string(runeVS16) + string(rune(p)),
		"two emoji":                          "\U0001F44D\U0001F44D",
		"sixteen emoji":                      strings.Repeat("\U0001F44D", 16),
		"a ZWJ chain over the rune cap":      zwjChain(9),
		"two enclosed ideographs":            "\U00003297\U00003299",
		"a squared word":                     "\U0001F137\U0001F138",
		"a braille word":                     "\U00002813\U00002811\U00002807",
		"a circled word":                     "\U000024D7\U000024D8",
		"a parenthesized word":               "\U0001F113\U0001F114",
		"a lone combining mark":              "\U00000301",
		"a combining mark on an emoji":       "\U0001F44D\U00000301",
		"an emoji, a zero-width space":       "\U0001F44D\U0000200B",
		"an emoji, a ZWJ and a letter":       "\U0001F44D\U0000200D" + "a",
		"two clusters, the second only glue": "\U0001F44D\n\U0000FE0F",
	} {
		if got, ok := cleanEmoji(in); ok {
			t.Errorf("%s: cleanEmoji(%q) = %q accepted, want it refused", name, in, got)
		}
	}
}

// What the cluster rule takes on purpose: a single unusual symbol is one character,
// not a message body. These used to be refused by lists of ranges; none is a hole.
func TestASingleUnusualSymbolIsAcceptedOnPurpose(t *testing.T) {
	for name, in := range map[string]string{
		"an enclosed ideograph, secret":   "\U00003299",
		"an enclosed ideograph, congrats": "\U00003297",
		"an enclosed ideograph, sun":      "\U00003244",
		"a square katakana word":          "\U00003300",
		"a circled letter":                "\U000024D0",
		"a parenthesized letter":          "\U0000249C",
		"a squared letter":                "\U0001F130",
		"a negative circled letter":       "\U0001F150",
		"a braille cell":                  "\U00002813",
		"a braille blank":                 "\U00002800",
		"circled M":                       "\U000024C2",
		"A button":                        "\U0001F170",
		"B button":                        "\U0001F171",
		"O button":                        "\U0001F17E",
		"P button":                        "\U0001F17F",
		"P button, emoji selector":        "\U0001F17F\U0000FE0F",
		"AB button":                       "\U0001F18E",
		"CL button":                       "\U0001F191",
	} {
		if got, ok := cleanEmoji(in); !ok || got != in {
			t.Errorf("%s: cleanEmoji(%q) = %q, %v, want it accepted", name, in, got, ok)
		}
	}
	// The five that used to be named because a range swallowed them are now accepted
	// on their own, so naming them again would be dead weight.
	for _, r := range []rune{0x24C2, 0x1F170, 0x1F171, 0x1F17E, 0x1F17F} {
		if _, named := emojiOutsideSymbols[r]; named {
			t.Errorf("%U is accepted by the symbol class and must not be in emojiOutsideSymbols", r)
		}
	}
}

// The character clause keeps text out of a cluster that is, on its own, one character.
func TestOneCharacterOfTextIsStillRefused(t *testing.T) {
	for name, in := range map[string]string{
		"a letter":                "a",
		"an accented letter":      "\U000000E9",
		"a CJK ideograph":         "\U00006F22",
		"a kana":                  "\U00003042",
		"a digit":                 "1",
		"an Arabic-Indic digit":   "\U00000663",
		"a circled digit":         "\U00002460",
		"a space":                 " \U0000200B",
		"a no-break space":        "\U000000A0",
		"an ideographic space":    "\U00003000",
		"a full stop":             ".",
		"an ellipsis":             "\U00002026",
		"a dash":                  "\U00002014",
		"an equals sign":          "=",
		"a plus":                  "+",
		"a plus-minus sign":       "\U000000B1",
		"a summation sign":        "\U00002211",
		"a euro sign":             "\U000020AC",
		"a dollar sign":           "$",
		"a replacement character": "\U0000FFFD",
		"a control character":     "\x01",
		"NUL":                     "\x00",
		"a ZWJ":                   "\U0000200D",
		"a variation selector":    "\U0000FE0F",
		"a lone keycap":           "\U000020E3",
		"only tag characters":     "\U000E0067\U000E007F",
	} {
		if got, ok := cleanEmoji(in); ok {
			t.Errorf("%s: cleanEmoji(%q) = %q accepted, want it refused", name, in, got)
		}
	}
}

// Over HTTP, so the refusal is a 400 bad_emoji on the way in and the way out.
func TestSeveralClustersAreBadEmojiOverHTTP(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hi")
	e.db.calls = nil
	for name, in := range map[string]string{
		"circled":         "\U000024D7\U000024D8",
		"squared":         "\U0001F137\U0001F138",
		"braille":         "\U00002813\U00002811",
		"enclosed CJK":    "\U00003297\U00003299",
		"flag letters":    "\U0001F1ED\U0001F1EA\U0001F1F1\U0001F1F1\U0001F1F4",
		"two flags":       "\U0001F1EF\U0001F1F5\U0001F1FA\U0001F1F8",
		"a flag and more": "\U0001F1EF\U0001F1F5\U0001F44D",
	} {
		rec := reactAs(t, e, "u2", "m1", in)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_emoji" {
			t.Errorf("PUT %s = %d %s, want 400 bad_emoji", name, rec.Code, rec.Body)
		}
		rec = e.unreact("u2", "m1", in)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_emoji" {
			t.Errorf("DELETE %s = %d %s, want 400 bad_emoji", name, rec.Code, rec.Body)
		}
	}
	if len(e.db.calls) != 0 || len(e.db.reactions) != 0 {
		t.Error("a refused reaction reached the database")
	}
}
