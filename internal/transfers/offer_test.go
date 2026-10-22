package transfers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// ---- harness ----------------------------------------------------------------

// fakeDB answers the friendship query from a set of pairs, and can be told to
// fail, so the handler is exercised without a database.
type fakeDB struct {
	friends map[[2]string]bool
	fail    error
	asked   int
}

func pair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (d *fakeDB) befriend(a, b string) {
	if d.friends == nil {
		d.friends = map[[2]string]bool{}
	}
	d.friends[pair(a, b)] = true
}

type boolRow struct {
	v   bool
	err error
}

func (r boolRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.v
	return nil
}

func (d *fakeDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	d.asked++
	if d.fail != nil {
		return boolRow{err: d.fail}
	}
	return boolRow{v: d.friends[pair(args[0].(string), args[1].(string))]}
}

// push is one call to the Notify func.
type push struct {
	users []string
	kind  string
	data  any
}

type listener struct{ pushes []push }

func (l *listener) notify(users []string, kind string, data any) {
	l.pushes = append(l.pushes, push{users: append([]string(nil), users...), kind: kind, data: data})
}

type env struct {
	db *fakeDB
	ln *listener
	h  *Handler
}

func newEnv() *env {
	db := &fakeDB{}
	ln := &listener{}
	return &env{db: db, ln: ln, h: NewHandler(db, ln.notify)}
}

// router mounts the handler as main.go does, as the given user.
func (e *env) router(userID string) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), userID)))
		})
	})
	r.Post("/transfers/offer", e.h.Offer)
	return r
}

func offer(t *testing.T, e *env, as string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw string
	switch b := body.(type) {
	case string:
		raw = b
	default:
		out, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(out)
	}
	req := httptest.NewRequest(http.MethodPost, "/transfers/offer", strings.NewReader(raw))
	rec := httptest.NewRecorder()
	e.router(as).ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", rec.Body.String(), err)
	}
	return body.Error
}

const room = "01JABCDEFGHJKMNPQRSTVWXYZ0"

func good(to string) map[string]any {
	return map[string]any{"to": to, "room": room, "name": "holiday.mp4", "size": 1_500_000_000}
}

// ---- the offer ---------------------------------------------------------------

