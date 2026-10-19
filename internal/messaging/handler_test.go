package messaging

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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

// frame returns the one push this test expects, as the JSON a client would read.
func (l *listener) frame(t *testing.T) (push, map[string]any) {
	t.Helper()
	if len(l.pushes) != 1 {
		t.Fatalf("pushes = %d, want exactly 1: %+v", len(l.pushes), l.pushes)
	}
	p := l.pushes[0]
	raw, err := json.Marshal(p.data)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return p, out
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
	wantKeys := []string{"body", "created_at", "deleted_at", "edited_at", "id", "nonce", "recipient_id", "sender_id", "seq"}
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
	if len(inserts) != 1 || len(inserts[0].args) != 5 || inserts[0].args[4] != "n-1" {
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
	if len(e.ln.pushes) != 2 {
		t.Errorf("pushes = %d, want 2", len(e.ln.pushes))
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
	if len(e.db.msgs) != 2 || len(e.ln.pushes) != 2 {
		t.Errorf("rows = %d, pushes = %d, want 2 and 2", len(e.db.msgs), len(e.ln.pushes))
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
			if e.db.commits != 1 || len(e.ln.pushes) != 1 {
				t.Errorf("commits=%d pushes=%d, want 1 and 1", e.db.commits, len(e.ln.pushes))
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
	want := []string{"body", "created_at", "deleted_at", "edited_at", "id", "recipient_id", "sender_id", "seq"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("message keys = %v, want %v", keys, want)
	}
	if got["edited_at"] != nil || got["deleted_at"] != nil {
		t.Errorf("unset timestamps must be null: %v", got)
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

// The reason this is one statement: the number of round trips must not grow with
// the number of friends.
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
		if len(e.db.calls) != 1 || e.db.calls[0].sql != listSummariesSQL {
			t.Errorf("%d friends took %d statements, want exactly the one list query", friends, len(e.db.calls))
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

	p, frame := e.ln.frame(t)
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
	p, frame := e.ln.frame(t)
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