func TestOfferTellsTheFriendAndNobodyElseAndStoresNothing(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	rec := offer(t, e, "alice", good("bob"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a 204 carried a body: %q", rec.Body.String())
	}
	if len(e.ln.pushes) != 1 {
		t.Fatalf("pushes = %d, want exactly 1: %+v", len(e.ln.pushes), e.ln.pushes)
	}
	p := e.ln.pushes[0]
	if p.kind != "transfer" {
		t.Errorf("kind = %q, want transfer", p.kind)
	}
	if len(p.users) != 1 || p.users[0] != "bob" {
		t.Errorf("sent to %v, want only bob", p.users)
	}
	raw, _ := json.Marshal(p.data)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"from": "alice", "room": room, "name": "holiday.mp4", "size": float64(1_500_000_000)}
	if len(got) != len(want) {
		t.Errorf("frame = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("frame[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// from is the caller, whatever the body says: an offer cannot claim to be from
// somebody else.
func TestOfferIgnoresAFromInTheBody(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")
	body := good("bob")
	body["from"] = "carol"

	if rec := offer(t, e, "alice", body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(e.ln.pushes[0].data)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["from"] != "alice" {
		t.Errorf("from = %v, want alice", got["from"])
	}
}

func TestOfferIsFriendGatedWithOneOpaqueAnswer(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	for name, to := range map[string]string{
		"a stranger":        "carol",
		"nobody at all":     "no-such-user",
		"yourself":          "alice",
		"an empty id":       "",
		"only whitespace":   "   ",
		"a friend of bob's": "dave",
	} {
		rec := offer(t, e, "alice", good(to))
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "recipient_not_found" {
			t.Errorf("%s: got %d %q, want 404 recipient_not_found", name, rec.Code, rec.Body.String())
		}
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a refused offer pushed %+v", e.ln.pushes)
	}
}

// A database that cannot answer is the same 404 as no, so the handler never
// tells a prober that the failure was on its side and not the answer's.
func TestOfferWhenTheFriendCheckFailsIsTheSameOpaque404(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")
	e.db.fail = errors.New("connection refused")

	rec := offer(t, e, "alice", good("bob"))
	if rec.Code != http.StatusNotFound || errCode(t, rec) != "recipient_not_found" {
		t.Fatalf("got %d %q, want 404 recipient_not_found", rec.Code, rec.Body.String())
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("pushed despite not knowing: %+v", e.ln.pushes)
	}
}

func TestOfferWithNoListenerIsStillA204(t *testing.T) {
	db := &fakeDB{}
	db.befriend("alice", "bob")
	h := NewHandler(db, nil)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), "alice")))
		})
	})
	r.Post("/transfers/offer", h.Offer)

	raw, _ := json.Marshal(good("bob"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/transfers/offer", strings.NewReader(string(raw))))
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestOfferWithoutASessionIs401(t *testing.T) {
	e := newEnv()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/transfers/offer", strings.NewReader("{}"))
	http.HandlerFunc(e.h.Offer).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// ---- validation --------------------------------------------------------------

func TestOfferRefusesABadNameBeforeAskingAnyoneAnything(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	for _, name := range []string{
		"",
		"   ",
		"../secret.txt",
		"..",
		".",
		"a..b.txt",
		"..hidden",
		"dir/file.txt",
		`dir\file.txt`,
		"/etc/passwd",
		`C:\Windows\system32`,
		"file.txt:stream",
		"nul\x00.txt",
		"new\nline.txt",
		"tab\tname.txt",
		"star*.txt",
		"what?.txt",
		`quote".txt`,
		"pipe|name.txt",
		"less<than.txt",
		"trailing.",
		"trailing ",
		"bad\xffutf8.txt",
		strings.Repeat("a", 256),
	} {
		body := good("bob")
		body["name"] = name
		rec := offer(t, e, "alice", body)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "bad_name" {
			t.Errorf("name %q: got %d %q, want 400 bad_name", name, rec.Code, rec.Body.String())
		}
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a refused offer pushed %+v", e.ln.pushes)
	}
	if e.db.asked != 0 {
		t.Errorf("friendship was looked up %d time(s) for a request that was never going to be sent", e.db.asked)
	}
}

func TestOfferAcceptsOrdinaryFilenames(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	for _, name := range []string{
		"holiday.mp4",
		"Report (final) v2.pdf",
		"no extension",
		".gitignore",
		"日本語のファイル.txt",
		"emoji 🎬.mov",
		"a.b.c.tar.gz",
		strings.Repeat("a", 255),
	} {
		body := good("bob")
		body["name"] = name
		if rec := offer(t, e, "alice", body); rec.Code != http.StatusNoContent {
			t.Errorf("name %q: got %d %q, want 204", name, rec.Code, rec.Body.String())
		}
	}
}

func TestOfferRefusesABadSize(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	for name, size := range map[string]any{
		"zero":            0,
		"negative":        -5,
		"past JS integer": int64(1)<<53 + 1,
		"a fraction":      1.5,
		"a string":        "12",
		"null":            nil,
	} {
		body := good("bob")
		body["size"] = size
		rec := offer(t, e, "alice", body)
		code := errCode(t, rec)
		wantCode := "bad_size"
		if name == "a fraction" || name == "a string" {
			wantCode = "invalid_body"
		}
		// null decodes to the zero value, which is the same refusal as zero.
		if rec.Code != http.StatusBadRequest || code != wantCode {
			t.Errorf("%s: got %d %q, want 400 %s", name, rec.Code, code, wantCode)
		}
	}

	body := good("bob")
	body["size"] = int64(1)<<53 - 1
	if rec := offer(t, e, "alice", body); rec.Code != http.StatusNoContent {
		t.Errorf("largest exact size: got %d, want 204", rec.Code)
	}
	body["size"] = 1
	if rec := offer(t, e, "alice", body); rec.Code != http.StatusNoContent {
		t.Errorf("one byte: got %d, want 204", rec.Code)
	}
}

func TestOfferRefusesABadRoom(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	for _, r := range []string{"", "  ", "has space", "bombers://room/abc", "../x", "a/b", strings.Repeat("a", 65)} {
		body := good("bob")
		body["room"] = r
		rec := offer(t, e, "alice", body)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "bad_room" {
			t.Errorf("room %q: got %d %q, want 400 bad_room", r, rec.Code, rec.Body.String())
		}
	}
}

func TestOfferRefusesAMalformedBody(t *testing.T) {
	e := newEnv()
	e.db.befriend("alice", "bob")

	for _, b := range []string{"", "not json", "[]", `{"to":`, `"just a string"`} {
		rec := offer(t, e, "alice", b)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_body" {
			t.Errorf("body %q: got %d %q, want 400 invalid_body", b, rec.Code, rec.Body.String())
		}
	}

	big := `{"to":"bob","room":"` + room + `","name":"` + strings.Repeat("a", 5000) + `","size":1}`
	if rec := offer(t, e, "alice", big); rec.Code != http.StatusBadRequest {
		t.Errorf("an oversized body: got %d, want 400", rec.Code)
	}
}
